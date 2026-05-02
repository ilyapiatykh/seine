package topology_test

import (
	"net/netip"
	"sort"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	cpv1 "github.com/ilyapiatykh/seine/api/proto/seine/controlplane/v1"
	"github.com/ilyapiatykh/seine/internal/spec"
	"github.com/ilyapiatykh/seine/internal/topology"
)

// makeKey returns a deterministic key derived from seed. It is not a
// cryptographically meaningful private key; we only need a parseable
// 32-byte value because PeersFor parses public keys via wgtypes.ParseKey.
func makeKey(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	k, err := wgtypes.NewKey(raw[:])
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return k.String()
}

func newSpec(t *testing.T) *spec.Document {
	t.Helper()
	d := &spec.Document{
		APIVersion: spec.APIVersion,
		Kind:       spec.Kind,
		Metadata:   spec.Metadata{Name: "test"},
		Spec: spec.Network{
			CIDR: "100.64.0.0/10",
			Hubs: []spec.Hub{
				{Name: "hub-eu", Endpoint: "10.0.0.1:51820", TunnelIP: "100.64.0.1"},
				{Name: "hub-us", Endpoint: "10.0.0.2:51820", TunnelIP: "100.64.0.2"},
			},
			Agents: []spec.Agent{
				{Name: "spoke-a", TunnelIP: "100.64.1.1", Hub: "hub-eu", Groups: []string{"g"}},
				{Name: "spoke-b", TunnelIP: "100.64.1.2", Hub: "hub-eu", Groups: []string{"g"}},
				{Name: "spoke-c", TunnelIP: "100.64.2.1", Hub: "hub-us", Groups: []string{"g"}},
			},
			Groups: []string{"g"},
			ACLs:   []spec.ACL{{From: []string{"g"}, To: []string{"g"}, Action: spec.ActionAllow}},
		},
	}
	d.ApplyDefaults()
	if err := d.Validate(); err != nil {
		t.Fatalf("invalid test spec: %v", err)
	}
	return d
}

func TestFindSelf(t *testing.T) {
	d := newSpec(t)
	hub, err := topology.FindSelf(d, "hub-eu")
	if err != nil {
		t.Fatalf("FindSelf hub: %v", err)
	}
	if hub.Role != cpv1.Role_ROLE_HUB || hub.TunnelIP.String() != "100.64.0.1" {
		t.Errorf("hub self: %+v", hub)
	}
	spoke, err := topology.FindSelf(d, "spoke-a")
	if err != nil {
		t.Fatalf("FindSelf spoke: %v", err)
	}
	if spoke.Role != cpv1.Role_ROLE_SPOKE || spoke.HubName != "hub-eu" {
		t.Errorf("spoke self: %+v", spoke)
	}
	if _, err := topology.FindSelf(d, "ghost"); err == nil {
		t.Error("FindSelf ghost: expected error")
	}
}

func registry(t *testing.T) []*cpv1.PeerInfo {
	t.Helper()
	return []*cpv1.PeerInfo{
		{Name: "hub-eu", PublicKey: makeKey(t, 1), Endpoint: "10.0.0.1:51820", TunnelIp: "100.64.0.1", Role: cpv1.Role_ROLE_HUB},
		{Name: "hub-us", PublicKey: makeKey(t, 2), Endpoint: "10.0.0.2:51820", TunnelIp: "100.64.0.2", Role: cpv1.Role_ROLE_HUB},
		{Name: "spoke-a", PublicKey: makeKey(t, 3), Endpoint: "", TunnelIp: "100.64.1.1", Role: cpv1.Role_ROLE_SPOKE},
		{Name: "spoke-b", PublicKey: makeKey(t, 4), Endpoint: "", TunnelIp: "100.64.1.2", Role: cpv1.Role_ROLE_SPOKE},
		{Name: "spoke-c", PublicKey: makeKey(t, 5), Endpoint: "", TunnelIp: "100.64.2.1", Role: cpv1.Role_ROLE_SPOKE},
	}
}

