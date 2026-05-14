# Package `internal/store`

SQLite-backed registry of agent runtime state for the management
server. Holds a single first-class entity, `Agent`, and a small set of
operations the gRPC handlers need.

## Files

- `store.go` — sole file; types, schema migration and CRUD.

## Why SQLite

The management server needs durable, structured storage for a small
number of rows (one per registered agent) with predictable read/write
patterns. SQLite gives:

- zero-configuration deployment — the database is a file,
- transactional safety,
- and pure-Go embedding via `modernc.org/sqlite`, which keeps the
  build cgo-free and the container image slim.

A heavier database would not unlock any feature the system needs.

## What the store does *not* hold

The declarative network spec is **not** stored here. It lives in Git
and is loaded into memory by `specsource.Watcher`. Splitting "intent"
(spec) from "facts" (registry) means the database never goes stale
relative to operator edits — the server picks up YAML changes on the
next pull.

The agent's role (`hub` vs `spoke`) is also not persisted: it is
recomputed from the spec on every request, so re-assigning an agent in
YAML does not require operator intervention against the database.

## Public API

### `var ErrNotFound`

Returned when a lookup matches no row. Sentinel; callers compare via
`errors.Is`.

### `type Agent`

```go
type Agent struct {
    ID            string    // opaque random identifier (16 random bytes hex-encoded)
    Name          string    // unique, must match a name declared in the spec
    PublicKey     string    // WireGuard public key, base64 (wgctrl form)
    Endpoint      string    // last advertised host:port; empty for spokes
    AuthTokenHash []byte    // SHA-256 of the bearer token (the plain token never leaves the agent)
    CreatedAt     time.Time // first Register
    LastSeenAt    time.Time // last successful Heartbeat
}
```

The plain bearer token is given to the agent only once — at Register
— and never persisted server-side. Authentication compares the SHA-256
digest.

### `type Store`

Wraps `*sql.DB`. The connection pool is capped at one open connection
because SQLite serialises writers, and the server's request volume
makes contention a non-issue at this size.

### `func Open(ctx context.Context, path string) (*Store, error)`

Opens the database at `path` (or `":memory:"`), pings it, and runs
`migrate` which is idempotent. Use a real file path in production and
`":memory:"` in tests.

### `func (s *Store) Close() error`

Closes the underlying `*sql.DB`.

### `func HashToken(token string) []byte`

Returns the SHA-256 digest of `token` as a 32-byte slice. Use this
both when storing a token (`Agent.AuthTokenHash`) and when verifying
an inbound bearer token. The function is intentionally not exported as
a method on `Store` because it has no dependency on database state.

### `func (s *Store) UpsertAgent(ctx, Agent) error`

Inserts a new row or updates the existing one keyed by `Name`. On
update the `id`, `public_key`, `endpoint`, `auth_token_hash` and
`last_seen_at` fields are overwritten — meaning a re-register replaces
the agent's identifier and invalidates the old bearer token. Useful
for the recovery path where an agent loses its keypair.

If `CreatedAt` is the zero value the function fills it with `time.Now`.

### `func (s *Store) GetAgentByName(ctx, name) (*Agent, error)`

Returns `ErrNotFound` if no row matches.

### `func (s *Store) AuthenticateByTokenHash(ctx, hash) (*Agent, error)`

Returns the agent whose `auth_token_hash` equals `hash`. Used by
`controlplane.AuthInterceptor`. Returns `ErrNotFound` for a wrong or
missing token.

### `func (s *Store) UpdateHeartbeat(ctx, id, endpoint string, when time.Time) error`

Updates `last_seen_at` to `when`. If `endpoint` is non-empty it is
also updated; an empty `endpoint` preserves the previous value (this
matches the gRPC contract where an empty `Heartbeat.Endpoint` means
"unchanged").

### `func (s *Store) ListAgents(ctx) ([]*Agent, error)`

Returns all rows ordered by `Name`. Used by `Heartbeat` to construct
the peer registry returned to the caller.

## Schema

```sql
CREATE TABLE agents (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    public_key      TEXT NOT NULL,
    endpoint        TEXT NOT NULL DEFAULT '',
    auth_token_hash BLOB NOT NULL,
    created_at      INTEGER NOT NULL,         -- Unix seconds, UTC
    last_seen_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_agents_token ON agents(auth_token_hash);
```

`migrate` runs on every `Open` with `IF NOT EXISTS`, so a brand new
file becomes a usable database without any one-shot bootstrap step.

The index on `auth_token_hash` accelerates the per-request lookup that
`AuthInterceptor` performs.

## Time handling

All timestamps are stored as Unix seconds in UTC. `scanAgent` converts
back to `time.Time` with `Location` set to `UTC` so callers do not
have to think about time zones.

## Concurrency

`*Store` is safe for concurrent use by multiple goroutines: SQLite
serialises writers internally, and the connection pool is sized at
one. Reads and writes from different goroutines are properly
sequenced.

## Used by

- `cmd/seine-server` — opens the store and passes it to
  `controlplane.NewServer` and `controlplane.AuthInterceptor`.
- `controlplane` — only direct consumer.
