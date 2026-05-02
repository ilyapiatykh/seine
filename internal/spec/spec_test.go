package spec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ilyapiatykh/seine/internal/spec"
)

// loadExample reads examples/network.yaml relative to the repository root.
// We resolve via the package's known location (internal/spec → ../../examples).
func loadExample(t *testing.T) []byte {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "examples", "network.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	return data
}

func TestParseExample(t *testing.T) {
	doc, err := spec.Parse(loadExample(t))
	if err != nil {
		t.Fatalf("parse example: %v", err)
	}
	if got, want := doc.APIVersion, spec.APIVersion; got != want {
		t.Errorf("apiVersion = %q, want %q", got, want)
	}
	if doc.Metadata.Name != "corp-overlay" {
		t.Errorf("metadata.name = %q", doc.Metadata.Name)
	}
	if got, want := doc.Spec.WireGuard.MTU, 1420; got != want {
		t.Errorf("mtu = %d, want %d", got, want)
	}
	if got, want := doc.Spec.WireGuard.PersistentKeepalive.Std(), 25*time.Second; got != want {
		t.Errorf("persistentKeepalive = %s, want %s", got, want)
	}
	if doc.FindHub("hub-eu") == nil {
		t.Error("hub-eu missing after parse")
	}
	if doc.FindAgent("dev-laptop") == nil {
		t.Error("dev-laptop missing after parse")
	}
	if got := doc.AgentsForHub("hub-eu"); len(got) != 2 {
		t.Errorf("AgentsForHub(hub-eu) returned %d, want 2", len(got))
	}
}

func TestApplyDefaultsIdempotent(t *testing.T) {
	doc := &spec.Document{
		Metadata: spec.Metadata{Name: "n"},
		Spec: spec.Network{
			CIDR: "100.64.0.0/10",
			Hubs: []spec.Hub{{Name: "h", Endpoint: "h:1", TunnelIP: "100.64.0.1"}},
		},
	}
	doc.ApplyDefaults()
	first := doc.Spec.WireGuard
	doc.ApplyDefaults()
	second := doc.Spec.WireGuard
	if first != second {
		t.Errorf("not idempotent: %+v vs %+v", first, second)
	}
	if first.MTU != spec.DefaultMTU || first.HubListenPort != spec.DefaultHubListenPort {
		t.Errorf("defaults not applied: %+v", first)
	}
}

func TestRoundTrip(t *testing.T) {
	src := loadExample(t)
	doc, err := spec.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := spec.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	doc2, err := spec.Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	// Re-emit should be idempotent at the second pass.
	out2, err := spec.Marshal(doc2)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(out) != string(out2) {
		t.Errorf("round-trip diverges:\nfirst:\n%s\nsecond:\n%s", out, out2)
	}
}

