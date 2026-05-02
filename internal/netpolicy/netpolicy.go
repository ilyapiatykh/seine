// Package netpolicy enforces hub-side network policies on Linux: IP
// forwarding (so the kernel routes between WireGuard peers on the same
// interface) and an ACL implemented as a curated iptables chain.
//
// The package exposes two small surfaces:
//
//   - EnsureIPForwarding — flips net.ipv4.ip_forward to 1 when running as
//     a hub.
//
//   - ACL — reconciles a set of allow/deny rules computed from the spec
//     against the kernel firewall.
//
// On non-Linux builds the package compiles but every operation returns an
// "unsupported" error so the agent fails fast with a clear message.
package netpolicy

import (
	"fmt"
	"net/netip"
	"sort"

	"github.com/ilyapiatykh/seine/internal/spec"
)

// Rule is a flat allow/deny decision between two tunnel IPs. ACL.Compile
// expands the spec's group-based rules into this form.
type Rule struct {
	Source netip.Addr
	Dest   netip.Addr
	Allow  bool
}

// String renders a rule as "src→dst (allow|deny)" for diagnostics.
func (r Rule) String() string {
	verb := "deny"
	if r.Allow {
		verb = "allow"
	}
	return fmt.Sprintf("%s→%s %s", r.Source, r.Dest, verb)
}

// Compiled holds the result of expanding a spec into firewall rules.
type Compiled struct {
	// Allows are the (src,dst) pairs explicitly permitted by the spec.
	Allows []Rule

	// Denies are the (src,dst) pairs explicitly denied.
	Denies []Rule

	// HubIPs are the tunnel IPs of all hubs in the network. They are
	// always allowed as both source and destination so diagnostics
	// (ping a hub) keep working regardless of ACL policy.
	HubIPs []netip.Addr

	// Overlay is the network-wide CIDR. It scopes the default-deny
	// rule to overlay traffic only.
	Overlay netip.Prefix
}

// Compile expands a Document's group-based ACL into per-IP-pair rules.
//
// Rules are computed with the convention: default policy is deny; allow
// rules whitelist flows; deny rules override allows for the same pair.
// Hubs are always allowed as endpoints (they are transit infrastructure
// and need to be diagnosable). Self-loops (src == dst) are skipped — the
// kernel does not see them anyway.
func Compile(doc *spec.Document) (*Compiled, error) {
	if doc == nil {
		return nil, fmt.Errorf("netpolicy: nil document")
	}
	overlay, err := netip.ParsePrefix(doc.Spec.CIDR)
	if err != nil {
		return nil, fmt.Errorf("netpolicy: parse overlay CIDR: %w", err)
	}

	// Build group → []tunnelIP map for agents.
	groupMembers := map[string][]netip.Addr{}
	for _, a := range doc.Spec.Agents {
		ip, err := netip.ParseAddr(a.TunnelIP)
		if err != nil {
			return nil, fmt.Errorf("netpolicy: agent %s tunnel IP: %w", a.Name, err)
		}
		for _, g := range a.Groups {
			groupMembers[g] = append(groupMembers[g], ip)
		}
	}

	// Expand each ACL rule to the cartesian product of (from, to)
	// member IPs. We deduplicate per (src,dst) to avoid emitting the
	// same iptables line twice when groups overlap.
	type pair struct{ s, d netip.Addr }
	seenAllow := map[pair]struct{}{}
	seenDeny := map[pair]struct{}{}
	var allows, denies []Rule

	for _, r := range doc.Spec.ACLs {
		isAllow := r.Action == spec.ActionAllow
		for _, fromG := range r.From {
			for _, toG := range r.To {
				for _, src := range groupMembers[fromG] {
					for _, dst := range groupMembers[toG] {
						if src == dst {
							continue
						}
						k := pair{src, dst}
						if isAllow {
							if _, dup := seenAllow[k]; dup {
								continue
							}
							seenAllow[k] = struct{}{}
							allows = append(allows, Rule{Source: src, Dest: dst, Allow: true})
						} else {
							if _, dup := seenDeny[k]; dup {
								continue
							}
							seenDeny[k] = struct{}{}
							denies = append(denies, Rule{Source: src, Dest: dst, Allow: false})
						}
					}
				}
			}
		}
	}

	// Stable order so callers (and reconciliation) get deterministic
	// rule sequences across runs.
	sortRules(allows)
	sortRules(denies)

	hubs := make([]netip.Addr, 0, len(doc.Spec.Hubs))
	for _, h := range doc.Spec.Hubs {
		ip, err := netip.ParseAddr(h.TunnelIP)
		if err != nil {
			return nil, fmt.Errorf("netpolicy: hub %s tunnel IP: %w", h.Name, err)
		}
		hubs = append(hubs, ip)
	}
	sort.Slice(hubs, func(i, j int) bool { return hubs[i].Less(hubs[j]) })

	return &Compiled{
		Allows:  allows,
		Denies:  denies,
		HubIPs:  hubs,
		Overlay: overlay,
	}, nil
}

func sortRules(rs []Rule) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Source != rs[j].Source {
			return rs[i].Source.Less(rs[j].Source)
		}
		return rs[i].Dest.Less(rs[j].Dest)
	})
}
