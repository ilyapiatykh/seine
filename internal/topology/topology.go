// Package topology computes the desired WireGuard peer set for a single
// agent given the network spec and the runtime peer registry from the
// control plane.
//
// The thesis settles on a hub-and-spoke topology where hubs form a full
// mesh and spokes route all overlay traffic through their assigned hub.
// This package encodes that decision: it does not implement direct
// spoke-to-spoke connectivity, and there is no NAT-traversal path —
// inter-spoke flows always transit at least one hub.
package topology

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	cpv1 "github.com/ilyapiatykh/seine/api/proto/seine/controlplane/v1"
	"github.com/ilyapiatykh/seine/internal/spec"
	"github.com/ilyapiatykh/seine/internal/wg"
)

// Self describes the local agent's identity in the network. Resolved from
// the spec by FindSelf.
type Self struct {
	Name     string
	Role     cpv1.Role
	TunnelIP netip.Addr

	// HubName is populated when Role == ROLE_SPOKE and points at the
	// hub assigned to this spoke in the spec.
	HubName string
}

// FindSelf locates name inside the spec. Returns an error if name does
// not match any declared hub or agent.
func FindSelf(doc *spec.Document, name string) (Self, error) {
	if h := doc.FindHub(name); h != nil {
		addr, err := netip.ParseAddr(h.TunnelIP)
		if err != nil {
			return Self{}, fmt.Errorf("topology: parse tunnel IP for hub %s: %w", name, err)
		}
		return Self{Name: name, Role: cpv1.Role_ROLE_HUB, TunnelIP: addr}, nil
	}
	if a := doc.FindAgent(name); a != nil {
		addr, err := netip.ParseAddr(a.TunnelIP)
		if err != nil {
			return Self{}, fmt.Errorf("topology: parse tunnel IP for agent %s: %w", name, err)
		}
		return Self{
			Name:     name,
			Role:     cpv1.Role_ROLE_SPOKE,
			TunnelIP: addr,
			HubName:  a.Hub,
		}, nil
	}
	return Self{}, fmt.Errorf("topology: %q is not declared in the spec", name)
}

// PeersFor returns the WireGuard peer set the local agent should install.
//
// The returned slice is the *complete* desired state: callers pass it to
// wg.Interface.ApplyPeers which diffs against the kernel's current set.
func PeersFor(
	doc *spec.Document,
	self Self,
	registry []*cpv1.PeerInfo,
	keepalive time.Duration,
) ([]wg.Peer, error) {
	overlay, err := netip.ParsePrefix(doc.Spec.CIDR)
	if err != nil {
		return nil, fmt.Errorf("topology: parse overlay CIDR: %w", err)
	}

	byName := make(map[string]*cpv1.PeerInfo, len(registry))
	for _, p := range registry {
		if p == nil || p.Name == "" || p.PublicKey == "" {
			continue
		}
		byName[p.Name] = p
	}

	switch self.Role {
	case cpv1.Role_ROLE_SPOKE:
		return spokePeers(self, byName, overlay, keepalive)
	case cpv1.Role_ROLE_HUB:
		return hubPeers(doc, self, byName, keepalive)
	default:
		return nil, fmt.Errorf("topology: unsupported role %v", self.Role)
	}
}

// spokePeers returns the singleton peer list for a spoke: its assigned hub,
// configured to forward all overlay traffic.
func spokePeers(
	self Self,
	byName map[string]*cpv1.PeerInfo,
	overlay netip.Prefix,
	keepalive time.Duration,
) ([]wg.Peer, error) {
	hub, ok := byName[self.HubName]
	if !ok {
		return nil, fmt.Errorf("topology: hub %q has not yet registered", self.HubName)
	}
	pub, err := wgtypes.ParseKey(hub.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("topology: parse hub public key: %w", err)
	}
	endpoint, err := resolveEndpoint(hub.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("topology: resolve hub endpoint %q: %w", hub.Endpoint, err)
	}
	if endpoint == nil {
		return nil, fmt.Errorf("topology: hub %q has no advertised endpoint", self.HubName)
	}
	return []wg.Peer{{
		Name:                hub.Name,
		PublicKey:           pub,
		Endpoint:            endpoint,
		AllowedIPs:          []netip.Prefix{overlay},
		PersistentKeepalive: keepalive,
	}}, nil
}

