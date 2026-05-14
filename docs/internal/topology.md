# Package `internal/topology`

Computes the desired WireGuard peer set for a single agent given the
network spec and the runtime peer registry returned by the management
server. This is where the thesis' multi-hub topology decision is
encoded in code.

## File

- `topology.go` — sole file.

## Topology rules in one paragraph

The thesis settles on a hub-and-spoke topology where hubs form a full
mesh and spokes route all overlay traffic through their assigned hub.
The package is the literal expression of that decision: it does not
emit any spoke-to-spoke peer entries, and inter-spoke traffic is
intentionally forced to transit at least one hub.

## Public API

### `type Self`

Identity of the local agent inside the spec.

```go
type Self struct {
    Name     string
    Role     cpv1.Role     // ROLE_HUB or ROLE_SPOKE
    TunnelIP netip.Addr
    HubName  string         // non-empty only for spokes
}
```

### `func FindSelf(doc *spec.Document, name string) (Self, error)`

Looks `name` up among `doc.Spec.Hubs` then `doc.Spec.Agents` and fills
the appropriate fields. Returns an error if `name` is declared in
neither.

### `func PeersFor(doc, self, registry, keepalive) ([]wg.Peer, error)`

Returns the *complete* desired peer set for `self`. Callers pass the
slice to `wg.Interface.ApplyPeers`, which computes the kernel diff
internally.

Inputs:

- `doc *spec.Document` — the parsed spec.
- `self Self` — what the local agent is in this spec.
- `registry []*cpv1.PeerInfo` — the latest registry returned by
  `Heartbeat`. Each entry that is missing a name or a public key is
  silently dropped; an entry that is in the registry but absent from
  the spec is also dropped (handled by the server).
- `keepalive time.Duration` — applied to the spoke→hub peer and to
  hub-mesh peers; not applied to hub→spoke peers (hubs do not initiate
  to spokes).

Outputs:

- A `[]wg.Peer` with no spoke-to-spoke entries, no self-entries, and
  no entries for peers that have not yet registered.
- An error if a referenced public key fails to parse, or if a peer's
  endpoint is required but missing or unresolvable.

## Per-role behaviour

### Spoke

`spokePeers` returns exactly one `wg.Peer`:

| Field | Value |
| --- | --- |
| `Name` | name of the spoke's hub |
| `PublicKey` | hub's registered public key |
| `Endpoint` | hub's resolved endpoint (DNS lookup if needed) |
| `AllowedIPs` | the entire overlay CIDR (e.g. `100.64.0.0/10`) |
| `PersistentKeepalive` | `keepalive` |

Setting `AllowedIPs = overlay` is the routing contract: from the
spoke's perspective everything inside the overlay is reachable through
the WG interface, and WireGuard hands the packet to the hub for
forwarding.

If the configured hub has not yet registered, `PeersFor` returns
`"hub … has not yet registered"`. The agent's reconciliation loop
treats this as a soft failure and retries on the next tick.

### Hub

`hubPeers` returns one entry per other hub plus one entry per assigned
spoke.

For each *other hub* `h`:

| Field | Value |
| --- | --- |
| `Name` | `h.Name` |
| `PublicKey` | `h`'s registered public key |
| `Endpoint` | `h`'s resolved endpoint (must be set) |
| `AllowedIPs` | `[h.TunnelIP/32, …all spokes anchored to h]/32` |
| `PersistentKeepalive` | `keepalive` |

The `AllowedIPs` formula is what makes hub-to-hub forwarding work: a
hub knows that the other hub "covers" both itself and the spokes
attached to it, so traffic toward any of those addresses is encrypted
and sent over the mesh peer.

For each *spoke* `s` anchored to this hub:

| Field | Value |
| --- | --- |
| `Name` | `s.Name` |
| `PublicKey` | `s`'s registered public key |
| `Endpoint` | `nil` (hub does not initiate; learns endpoint from incoming handshake) |
| `AllowedIPs` | `[s.TunnelIP/32]` |
| `PersistentKeepalive` | `0` |

Hubs do not actively dial spokes; spokes initiate, and WireGuard's
roaming handshake carries each spoke's current public IP/port up to
the hub.

## Endpoint resolution (`resolveEndpoint`)

A `host:port` string is converted to `*net.UDPAddr`:

1. If `host` is an IP literal, use it directly.
2. Otherwise resolve with `net.LookupIP` and pick the first result.
3. An empty string returns `(nil, nil)` — used for hub→spoke entries
   where there is no known endpoint.

DNS failure produces an error that aborts the current `PeersFor` call,
which the agent treats as a soft failure (the cycle retries).

The resolution happens at peer-set computation time, not at config
load. Specs are reviewable offline because the spec itself is not
resolved at parse time.

## Failure-mode summary

| Condition | Outcome |
| --- | --- |
| `self.Role` neither hub nor spoke | Error: "unsupported role". |
| Spoke's hub not in registry | Error; agent retries next tick. |
| Hub mesh peer has empty endpoint in registry | Error; mesh peers must dial each other. |
| Public key cannot be parsed | Error; usually means the registry is corrupted. |
| Other hub or spoke not in registry | Silently skipped; appears on the next tick. |
| `doc.Spec.CIDR` invalid | Error; should be impossible because the spec is `Validate`d. |

## Used by

- `agentcore.reconcileOnce` — the only direct consumer; the slice
  feeds straight into `wg.Interface.ApplyPeers`.
