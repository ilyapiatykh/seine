package netpolicy_test

import (
	"net/netip"
	"sort"
	"testing"

	"github.com/ilyapiatykh/seine/internal/netpolicy"
	"github.com/ilyapiatykh/seine/internal/spec"
)

func compile(t *testing.T, d *spec.Document) *netpolicy.Compiled {
	t.Helper()
	d.ApplyDefaults()
	if err := d.Validate(); err != nil {
		t.Fatalf("invalid spec: %v", err)
	}
	c, err := netpolicy.Compile(d)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return c
}

func base() *spec.Document {
	return &spec.Document{
		APIVersion: spec.APIVersion,
		Kind:       spec.Kind,
		Metadata:   spec.Metadata{Name: "n"},
		Spec: spec.Network{
			CIDR: "100.64.0.0/10",
			Hubs: []spec.Hub{{Name: "hub", Endpoint: "1.2.3.4:51820", TunnelIP: "100.64.0.1"}},
			Agents: []spec.Agent{
				{Name: "dev1", Hub: "hub", TunnelIP: "100.64.1.1", Groups: []string{"dev"}},
				{Name: "dev2", Hub: "hub", TunnelIP: "100.64.1.2", Groups: []string{"dev"}},
				{Name: "infra1", Hub: "hub", TunnelIP: "100.64.2.1", Groups: []string{"infra"}},
				{Name: "prod1", Hub: "hub", TunnelIP: "100.64.3.1", Groups: []string{"prod"}},
			},
			Groups: []string{"dev", "infra", "prod"},
			ACLs: []spec.ACL{
				{From: []string{"dev"}, To: []string{"dev", "infra"}, Action: spec.ActionAllow},
				{From: []string{"infra"}, To: []string{"infra"}, Action: spec.ActionAllow},
			},
		},
	}
}

func TestCompile_ExpandsGroupsToPairs(t *testing.T) {
	c := compile(t, base())

	// dev → dev: (dev1↔dev2), 2 rules (each direction).
	// dev → infra: (dev1→infra1, dev2→infra1), 2 rules.
	// infra → infra: 0 rules (only infra1; self-loops skipped).
	// Expected total allows: 4.
	if got := len(c.Allows); got != 4 {
		t.Errorf("allows = %d, want 4: %+v", got, c.Allows)
	}
	if len(c.Denies) != 0 {
		t.Errorf("denies = %d, want 0", len(c.Denies))
	}
	if c.Overlay.String() != "100.64.0.0/10" {
		t.Errorf("overlay = %s", c.Overlay)
	}
	if len(c.HubIPs) != 1 || c.HubIPs[0].String() != "100.64.0.1" {
		t.Errorf("HubIPs = %v", c.HubIPs)
	}
}

func TestCompile_SkipsSelfLoops(t *testing.T) {
	d := base()
	d.Spec.ACLs = []spec.ACL{
		{From: []string{"dev"}, To: []string{"dev"}, Action: spec.ActionAllow},
	}
	c := compile(t, d)
	for _, r := range c.Allows {
		if r.Source == r.Dest {
			t.Errorf("self-loop in compiled rules: %v", r)
		}
	}
}

func TestCompile_DeduplicatesAcrossOverlappingRules(t *testing.T) {
	d := base()
	// Two rules that overlap on (dev1→dev2): one via the dev→dev rule
	// and one explicit.
	d.Spec.ACLs = []spec.ACL{
		{From: []string{"dev"}, To: []string{"dev"}, Action: spec.ActionAllow},
		{From: []string{"dev"}, To: []string{"dev"}, Action: spec.ActionAllow},
	}
	c := compile(t, d)
	// dev1→dev2 and dev2→dev1: 2 unique pairs even with duplicate rules.
	if got := len(c.Allows); got != 2 {
		t.Errorf("allows = %d, want 2 (deduplicated)", got)
	}
}

func TestCompile_DeniesAreSeparate(t *testing.T) {
	d := base()
	d.Spec.ACLs = append(d.Spec.ACLs,
		spec.ACL{From: []string{"dev"}, To: []string{"prod"}, Action: spec.ActionDeny},
	)
	c := compile(t, d)
	// Denies: (dev1→prod1, dev2→prod1) = 2 rules.
	if got := len(c.Denies); got != 2 {
		t.Errorf("denies = %d, want 2", got)
	}
}

func TestCompile_StableOrder(t *testing.T) {
	c := compile(t, base())
	sorted := make([]netpolicy.Rule, len(c.Allows))
	copy(sorted, c.Allows)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Source != sorted[j].Source {
			return sorted[i].Source.Less(sorted[j].Source)
		}
		return sorted[i].Dest.Less(sorted[j].Dest)
	})
	for i := range sorted {
		if sorted[i] != c.Allows[i] {
			t.Fatalf("Allows not sorted at %d: %v vs %v", i, sorted[i], c.Allows[i])
		}
	}
}

// Sanity check that overlay is a valid prefix (not just a copy of the
// raw string).
func TestCompile_OverlayIsParsed(t *testing.T) {
	c := compile(t, base())
	addr := netip.MustParseAddr("100.64.1.1")
	if !c.Overlay.Contains(addr) {
		t.Errorf("Overlay does not contain known agent IP: %v", c.Overlay)
	}
}
