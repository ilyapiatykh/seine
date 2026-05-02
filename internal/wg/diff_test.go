package wg

import (
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func mustKey(t *testing.T, seed byte) wgtypes.Key {
	t.Helper()
	var k wgtypes.Key
	for i := range k {
		k[i] = seed + byte(i)
	}
	return k
}

func TestComputeDiff_AddedRemovedUpdated(t *testing.T) {
	keyA := mustKey(t, 1)
	keyB := mustKey(t, 2)
	keyC := mustKey(t, 3)

	current := map[wgtypes.Key]Peer{
		keyA: {
			PublicKey:           keyA,
			Endpoint:            &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820},
			AllowedIPs:          []netip.Prefix{netip.MustParsePrefix("100.64.0.1/32")},
			PersistentKeepalive: 25 * time.Second,
		},
		keyB: {
			PublicKey:  keyB,
			Endpoint:   nil,
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("100.64.1.1/32")},
		},
	}
	desired := []Peer{
		// keyA: unchanged
		{
			PublicKey:           keyA,
			Endpoint:            &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820},
			AllowedIPs:          []netip.Prefix{netip.MustParsePrefix("100.64.0.1/32")},
			PersistentKeepalive: 25 * time.Second,
		},
		// keyB: AllowedIPs changed → updated
		{
			PublicKey:  keyB,
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("100.64.1.1/32"), netip.MustParsePrefix("100.64.1.2/32")},
		},
		// keyC: new
		{
			PublicKey:  keyC,
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("100.64.2.0/24")},
		},
	}
	diff := computeDiff(current, desired)
	if len(diff.Added) != 1 || diff.Added[0].PublicKey != keyC {
		t.Fatalf("Added = %+v", diff.Added)
	}
	if len(diff.Updated) != 1 || diff.Updated[0].PublicKey != keyB {
		t.Fatalf("Updated = %+v", diff.Updated)
	}
	if len(diff.Removed) != 0 {
		t.Fatalf("Removed = %+v", diff.Removed)
	}
}

func TestComputeDiff_RemovesMissing(t *testing.T) {
	keyA := mustKey(t, 1)
	keyB := mustKey(t, 2)
	current := map[wgtypes.Key]Peer{
		keyA: {PublicKey: keyA},
		keyB: {PublicKey: keyB},
	}
	diff := computeDiff(current, []Peer{{PublicKey: keyA}})
	if !reflect.DeepEqual(diff.Removed, []wgtypes.Key{keyB}) {
		t.Errorf("Removed = %+v, want [keyB]", diff.Removed)
	}
}

func TestComputeDiff_AllowedIPsOrderIndependent(t *testing.T) {
	keyA := mustKey(t, 1)
	current := map[wgtypes.Key]Peer{
		keyA: {
			PublicKey: keyA,
			AllowedIPs: []netip.Prefix{
				netip.MustParsePrefix("100.64.1.1/32"),
				netip.MustParsePrefix("100.64.1.2/32"),
			},
		},
	}
	desired := []Peer{
		{
			PublicKey: keyA,
			AllowedIPs: []netip.Prefix{
				netip.MustParsePrefix("100.64.1.2/32"),
				netip.MustParsePrefix("100.64.1.1/32"),
			},
		},
	}
	diff := computeDiff(current, desired)
	if !diff.Empty() {
		t.Errorf("expected no diff for reordered AllowedIPs, got %+v", diff)
	}
}

func TestComputeDiff_EndpointChange(t *testing.T) {
	keyA := mustKey(t, 1)
	current := map[wgtypes.Key]Peer{
		keyA: {
			PublicKey: keyA,
			Endpoint:  &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 51820},
		},
	}
	desired := []Peer{
		{
			PublicKey: keyA,
			Endpoint:  &net.UDPAddr{IP: net.ParseIP("5.6.7.8"), Port: 51820},
		},
	}
	diff := computeDiff(current, desired)
	if len(diff.Updated) != 1 {
		t.Errorf("expected updated peer for endpoint change, got %+v", diff)
	}
}

func TestComputeDiff_KeepaliveZeroNormalised(t *testing.T) {
	keyA := mustKey(t, 1)
	current := map[wgtypes.Key]Peer{
		keyA: {PublicKey: keyA, PersistentKeepalive: 0},
	}
	desired := []Peer{{PublicKey: keyA, PersistentKeepalive: -1}}
	diff := computeDiff(current, desired)
	if !diff.Empty() {
		t.Errorf("expected no diff for negative→zero keepalive, got %+v", diff)
	}
}
