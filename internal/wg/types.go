package wg

import (
	"context"
	"net"
	"net/netip"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Peer is the agent-facing description of a WireGuard peer.
type Peer struct {
	// Name is the spec-declared identity. Carried for diagnostics; it is
	// not part of the WireGuard protocol.
	Name string

	// PublicKey is the peer's static Curve25519 public key.
	PublicKey wgtypes.Key

	// Endpoint is where outbound handshakes are sent. nil means
	// "listen-only": the peer can connect to us but we do not initiate.
	Endpoint *net.UDPAddr

	// AllowedIPs is the set of overlay prefixes WireGuard will accept
	// from this peer (and the route prefixes installed for outbound
	// traffic — but route installation lives in the netiface package).
	AllowedIPs []netip.Prefix

	// PersistentKeepalive sends an empty packet at this cadence to keep
	// stateful NAT mappings open. 0 disables.
	PersistentKeepalive time.Duration
}

// UpOptions controls the initial bring-up of the local WireGuard interface.
type UpOptions struct {
	PrivateKey wgtypes.Key

	// ListenPort is the UDP port WireGuard binds to. 0 means "system
	// assigned" — typical for spokes that connect outbound only.
	ListenPort int

	// Address is the tunnel IP assigned to the local interface. The
	// prefix length is also used to install the connected-route for the
	// overlay.
	Address netip.Prefix

	// MTU is the interface MTU. 0 leaves it at the system default.
	MTU int
}

// Status is a snapshot of the live interface, returned for telemetry.
type Status struct {
	Name       string
	PublicKey  wgtypes.Key
	ListenPort int
	Peers      []PeerStatus
}

// PeerStatus is per-peer runtime information.
type PeerStatus struct {
	PublicKey         wgtypes.Key
	Endpoint          *net.UDPAddr
	AllowedIPs        []netip.Prefix
	LastHandshake     time.Time
	BytesReceived     int64
	BytesTransmitted  int64
}

// Interface manages a single WireGuard tunnel on the local host.
type Interface interface {
	// Name returns the OS-level interface name (e.g. "wg0").
	Name() string

	// Up creates the interface (if absent), assigns an address, sets MTU
	// and brings the link administratively up. Idempotent.
	Up(ctx context.Context, opts UpOptions) error

	// ApplyPeers replaces the WireGuard peer set with desired and returns
	// the diff that was applied. Stable peers (same public key, same
	// configuration) are not touched.
	ApplyPeers(ctx context.Context, desired []Peer) (Diff, error)

	// Status returns a snapshot of the interface and its peers.
	Status(ctx context.Context) (Status, error)

	// Down tears down the interface. Idempotent.
	Down(ctx context.Context) error

	// Close releases handles. Down is not implied.
	Close() error
}
