# Package `internal/controlplane`

Implementation of the management plane gRPC service. Composed of three
files; each owns a small surface.

## Files

- `server.go` — `Server`, `Config`, `NewServer`, `AttachTo`,
  `SkipAuthMethods`, and the `Register` / `Heartbeat` handlers.
- `auth.go` — bearer-token generation, `AuthInterceptor`,
  `AgentFromContext`, and metadata parsing.
- `spec.go` — `SpecProvider` interface and small spec lookups
  (`resolveRole`, `tunnelIPFor`).

## API surface (proto)

The wire contract is defined in
`api/proto/seine/controlplane/v1/controlplane.proto`. The generated Go
types are imported as the `cpv1` alias. The two RPCs:

```proto
service ControlPlane {
  rpc Register(RegisterRequest) returns (RegisterResponse);
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);
}

message RegisterRequest {
  string name             = 1;
  string bootstrap_token  = 2;
  string public_key       = 3;
  string endpoint         = 4;
  string agent_version    = 5;
}

message RegisterResponse {
  string auth_token = 1;
}

message HeartbeatRequest {
  string endpoint = 1;
}

message HeartbeatResponse {
  string spec_commit         = 1;
  repeated PeerInfo peers    = 2;
}

message PeerInfo {
  string                    name        = 1;
  string                    public_key  = 2;
  string                    endpoint    = 3;
  string                    tunnel_ip   = 4;
  google.protobuf.Timestamp last_seen   = 5;
  Role                      role        = 6;
}

enum Role { ROLE_UNSPECIFIED = 0; ROLE_HUB = 1; ROLE_SPOKE = 2; }
```

## Public API (Go)

### `type Config`

```go
type Config struct {
    Store          *store.Store
    Spec           SpecProvider
    BootstrapToken string
}
```

All three fields are required.

### `type SpecProvider`

```go
type SpecProvider interface {
    Current() (doc *spec.Document, commitSHA string, err error)
}
```

Implemented by `specsource.Watcher`. Implementations must be safe for
concurrent use.

### `type Server`

Implements `cpv1.ControlPlaneServer`. Holds a `Config`. Stateless
beyond that; all persistent state lives in the embedded `*store.Store`.

### `func NewServer(cfg Config) (*Server, error)`

Validates the three required fields and returns a `*Server`. Does not
start any listener.

### `func (s *Server) AttachTo(srv *grpc.Server)`

Registers the gRPC handlers onto an existing `*grpc.Server`. Decoupling
construction from listener setup makes the package usable from
`bufconn`-based tests without changes.

### `func SkipAuthMethods() map[string]struct{}`

Returns the set of fully-qualified method names that the auth
interceptor should not challenge. The set contains
`/seine.controlplane.v1.ControlPlane/Register` because the agent has
no bearer token until that call returns.

## Handlers

### `Register`

Validates the request and enrols an agent.

```
input checks:
    Name                  non-empty
    PublicKey             non-empty
    BootstrapToken        constant-time-equal to cfg.BootstrapToken
                          → Unauthenticated otherwise

spec checks:
    cfg.Spec.Current()  must be available  → Unavailable
    Name must be declared in the spec      → PermissionDenied

side effects:
    if role == HUB and Endpoint == "":
        endpoint ← spec hub's endpoint   (orchestrator-friendly default)
    id        ← random 16-byte hex
    tok       ← random 32-byte URL-safe base64
    store.UpsertAgent(Agent{
        ID, Name, PublicKey, Endpoint,
        AuthTokenHash = SHA256(tok),
        CreatedAt = now, LastSeenAt = now,
    })

response:
    RegisterResponse{ AuthToken: tok }
```

The agent persists `tok` to disk (`<state-dir>/auth.token`) and uses
it as the bearer for subsequent calls. Re-registering is destructive
in the store: the old bearer token is invalidated.

### `Heartbeat`

Refreshes the caller's runtime state and returns the peer registry.