func TestPeersForSpoke(t *testing.T) {
	d := newSpec(t)
	self, _ := topology.FindSelf(d, "spoke-a")
	peers, err := topology.PeersFor(d, self, registry(t), 25*time.Second)
	if err != nil {
		t.Fatalf("PeersFor: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("want 1 peer (its hub), got %d", len(peers))
	}
	p := peers[0]
	if p.Name != "hub-eu" {
		t.Errorf("peer name = %q", p.Name)
	}
	if p.Endpoint == nil || p.Endpoint.String() != "10.0.0.1:51820" {
		t.Errorf("endpoint = %v", p.Endpoint)
	}
	if len(p.AllowedIPs) != 1 || p.AllowedIPs[0].String() != "100.64.0.0/10" {
		t.Errorf("AllowedIPs = %v", p.AllowedIPs)
	}
	if p.PersistentKeepalive != 25*time.Second {
		t.Errorf("keepalive = %v", p.PersistentKeepalive)
	}
}

func TestPeersForSpoke_HubNotRegistered(t *testing.T) {
	d := newSpec(t)
	self, _ := topology.FindSelf(d, "spoke-a")
	reg := registry(t)
	// Strip hub-eu from the registry.
	var filtered []*cpv1.PeerInfo
	for _, p := range reg {
		if p.Name != "hub-eu" {
			filtered = append(filtered, p)
		}
	}
	if _, err := topology.PeersFor(d, self, filtered, 25*time.Second); err == nil {
		t.Fatal("expected error when hub not registered")
	}
}

func TestPeersForHub_MeshAndSpokes(t *testing.T) {
	d := newSpec(t)
	self, _ := topology.FindSelf(d, "hub-eu")
	peers, err := topology.PeersFor(d, self, registry(t), 25*time.Second)
	if err != nil {
		t.Fatalf("PeersFor: %v", err)
	}
	// Expected: hub-us (mesh) + spoke-a + spoke-b (anchored to hub-eu).
	if len(peers) != 3 {
		t.Fatalf("want 3 peers, got %d: %+v", len(peers), peers)
	}

	byName := map[string]wgPeerView{}
	for _, p := range peers {
		ips := make([]string, 0, len(p.AllowedIPs))
		for _, ip := range p.AllowedIPs {
			ips = append(ips, ip.String())
		}
		sort.Strings(ips)
		byName[p.Name] = wgPeerView{
			HasEndpoint: p.Endpoint != nil,
			AllowedIPs:  ips,
			Keepalive:   p.PersistentKeepalive,
		}
	}

	// hub-us: must have endpoint, AllowedIPs = its tunnel IP + its spokes.
	hu := byName["hub-us"]
	if !hu.HasEndpoint {
		t.Errorf("hub-us must have endpoint (mesh dial)")
	}
	wantHU := []string{"100.64.0.2/32", "100.64.2.1/32"}
	sort.Strings(wantHU)
	if !equalStrings(hu.AllowedIPs, wantHU) {
		t.Errorf("hub-us AllowedIPs = %v, want %v", hu.AllowedIPs, wantHU)
	}
	if hu.Keepalive != 25*time.Second {
		t.Errorf("hub-us keepalive = %v", hu.Keepalive)
	}

	// spoke-a / spoke-b: no endpoint (hub does not dial spoke), one /32.
	for _, s := range []string{"spoke-a", "spoke-b"} {
		v := byName[s]
		if v.HasEndpoint {
			t.Errorf("%s: hub should not dial spoke", s)
		}
		if v.Keepalive != 0 {
			t.Errorf("%s: hub→spoke keepalive should be 0, got %v", s, v.Keepalive)
		}
	}

	// spoke-c (anchored to hub-us) must NOT appear in hub-eu's peer list.
	if _, ok := byName["spoke-c"]; ok {
		t.Error("spoke-c (anchored to hub-us) leaked into hub-eu peers")
	}
}

func TestPeersForHub_SkipsUnregisteredPeers(t *testing.T) {
	d := newSpec(t)
	self, _ := topology.FindSelf(d, "hub-eu")
	// Only the caller has registered.
	peers, err := topology.PeersFor(d, self, []*cpv1.PeerInfo{
		{Name: "hub-eu", PublicKey: makeKey(t, 1), Endpoint: "10.0.0.1:51820"},
	}, 25*time.Second)
	if err != nil {
		t.Fatalf("PeersFor: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("want 0 peers when no others registered, got %d", len(peers))
	}
}

type wgPeerView struct {
	HasEndpoint bool
	AllowedIPs  []string
	Keepalive   time.Duration
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Sanity check that overlay parsing is not regressed by future spec edits.
func TestPeersForSpoke_OverlayMatchesSpec(t *testing.T) {
	d := newSpec(t)
	self, _ := topology.FindSelf(d, "spoke-a")
	peers, err := topology.PeersFor(d, self, registry(t), 0)
	if err != nil {
		t.Fatalf("PeersFor: %v", err)
	}
	overlay := netip.MustParsePrefix(d.Spec.CIDR)
	if peers[0].AllowedIPs[0] != overlay {
		t.Errorf("AllowedIPs = %v, want %v", peers[0].AllowedIPs[0], overlay)
	}
}
