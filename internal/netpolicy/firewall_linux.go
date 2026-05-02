//go:build linux

package netpolicy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/coreos/go-iptables/iptables"

	"github.com/ilyapiatykh/seine/internal/logging"
)

// firewallChain is the custom chain seine populates with ACL rules. The
// FORWARD chain is left mostly alone — we only insert one jump rule that
// hands seine-to-seine traffic over to this chain.
const firewallChain = "SEINE-FWD"

// Firewall is the iptables-backed implementation of an ACL applier. A
// single instance is created per hub and reused for the lifetime of the
// agent.
type Firewall struct {
	ipt   *iptables.IPTables
	iface string
}

// NewFirewall opens the iptables socket. Failures here usually mean the
// process is unprivileged (CAP_NET_ADMIN missing) — the error message
// surfaces that hint.
func NewFirewall(ifaceName string) (*Firewall, error) {
	ipt, err := iptables.New(iptables.IPFamily(iptables.ProtocolIPv4))
	if err != nil {
		return nil, fmt.Errorf("netpolicy: iptables init (need CAP_NET_ADMIN): %w", err)
	}
	return &Firewall{ipt: ipt, iface: ifaceName}, nil
}

// Reconcile installs (or refreshes) the ACL rules described by c. The
// implementation follows the standard "ensure custom chain → flush →
// populate" pattern so the live ruleset always matches the spec.
//
// Concretely the resulting iptables looks like:
//
//	-N SEINE-FWD
//	-I FORWARD 1 -i seine0 -o seine0 -j SEINE-FWD   # idempotent
//	-A SEINE-FWD -s <hub-ip>  -j ACCEPT             # hubs always reachable
//	-A SEINE-FWD -d <hub-ip>  -j ACCEPT
//	-A SEINE-FWD -s <src> -d <dst> -j ACCEPT        # one per allow rule
//	-A SEINE-FWD -s <src> -d <dst> -j DROP          # one per explicit deny
//	-A SEINE-FWD -s <overlay> -d <overlay> -j DROP  # default deny
//	-A SEINE-FWD -j RETURN                          # safety net
func (f *Firewall) Reconcile(ctx context.Context, c *Compiled) error {
	log := logging.FromContext(ctx).With(slog.String("component", "netpolicy"))

	if err := f.ensureChain(); err != nil {
		return err
	}
	if err := f.ensureForwardJump(); err != nil {
		return err
	}
	if err := f.ipt.ClearChain("filter", firewallChain); err != nil {
		return fmt.Errorf("netpolicy: clear chain: %w", err)
	}

	rules := buildRules(c)
	for _, r := range rules {
		if err := f.ipt.Append("filter", firewallChain, r...); err != nil {
			return fmt.Errorf("netpolicy: append %v: %w", r, err)
		}
	}

	log.Info("acl reconciled",
		slog.Int("allow_rules", len(c.Allows)),
		slog.Int("deny_rules", len(c.Denies)),
		slog.Int("hub_count", len(c.HubIPs)),
	)
	return nil
}

// Teardown removes the FORWARD jump and deletes the seine chain. Best-
// effort: errors are logged but not returned, so a noisy shutdown does
// not mask the agent's primary exit code.
func (f *Firewall) Teardown(ctx context.Context) {
	log := logging.FromContext(ctx).With(slog.String("component", "netpolicy"))
	jump := f.forwardJump()
	if exists, _ := f.ipt.Exists("filter", "FORWARD", jump...); exists {
		if err := f.ipt.Delete("filter", "FORWARD", jump...); err != nil {
			log.Warn("remove FORWARD jump", slog.String("err", err.Error()))
		}
	}
	if exists, _ := f.ipt.ChainExists("filter", firewallChain); exists {
		if err := f.ipt.ClearChain("filter", firewallChain); err != nil {
			log.Warn("flush SEINE-FWD", slog.String("err", err.Error()))
		}
		if err := f.ipt.DeleteChain("filter", firewallChain); err != nil {
			log.Warn("delete SEINE-FWD", slog.String("err", err.Error()))
		}
	}
}

func (f *Firewall) ensureChain() error {
	exists, err := f.ipt.ChainExists("filter", firewallChain)
	if err != nil {
		return fmt.Errorf("netpolicy: chain exists query: %w", err)
	}
	if exists {
		return nil
	}
	if err := f.ipt.NewChain("filter", firewallChain); err != nil {
		return fmt.Errorf("netpolicy: create chain %s: %w", firewallChain, err)
	}
	return nil
}

func (f *Firewall) forwardJump() []string {
	return []string{"-i", f.iface, "-o", f.iface, "-j", firewallChain}
}

func (f *Firewall) ensureForwardJump() error {
	jump := f.forwardJump()
	exists, err := f.ipt.Exists("filter", "FORWARD", jump...)
	if err != nil {
		return fmt.Errorf("netpolicy: forward jump query: %w", err)
	}
	if exists {
		return nil
	}
	if err := f.ipt.Insert("filter", "FORWARD", 1, jump...); err != nil {
		return fmt.Errorf("netpolicy: insert forward jump: %w", err)
	}
	return nil
}

// buildRules turns a Compiled into the ordered list of iptables rule
// arg-sets. The order matters: hub allow-alls and explicit denies come
// before the catch-all default deny.
func buildRules(c *Compiled) [][]string {
	var rules [][]string

	// Hub allow-alls (both directions). Hubs route traffic; if we
	// dropped them they would also drop our diagnostics.
	for _, h := range c.HubIPs {
		rules = append(rules, []string{"-s", h.String(), "-j", "ACCEPT"})
		rules = append(rules, []string{"-d", h.String(), "-j", "ACCEPT"})
	}

	// Explicit denies before allows, so a deny rule wins over an allow
	// at the same source/destination.
	for _, r := range c.Denies {
		rules = append(rules, []string{"-s", r.Source.String(), "-d", r.Dest.String(), "-j", "DROP"})
	}
	for _, r := range c.Allows {
		rules = append(rules, []string{"-s", r.Source.String(), "-d", r.Dest.String(), "-j", "ACCEPT"})
	}

	// Default-deny inside the overlay; let everything else fall
	// through (which would only happen if a non-overlay packet reached
	// the chain, which our FORWARD filter prevents anyway).
	rules = append(rules,
		[]string{"-s", c.Overlay.String(), "-d", c.Overlay.String(), "-j", "DROP"},
		[]string{"-j", "RETURN"},
	)
	return rules
}
