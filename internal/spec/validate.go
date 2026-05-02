package spec

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
)

// dns1123Label matches a DNS-1123 label: lower-case alnum and hyphens, must
// start and end with alphanumeric, max 63 chars. Suitable for our names
// (hubs, agents, groups) and roughly equivalent to Kubernetes object names.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Validate enforces structural and cross-reference invariants of a Document.
// All discovered problems are joined into a single error so the operator can
// fix them in one editing pass.
func (d *Document) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	// Envelope
	if d.APIVersion != APIVersion {
		add("apiVersion: must be %q, got %q", APIVersion, d.APIVersion)
	}
	if d.Kind != Kind {
		add("kind: must be %q, got %q", Kind, d.Kind)
	}
	if !validName(d.Metadata.Name) {
		add("metadata.name: %q is not a valid DNS-1123 label", d.Metadata.Name)
	}

	// Overlay subnet
	overlay, err := netip.ParsePrefix(d.Spec.CIDR)
	if err != nil {
		add("spec.cidr: %q is not a valid CIDR: %v", d.Spec.CIDR, err)
	} else if !overlay.IsValid() {
		add("spec.cidr: %q is invalid", d.Spec.CIDR)
	}

	// WireGuard
	if mtu := d.Spec.WireGuard.MTU; mtu < 1280 || mtu > 1500 {
		add("spec.wireguard.mtu: %d outside [1280..1500]", mtu)
	}
	if port := d.Spec.WireGuard.HubListenPort; port < 1 || port > 65535 {
		add("spec.wireguard.hubListenPort: %d not in [1..65535]", port)
	}

	// Groups (declarations) — collected first because agents and ACLs
	// reference them.
	groups := map[string]struct{}{}
	for i, g := range d.Spec.Groups {
		if !validName(g) {
			add("spec.groups[%d]: %q is not a valid DNS-1123 label", i, g)
			continue
		}
		if _, dup := groups[g]; dup {
			add("spec.groups[%d]: duplicate group %q", i, g)
			continue
		}
		groups[g] = struct{}{}
	}

	// Names must be globally unique across hubs and agents because
	// agents reference hubs by name and we want unambiguous identity.
	names := map[string]string{} // name -> "hub" or "agent"
	tunnelIPs := map[netip.Addr]string{}

	checkAddrInOverlay := func(path, raw string) (netip.Addr, bool) {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			add("%s: %q is not a valid IP: %v", path, raw, err)
			return netip.Addr{}, false
		}
		if overlay.IsValid() && !overlay.Contains(addr) {
			add("%s: %s is outside overlay %s", path, addr, overlay)
		}
		if other, dup := tunnelIPs[addr]; dup {
			add("%s: tunnelIP %s is already used by %q", path, addr, other)
		} else {
			tunnelIPs[addr] = raw
		}
		return addr, true
	}

	// Hubs
	hubNames := map[string]struct{}{}
	for i, h := range d.Spec.Hubs {
		path := fmt.Sprintf("spec.hubs[%d]", i)
		if !validName(h.Name) {
			add("%s.name: %q is not a valid DNS-1123 label", path, h.Name)
		} else {
			if other, dup := names[h.Name]; dup {
				add("%s.name: %q already used as %s", path, h.Name, other)
			} else {
				names[h.Name] = "hub"
				hubNames[h.Name] = struct{}{}
			}
		}
		if h.Endpoint == "" {
			add("%s.endpoint: required", path)
		} else if err := validateEndpoint(h.Endpoint); err != nil {
			add("%s.endpoint: %v", path, err)
		}
		checkAddrInOverlay(path+".tunnelIP", h.TunnelIP)
	}

	// Agents
	for i, a := range d.Spec.Agents {
		path := fmt.Sprintf("spec.agents[%d]", i)
		if !validName(a.Name) {
			add("%s.name: %q is not a valid DNS-1123 label", path, a.Name)
		} else if other, dup := names[a.Name]; dup {
			add("%s.name: %q already used as %s", path, a.Name, other)
		} else {
			names[a.Name] = "agent"
		}
		checkAddrInOverlay(path+".tunnelIP", a.TunnelIP)
		if a.Hub == "" {
			add("%s.hub: required", path)
		} else if _, ok := hubNames[a.Hub]; !ok {
			add("%s.hub: %q does not match any declared hub", path, a.Hub)
		}
		seenG := map[string]struct{}{}
		for j, g := range a.Groups {
			gp := fmt.Sprintf("%s.groups[%d]", path, j)
			if _, ok := groups[g]; !ok {
				add("%s: %q is not declared in spec.groups", gp, g)
			}
			if _, dup := seenG[g]; dup {
				add("%s: duplicate group %q", gp, g)
			}
			seenG[g] = struct{}{}
		}
	}

	// ACLs
	for i, r := range d.Spec.ACLs {
		path := fmt.Sprintf("spec.acls[%d]", i)
		if r.Action != ActionAllow && r.Action != ActionDeny {
			add("%s.action: must be %q or %q, got %q",
				path, ActionAllow, ActionDeny, r.Action)
		}
		if len(r.From) == 0 {
			add("%s.from: must list at least one group", path)
		}
		if len(r.To) == 0 {
			add("%s.to: must list at least one group", path)
		}
		for j, g := range r.From {
			if _, ok := groups[g]; !ok {
				add("%s.from[%d]: %q is not declared in spec.groups", path, j, g)
			}
		}
		for j, g := range r.To {
			if _, ok := groups[g]; !ok {
				add("%s.to[%d]: %q is not declared in spec.groups", path, j, g)
			}
		}
	}

	if len(d.Spec.Hubs) == 0 {
		add("spec.hubs: at least one hub is required")
	}

	return errors.Join(errs...)
}

func validName(s string) bool {
	return dns1123Label.MatchString(s)
}

// validateEndpoint requires "host:port" with port 1..65535. Host may be a
// DNS name or literal IP. We do not resolve DNS at validation time so that
// specs are reviewable offline.
func validateEndpoint(ep string) error {
	host, port, err := net.SplitHostPort(ep)
	if err != nil {
		return fmt.Errorf("not a host:port pair: %w", err)
	}
	if host == "" {
		return errors.New("host is empty")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("port %q not in [1..65535]", port)
	}
	return nil
}
