# Package `internal/wg`

WireGuard interface abstraction. The agent drives the local tunnel
through this package: it owns the keypair, brings the kernel netdev
up, applies a desired peer set, and tears the netdev down on exit.

## Files

- `keys.go` — `Keypair`, `GenerateKeypair`, `LoadOrGenerate`.
- `types.go` — `Peer`, `UpOptions`, `Status`, `PeerStatus`, the
  `Interface` interface.
- `diff.go` — `Diff` and the unexported `computeDiff`.
- `iface.go` — `New(name)` factory.
- `iface_linux.go` — Linux kernel implementation (build tag: `linux`).
- `iface_unsupported.go` — stub for other operating systems
  (build tag: `!linux`).

## Why an abstraction over `wgctrl`

Two reasons:

1. **Testability.** `agentcore` depends on the `wg.Interface`
   interface, not on `wgctrl.Client`. Tests can fake the interface.
2. **Portability shape.** Even though only the Linux kernel backend is
   implemented, the surface is shaped to admit a `wireguard-go`
   userspace backend without changes to callers. The interface trades
   `*net.UDPAddr` and `netip.Prefix` instead of leaking
   `wgtypes.PeerConfig` so the second backend can be added in a
   future iteration.

## Public API

### Keys

```go
type Keypair struct {
    Private wgtypes.Key
    Public  wgtypes.Key
}

func GenerateKeypair() (Keypair, error)
func LoadOrGenerate(path string) (Keypair, error)
```

`GenerateKeypair` calls `wgtypes.GeneratePrivateKey()` (Curve25519)
and returns both halves.

`LoadOrGenerate`:

- If the file at `path` exists and parses as a base64 WG private key,
  returns the parsed keypair (whitespace-tolerant).
- Otherwise generates a new one, creates `dirname(path)` with `0700`,
  and writes the key with `0600` (newline-terminated, base64 form).
- Failure modes are surfaced with the original path embedded for
  legibility ("read", "parse", "mkdir", "write").

### `type Peer`

The agent-facing WireGuard peer description. Free of `wgctrl` types.

```go
type Peer struct {
    Name                string         // for diagnostics; not part of WG protocol
    PublicKey           wgtypes.Key    // 32-byte Curve25519
    Endpoint            *net.UDPAddr   // nil → listen-only
    AllowedIPs          []netip.Prefix
    PersistentKeepalive time.Duration  // 0 disables
}
```

### `type UpOptions`

```go
type UpOptions struct {
    PrivateKey wgtypes.Key
    ListenPort int           // 0 → system assigned
    Address    netip.Prefix  // tunnel IP plus the overlay prefix length
    MTU        int           // 0 → leave at default
}
```

`Address` is intentionally a `Prefix`, not a bare address: the prefix
length doubles as the connected-route declaration that lets the kernel
treat the entire overlay subnet as reachable through this interface.
For the thesis demo, that prefix is the network's `spec.cidr` (e.g.
`/10`).

### `type Diff`

The result of a peer-set reconcile.

```go
type Diff struct {
    Added   []Peer
    Updated []Peer
    Removed []wgtypes.Key
}

func (d Diff) Empty() bool
```

A peer counts as `Updated` if its endpoint, AllowedIPs (compared as a
set, order-independent) or persistent-keepalive change. Equal
configurations are silently kept stable so logs do not spam at every
tick.

### `type Status` / `type PeerStatus`

Runtime view returned by `Interface.Status`. Includes per-peer last
handshake and byte counters — useful for `wg show`-style telemetry.

### `type Interface`

```go
type Interface interface {
    Name() string
    Up(ctx context.Context, opts UpOptions) error
    ApplyPeers(ctx context.Context, desired []Peer) (Diff, error)
    Status(ctx context.Context) (Status, error)
    Down(ctx context.Context) error
    Close() error
}
```

Method semantics:

- `Up` is idempotent: it creates the netdev if missing, configures the
  private key, listen port, address and MTU, then brings the link
  administratively up. Calling `Up` twice is safe.
- `ApplyPeers` reads the current kernel state, computes a `Diff`, and
  applies the additions / updates / removals in a single
  `wgctrl.ConfigureDevice` call. Returns the diff that was applied.
- `Status` reads the kernel state and returns it in the package's
  domain types.
- `Down` deletes the netdev. Idempotent.
- `Close` releases handles (for the kernel backend, the wgctrl
  client). Does *not* imply `Down`.

### `func New(name string) (Interface, error)`

Factory. On Linux returns the kernel implementation; on other systems
returns an error.

## Linux backend (`iface_linux.go`)

Implements `Interface` on top of:

- `github.com/vishvananda/netlink` — link, address and MTU management.
- `golang.zx2c4.com/wireguard/wgctrl` — peer and device configuration.

A single `wgctrl.Client` is held for the lifetime of the iface to
avoid reopening the netlink generic socket per call.

Notable details:

- `Up` calls `ensureLink`, which creates a `netlink.Wireguard` link if
  `LinkByName` returns `LinkNotFoundError`. MTU updates after creation
  use `LinkSetMTU` so existing-link cases also pick them up.
- The address is set with `netlink.AddrReplace`, which is idempotent.
- `ApplyPeers` builds one `wgtypes.Config` whose `Peers` field
  contains the additions (with `ReplaceAllowedIPs=false`), the updates
  (with `ReplaceAllowedIPs=true` to overwrite cleanly), and the
  removals (`Remove=true`). All applied in a single
  `ConfigureDevice` call to keep the kernel snapshot consistent.
- Conversions between domain types (`netip.Prefix`, `wgtypes.Key`) and
  `wgctrl`/`netlink` types are kept private to this file.

## Unsupported-platform stub (`iface_unsupported.go`)

Returns an error from `newPlatformInterface` that names the host OS.
The package still compiles on macOS and Windows so the rest of the
codebase can be cross-built, with the failure deferred to the agent's
`wg.New` call.

## Concurrency

The kernel backend serialises calls through a single `wgctrl.Client`.
Concurrent calls to `Up`, `ApplyPeers`, `Status` and `Down` from
multiple goroutines on the same interface instance are not part of the
contract — `agentcore` keeps a single goroutine driving each.

## Used by

- `agentcore` — the only consumer.
- `topology` — produces the `[]wg.Peer` slice fed to `ApplyPeers`.
