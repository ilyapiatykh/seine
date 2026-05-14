# Package `internal/specsource`

Glue layer between [`gitsource`](gitsource.md) and [`spec`](spec.md):
periodically pulls a YAML file from Git, parses and validates it, and
caches the latest successful result. Both binaries embed a `Watcher`.

## Files

- `watcher.go` — sole file; defines `Config`, `Watcher`, and the run
  loop.

## Why this layer exists

`gitsource` returns raw bytes; `spec.Parse` wants a clean document.
The reconciliation loops want a non-blocking "give me the latest doc"
call. `Watcher` does the obvious composition — pull, parse, cache,
expose — and adds:

- exponential backoff on transient failures,
- a single-slot, lossy update channel for "the cached commit changed",
- and the `controlplane.SpecProvider` shape so the management server
  can consume the cache without owning a Git client.

## Public API

### `type Config`

```go
type Config struct {
    Source     gitsource.Config
    Interval   time.Duration // poll cadence on success; default 30s
    MinBackoff time.Duration // initial retry delay; default 2s
    MaxBackoff time.Duration // backoff cap; default 60s
}
```

### `type Watcher`

The cache. Internally holds the latest `*spec.Document`, its commit
SHA, an "as of" timestamp and the most recent pull/parse error.

### `func New(cfg Config) (*Watcher, error)`

Opens the underlying `gitsource.Source` (which validates `cfg.Source`)
and primes the cache with the sentinel error `"specsource: not yet
loaded"`. Does **not** contact the remote — `Run` does.

Defaults are applied here: `Interval`, `MinBackoff` and `MaxBackoff`.

### `func (w *Watcher) Close() error`

Closes the underlying `gitsource.Source` (removing the tempdir if it
was owned). Idempotent.

### `func (w *Watcher) Current() (*spec.Document, string, error)`

Returns the latest snapshot or the most recent error if no pull has
ever succeeded. Read-only; safe for concurrent callers (RWMutex).

This satisfies `controlplane.SpecProvider`.

### `func (w *Watcher) Updates() <-chan struct{}`

Single-slot, non-blocking channel that emits a value every time the
cached commit SHA changes (i.e. either the first successful pull, or
any pull where the new SHA differs from the previous). The channel
buffer is one — the watcher does a non-blocking send, dropping the
event if the receiver has not yet drained the previous one. Receivers
should treat the signal as "something changed since you last looked",
not as a count of events.

### `func (w *Watcher) Run(ctx context.Context) error`

Long-running poll loop:

```
while ctx is alive:
    err = pullOnce(ctx)
    if err == nil:
        backoff = MinBackoff
        delay   = Interval
    else:
        log warning
        delay   = backoff
        backoff = min(backoff*2, MaxBackoff)
    sleep(delay) or return on ctx.Done
```

`pullOnce`:

1. Calls `Source.Pull`.
2. Calls `spec.Parse` on the returned bytes.
3. On success, swaps the cached snapshot under the write lock; if the
   new commit differs from the previous, sends on `Updates` (lossy).
4. On any error, records it as `Watcher.last` so `Current` can return
   it.

Returns `nil` on context cancellation; never returns a non-nil error.

## Concurrency

- All cache reads (`Current`) are protected by an `sync.RWMutex`.
- All writes (success or error) take the write lock.
- `Updates()` returns the same channel for the lifetime of the
  watcher; it is safe to read from any goroutine.

## Failure semantics

- A failed `Pull` does not invalidate the previously cached document;
  consumers continue to see the last good snapshot.
- A failed `spec.Parse` (for example because someone committed an
  invalid YAML) likewise leaves the previous good snapshot in place.
  The error is exposed via `Current` only when no good snapshot has
  ever been cached.
- Backoff is per-watcher, not per-process: a transient outage at
  startup is retried until the watcher is closed or the context is
  cancelled.

## Used by

- `cmd/seine-server` — passes the watcher as `controlplane.Config.Spec`.
- `cmd/seine-agent` — passes the watcher as `agentcore.Config.Spec`;
  the reconciliation loop both reads `Current()` and listens on
  `Updates()` for early ticks.
