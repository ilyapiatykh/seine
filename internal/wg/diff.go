package wg

import (
	"net"
	"net/netip"
	"sort"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Diff describes a peer-set delta applied by ApplyPeers.
type Diff struct {
	Added   []Peer
	Updated []Peer
	Removed []wgtypes.Key
}

// Empty reports whether the diff contains no changes.
func (d Diff) Empty() bool {
	return len(d.Added) == 0 && len(d.Updated) == 0 && len(d.Removed) == 0
}

// computeDiff returns the changes required to move from current to desired.
// "Current" is keyed by public key; "desired" is the target peer set in
// declaration order. A peer is considered Updated if any of its endpoint,
// allowed IPs or keepalive change; AllowedIPs comparison is order-
// independent (we sort before comparing).
func computeDiff(current map[wgtypes.Key]Peer, desired []Peer) Diff {
	var d Diff
	seen := make(map[wgtypes.Key]struct{}, len(desired))
	for _, want := range desired {
		seen[want.PublicKey] = struct{}{}
		have, ok := current[want.PublicKey]
		if !ok {
			d.Added = append(d.Added, want)
			continue
		}
		if !peersEqual(have, want) {
			d.Updated = append(d.Updated, want)
		}
	}
	for k := range current {
		if _, kept := seen[k]; !kept {
			d.Removed = append(d.Removed, k)
		}
	}
	return d
}

// peersEqual compares the configuration-relevant fields of two peers. The
// caller must guarantee both have a non-zero public key (the map key).
func peersEqual(a, b Peer) bool {
	if a.PublicKey != b.PublicKey {
		return false
	}
	if !udpAddrsEqual(a.Endpoint, b.Endpoint) {
		return false
	}
	if !prefixSetsEqual(a.AllowedIPs, b.AllowedIPs) {
		return false
	}
	if normalizeKeepalive(a.PersistentKeepalive) != normalizeKeepalive(b.PersistentKeepalive) {
		return false
	}
	return true
}

func normalizeKeepalive(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

func udpAddrsEqual(a, b *net.UDPAddr) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Port == b.Port && a.IP.Equal(b.IP) && a.Zone == b.Zone
}

func prefixSetsEqual(a, b []netip.Prefix) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]netip.Prefix(nil), a...)
	y := append([]netip.Prefix(nil), b...)
	sort.Slice(x, func(i, j int) bool { return x[i].String() < x[j].String() })
	sort.Slice(y, func(i, j int) bool { return y[i].String() < y[j].String() })
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