// hubPeers returns the peer set for a hub: every other hub in the mesh
// (with AllowedIPs covering that hub plus the spokes assigned to it) and
// every spoke whose `hub` field points at this hub.
func hubPeers(
	doc *spec.Document,
	self Self,
	byName map[string]*cpv1.PeerInfo,
	keepalive time.Duration,
) ([]wg.Peer, error) {
	var peers []wg.Peer

	for _, h := range doc.Spec.Hubs {
		if h.Name == self.Name {
			continue
		}
		info, ok := byName[h.Name]
		if !ok {
			// Other hub not yet registered — skip; will materialise on
			// the next reconcile.
			continue
		}
		pub, err := wgtypes.ParseKey(info.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("topology: hub %s public key: %w", h.Name, err)
		}
		endpoint, err := resolveEndpoint(info.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("topology: hub %s endpoint: %w", h.Name, err)
		}
		if endpoint == nil {
			// Mesh peers must dial each other; no endpoint is fatal.
			return nil, fmt.Errorf("topology: hub %s has empty endpoint", h.Name)
		}
		allowed, err := hubMeshAllowedIPs(doc, &h)
		if err != nil {
			return nil, err
		}
		peers = append(peers, wg.Peer{
			Name:                h.Name,
			PublicKey:           pub,
			Endpoint:            endpoint,
			AllowedIPs:          allowed,
			PersistentKeepalive: keepalive,
		})
	}

	for _, a := range doc.AgentsForHub(self.Name) {
		info, ok := byName[a.Name]
		if !ok {
			// Spoke not yet registered — skip; it'll appear on the
			// next heartbeat after it has called Register.
			continue
		}
		pub, err := wgtypes.ParseKey(info.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("topology: spoke %s public key: %w", a.Name, err)
		}
		// Hubs do not initiate to spokes — leave Endpoint nil so the
		// kernel learns it from the spoke's incoming handshake.
		spokeIP, err := singleHostPrefix(a.TunnelIP)
		if err != nil {
			return nil, err
		}
		peers = append(peers, wg.Peer{
			Name:       a.Name,
			PublicKey:  pub,
			Endpoint:   nil,
			AllowedIPs: []netip.Prefix{spokeIP},
		})
	}

	return peers, nil
}

// hubMeshAllowedIPs computes the AllowedIPs that hub `self` should set on
// peer `other`: other's tunnel IP plus the tunnel IPs of all spokes
// anchored to `other`. This is what implements hub-to-hub forwarding.
func hubMeshAllowedIPs(doc *spec.Document, other *spec.Hub) ([]netip.Prefix, error) {
	otherIP, err := singleHostPrefix(other.TunnelIP)
	if err != nil {
		return nil, err
	}
	allowed := []netip.Prefix{otherIP}
	for _, s := range doc.AgentsForHub(other.Name) {
		p, err := singleHostPrefix(s.TunnelIP)
		if err != nil {
			return nil, err
		}
		allowed = append(allowed, p)
	}
	return allowed, nil
}

func singleHostPrefix(addr string) (netip.Prefix, error) {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("topology: parse address %q: %w", addr, err)
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// resolveEndpoint converts a host:port string into a *net.UDPAddr, doing
// DNS resolution if host is not a literal IP. Returns nil for empty input.
func resolveEndpoint(ep string) (*net.UDPAddr, error) {
	if ep == "" {
		return nil, nil
	}
	host, portStr, err := net.SplitHostPort(ep)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		return &net.UDPAddr{IP: ip, Port: port}, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("no IPs returned by DNS")
	}
	return &net.UDPAddr{IP: ips[0], Port: port}, nil
}