```
auth: AgentFromContext(ctx) must be non-nil
      (set by AuthInterceptor; otherwise Unauthenticated)

inputs: HeartbeatRequest{ Endpoint string }
        empty endpoint preserves the previously stored value

side effects:
    cfg.Spec.Current()  → doc, commit
    store.UpdateHeartbeat(caller.ID, Endpoint, now)
    all = store.ListAgents()

response:
    spec_commit = commit
    for each registered agent a:
        role = resolveRole(doc, a.Name)
        if role == ROLE_UNSPECIFIED:    # name removed from spec
            skip                          # stale registration
        endpoint = a.Endpoint
        if a.ID == caller.ID and Endpoint != "":
            endpoint = Endpoint           # reflect the just-updated value
        peers ← PeerInfo{
            name, public_key, endpoint,
            tunnel_ip = tunnelIPFor(doc, name),
            last_seen, role,
        }
```

Stale registrations (entries whose name is no longer in the spec) are
filtered out of the response. They remain in the database — the
operator is expected to garbage-collect them out of band.

## Authentication

### `AuthInterceptor`

```go
func AuthInterceptor(s *store.Store, skip map[string]struct{}) grpc.UnaryServerInterceptor
```

A unary server interceptor. For every RPC whose `info.FullMethod` is
not in `skip`, it:

1. Extracts the bearer token from gRPC metadata (see below).
2. Computes `store.HashToken(token)` and looks the agent up via
   `store.AuthenticateByTokenHash`.
3. On a `store.ErrNotFound` returns `Unauthenticated`; on any other
   error returns `Internal`.
4. On success attaches `*store.Agent` to the context and calls the
   handler.

### Metadata parsing

```
authorization: Bearer <token>     (canonical; case-insensitive prefix)
x-seine-token: <token>            (fallback for tooling)
```

If neither header is present the interceptor returns
`Unauthenticated`.

### Token format

- 32 random bytes from `crypto/rand`, encoded with
  `base64.RawURLEncoding`. Result is ~43 characters.
- Server stores only the SHA-256 digest. The plain token leaves the
  server exactly once, in a `RegisterResponse`.

### Bootstrap token

Compared with `subtle.ConstantTimeCompare` to keep the comparison
side-channel-free. There is exactly one bootstrap token per server
process; rotating it requires a rolling restart and a new operator
push of the value to agents that have not yet enrolled.

### `AgentFromContext`

```go
func AgentFromContext(ctx context.Context) *store.Agent
```

Returns the authenticated agent, or `nil` if no auth has been performed
on this context. Handlers that require auth should treat `nil` as
`Unauthenticated`.

## Spec helpers (`spec.go`)

Two unexported utilities used by the handlers:

- `resolveRole(doc, name)` — returns `(role, *spec.Hub, *spec.Agent)`.
  Role is `ROLE_HUB`, `ROLE_SPOKE` or `ROLE_UNSPECIFIED`.
- `tunnelIPFor(doc, name)` — returns the spec-declared overlay IP of
  the named hub or agent, or `""`.

Both are nil-safe with respect to `doc`.

## Failure-mode summary

| Condition | gRPC code | Where |
| --- | --- | --- |
| Empty name or public key | `InvalidArgument` | `Register` |
| Mismatched bootstrap token | `Unauthenticated` | `Register` |
| Spec not yet loaded | `Unavailable` | `Register`, `Heartbeat` |
| Name not declared | `PermissionDenied` | `Register` |
| Internal RNG / DB failure | `Internal` | both |
| Missing or invalid bearer token | `Unauthenticated` | interceptor |
| Auth context absent at handler | `Unauthenticated` | `Heartbeat` |

## Used by

- `cmd/seine-server` — composes server, attaches to `grpc.Server`,
  installs `AuthInterceptor`.
- `agentcore` — the only client; uses `cpv1.ControlPlaneClient` over a
  `grpc.NewClient` connection.
