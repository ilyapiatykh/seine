# Package `internal/logging`

Project-wide structured logging configured around the standard
library's `log/slog`. Every binary calls `Setup` exactly once at
process start and propagates a per-request logger through `context`.

## File

- `logging.go` — sole file.

## Why a wrapper

`slog` is the source of truth for the project's log records. The
wrapper exists for three small reasons:

1. **Choice of handler from configuration.** `text` for humans, `json`
   for log aggregators. The choice comes from a flag.
2. **Per-context logger.** A handler attached to the global
   `slog.Default()` is fine for top-level code but inconvenient when a
   request needs additional fields (e.g. `rpc=Heartbeat`,
   `agent=hub-eu`). `WithLogger` / `FromContext` thread a child logger
   through `context`.
3. **A single place to add OTel.** The OpenTelemetry slog bridge can
   later be installed alongside the configured handler without
   touching every call site.

## Public API

### `type Format`

```go
type Format string

const (
    FormatText Format = "text"
    FormatJSON Format = "json"
)
```

### `type Options`

```go
type Options struct {
    Level  slog.Level
    Format Format
    Output io.Writer  // default os.Stderr
}
```

### `func Setup(opts Options) *slog.Logger`

Builds a `slog.Handler` for the requested format and level, wraps it
in a `*slog.Logger`, and installs it as `slog.Default`. `AddSource` is
turned on automatically for `LevelDebug` so debug records carry the
file:line they were emitted from.

The returned logger is the same instance accessible via
`slog.Default()`; both binaries hold a local reference to it for
convenience.

### `func ParseLevel(s string) (slog.Level, error)`

Maps `"debug" | "info" | "warn" | "warning" | "error"` (case- and
whitespace-tolerant) to the matching `slog.Level`. The empty string
defaults to `info`. Anything else is an error.

### `func ParseFormat(s string) (Format, error)`

Maps `"text" | "json"` likewise; empty string defaults to `text`.

### `func FromContext(ctx context.Context) *slog.Logger`

Returns a request-scoped logger if one was attached with `WithLogger`,
otherwise `slog.Default()`. Every package that wants a contextual
logger uses this — for example
`logging.FromContext(ctx).With(slog.String("component", "wg"))`.

### `func WithLogger(ctx context.Context, l *slog.Logger) context.Context`

Attaches `l` to a context. The unexported `loggerKey` is private to
this package, so values cannot collide with other libraries' context
keys.

## Conventions in the codebase

- The two binaries' `run` functions call `Setup`, then enrich the root
  context with `WithLogger(ctx, log)` before passing it into any
  long-lived component.
- Packages prefix their records with `slog.String("component", "<pkg>")`
  in their entry-level functions and let downstream calls inherit it
  through `With`.
- Per-call attributes (RPC method, agent name, commit SHA) are added
  near the top of handlers / cycle iterations.

## Used by

- Every package that emits logs.
