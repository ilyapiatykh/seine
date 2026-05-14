# Package `internal/spec`

Declarative network specification: types, parser, defaults and
validator. This is the schema operators write into Git.

## Files

- `types.go` — Go types matching the YAML schema.
- `parse.go` — `Parse`, `LoadFile`, `Marshal`.
- `defaults.go` — `Document.ApplyDefaults`.
- `validate.go` — `Document.Validate` and helpers.
- `duration.go` — YAML-friendly `Duration` type.
- `lookup.go` — `FindHub`, `FindAgent`, `AgentsForHub`, `HasGroup`.

## YAML envelope

The schema follows a Kubernetes-style envelope:

```yaml
apiVersion: seine.io/v1
kind: Network
metadata:
  name: corp-overlay
  description: optional
spec:
  cidr: 100.64.0.0/10
  wireguard:
    mtu: 1420
    persistentKeepalive: 25s
    hubListenPort: 51820
  hubs:
    - { name: hub-eu, endpoint: hub-eu.example.com:51820, tunnelIP: 100.64.0.1 }
    - { name: hub-us, endpoint: hub-us.example.com:51820, tunnelIP: 100.64.0.2 }
  agents:
    - { name: dev-laptop,  tunnelIP: 100.64.1.10, hub: hub-eu, groups: [dev] }
    - { name: prod-server, tunnelIP: 100.64.1.20, hub: hub-us, groups: [prod] }
  groups: [dev, prod]
  acls:
    - { from: [dev], to: [dev], action: allow }
```

## Types

### `Document`

Top-level object. Fields:

| Field | Type | Notes |
| --- | --- | --- |
| `APIVersion` | `string` | Must equal the `APIVersion` constant (`seine.io/v1`). |
| `Kind` | `string` | Must equal the `Kind` constant (`Network`). |
| `Metadata` | `Metadata` | `Name` (DNS-1123 label), optional `Description`. |
| `Spec` | `Network` | The substantive body. |

### `Network`

| Field | Type | Notes |
| --- | --- | --- |
| `CIDR` | `string` | Overlay subnet; every tunnel IP must fall inside it. |
| `WireGuard` | `WireGuard` | Tunnel-level tuning; defaults applied. |
| `Hubs` | `[]Hub` | At least one is required. |
| `Agents` | `[]Agent` | Spokes; may be empty. |
| `Groups` | `[]string` | Universe of group names referenced elsewhere. |
| `ACLs` | `[]ACL` | Group-to-group rules; default policy is deny. |

### `WireGuard`

| Field | Type | Default | Notes |
| --- | --- | --- | --- |
| `MTU` | `int` | `1420` | Validated to `[1280, 1500]`. |
| `PersistentKeepalive` | `Duration` | `25s` | Used by spokes toward their hub. |
| `HubListenPort` | `int` | `51820` | Network-wide default; per-hub overrides via `Hub.Endpoint`. |

### `Hub`, `Agent`, `ACL`

```go
type Hub struct {
    Name     string  // DNS-1123 label, unique across hubs and agents
    Endpoint string  // host:port (host may be DNS or IP)
    TunnelIP string  // address inside Network.CIDR, unique
}

type Agent struct {
    Name     string   // DNS-1123 label, unique
    TunnelIP string   // inside Network.CIDR, unique
    Hub      string   // must match a declared Hub.Name
    Groups   []string // each must be declared in Network.Groups
}

type ACL struct {
    From   []string // group names; must be declared
    To     []string // group names; must be declared
    Action Action   // ActionAllow or ActionDeny
}
```

### `Action`

```go
type Action string

const (
    ActionAllow Action = "allow"
    ActionDeny  Action = "deny"
)
```

## Public API

### Constants

```go
const APIVersion                  = "seine.io/v1"
const Kind                        = "Network"
const DefaultMTU                  = 1420
const DefaultPersistentKeepalive  = 25 * time.Second
const DefaultHubListenPort        = 51820
```

### `func Parse(data []byte) (*Document, error)`

Decodes a YAML byte slice into a `Document`, applies defaults, then
validates. The decoder is configured with `KnownFields(true)`, so any
unknown field is a parse error — this catches typos in the operator's
YAML at submit time.

Returns `*Document` only on success; on failure the caller receives a
parse error or a joined validation error (see `Validate`).

### `func LoadFile(path string) (*Document, error)`

Reads the file at `path` and forwards to `Parse`.

### `func Marshal(d *Document) ([]byte, error)`

Renders a `Document` back to YAML with two-space indentation. Used by
tests and round-trip tooling.

### `(*Document).Validate() error`

Validates structural and cross-reference invariants:

- `apiVersion` and `kind` match the package constants.
- `metadata.name` is a DNS-1123 label.
- `spec.cidr` is a valid `netip.Prefix`.
- `spec.wireguard.mtu` ∈ `[1280, 1500]`.
- `spec.wireguard.hubListenPort` ∈ `[1, 65535]`.
- Group names are unique DNS-1123 labels.
- Hub names and agent names are DNS-1123 labels and globally unique
  across hubs *and* agents (so an agent cannot reference a hub by an
  ambiguous name).
- Tunnel IPs parse, fall inside `spec.cidr` and are unique across
  hubs and agents.
- Each `Agent.Hub` references a declared hub; each `Agent.Groups`
  entry references a declared group; agent group lists have no
  duplicates.
- Each `ACL.Action` is `allow` or `deny`; `From` and `To` are
  non-empty and reference declared groups.
- `spec.hubs` is non-empty.

Multiple problems are joined with `errors.Join`, so the operator sees
all issues from a single parse attempt rather than fixing them one at
a time.

### `(*Document).ApplyDefaults()`

Idempotent. Fills:

- `APIVersion` and `Kind` if empty.
- `Spec.WireGuard.MTU`, `PersistentKeepalive`, `HubListenPort` if zero.

### Lookups

```go
func (d *Document) FindHub(name string) *Hub
func (d *Document) FindAgent(name string) *Agent
func (d *Document) AgentsForHub(hubName string) []*Agent
func (d *Document) HasGroup(name string) bool
```

The returned pointers alias into the `Document`'s slices and must not
be mutated; specs are intended to be immutable after `Parse`.

### `Duration`

`type Duration time.Duration` with custom `UnmarshalYAML` /
`MarshalYAML`. Accepts both the human form (`"25s"`, `"1m30s"`) and
plain numbers (interpreted as seconds). `Std()` returns the underlying
`time.Duration`.

## Naming conventions enforced

DNS-1123 label regex: `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`. This is
the same constraint Kubernetes uses for object names and is strict
enough to be safe inside iptables / netlink / shell substitution.

## Concurrency contract

`Document` instances are immutable after `Parse`. Multiple goroutines
may call lookup methods concurrently. Mutation (for example to apply a
hot-fix) is not part of the contract; reload from Git instead.

## Used by

- `specsource` — calls `Parse` on every successful Git pull.
- `controlplane` — calls `FindHub` / `FindAgent` to validate Register.
- `topology` — central consumer; reads hubs, agents and groups.
- `netpolicy` — reads agents, hubs and ACLs to compile firewall rules.