func TestValidateRejects(t *testing.T) {
	// Each case is a partial mutation applied to a known-good document.
	// We assert that Validate returns an error mentioning the expected
	// JSON-style path so operators can find the offending field.
	good := &spec.Document{
		APIVersion: spec.APIVersion,
		Kind:       spec.Kind,
		Metadata:   spec.Metadata{Name: "net"},
		Spec: spec.Network{
			CIDR: "100.64.0.0/10",
			Hubs: []spec.Hub{{Name: "h1", Endpoint: "h1.example.com:51820", TunnelIP: "100.64.0.1"}},
			Agents: []spec.Agent{
				{Name: "a1", Hub: "h1", TunnelIP: "100.64.1.1", Groups: []string{"g1"}},
			},
			Groups: []string{"g1"},
			ACLs:   []spec.ACL{{From: []string{"g1"}, To: []string{"g1"}, Action: spec.ActionAllow}},
		},
	}

	cases := []struct {
		name   string
		mutate func(*spec.Document)
		want   string
	}{
		{
			name:   "invalid apiVersion",
			mutate: func(d *spec.Document) { d.APIVersion = "wrong" },
			want:   "apiVersion",
		},
		{
			name:   "metadata name not DNS-1123",
			mutate: func(d *spec.Document) { d.Metadata.Name = "Bad Name" },
			want:   "metadata.name",
		},
		{
			name:   "invalid cidr",
			mutate: func(d *spec.Document) { d.Spec.CIDR = "not-a-cidr" },
			want:   "spec.cidr",
		},
		{
			name:   "tunnelIP outside overlay",
			mutate: func(d *spec.Document) { d.Spec.Hubs[0].TunnelIP = "10.0.0.1" },
			want:   "outside overlay",
		},
		{
			name: "duplicate tunnelIP",
			mutate: func(d *spec.Document) {
				d.Spec.Agents[0].TunnelIP = d.Spec.Hubs[0].TunnelIP
			},
			want: "already used",
		},
		{
			name:   "agent referencing missing hub",
			mutate: func(d *spec.Document) { d.Spec.Agents[0].Hub = "nope" },
			want:   "does not match any declared hub",
		},
		{
			name:   "agent in undeclared group",
			mutate: func(d *spec.Document) { d.Spec.Agents[0].Groups = []string{"ghost"} },
			want:   "not declared in spec.groups",
		},
		{
			name:   "ACL refers to undeclared group",
			mutate: func(d *spec.Document) { d.Spec.ACLs[0].From = []string{"ghost"} },
			want:   "not declared in spec.groups",
		},
		{
			name: "duplicate names across hub and agent",
			mutate: func(d *spec.Document) {
				d.Spec.Agents[0].Name = d.Spec.Hubs[0].Name
			},
			want: "already used",
		},
		{
			name:   "no hubs declared",
			mutate: func(d *spec.Document) { d.Spec.Hubs = nil },
			want:   "at least one hub is required",
		},
		{
			name:   "invalid endpoint",
			mutate: func(d *spec.Document) { d.Spec.Hubs[0].Endpoint = "not-host-port" },
			want:   "endpoint",
		},
		{
			name:   "MTU out of range",
			mutate: func(d *spec.Document) { d.Spec.WireGuard.MTU = 800 },
			want:   "mtu",
		},
		{
			name:   "ACL with empty from",
			mutate: func(d *spec.Document) { d.Spec.ACLs[0].From = nil },
			want:   "from",
		},
		{
			name:   "ACL with invalid action",
			mutate: func(d *spec.Document) { d.Spec.ACLs[0].Action = "log" },
			want:   "action",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := deepCopy(good)
			doc.ApplyDefaults()
			tc.mutate(doc)
			err := doc.Validate()
			if err == nil {
				t.Fatalf("Validate returned nil, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// deepCopy round-trips a document through Marshal/Parse to obtain an
// independent copy. We rely on the spec being marshalable.
func deepCopy(d *spec.Document) *spec.Document {
	data, err := spec.Marshal(d)
	if err != nil {
		panic(err)
	}
	// Bypass validation since we want to mutate intentionally; reuse the
	// raw decoder by going through Parse on a known-good source then
	// re-decoding without validation.
	out := *d
	out.Spec.Hubs = append([]spec.Hub(nil), d.Spec.Hubs...)
	out.Spec.Agents = make([]spec.Agent, len(d.Spec.Agents))
	for i := range d.Spec.Agents {
		a := d.Spec.Agents[i]
		a.Groups = append([]string(nil), d.Spec.Agents[i].Groups...)
		out.Spec.Agents[i] = a
	}
	out.Spec.Groups = append([]string(nil), d.Spec.Groups...)
	out.Spec.ACLs = make([]spec.ACL, len(d.Spec.ACLs))
	for i := range d.Spec.ACLs {
		a := d.Spec.ACLs[i]
		a.From = append([]string(nil), d.Spec.ACLs[i].From...)
		a.To = append([]string(nil), d.Spec.ACLs[i].To...)
		out.Spec.ACLs[i] = a
	}
	_ = data
	return &out
}
