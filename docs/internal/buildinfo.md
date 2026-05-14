# Package `internal/buildinfo`

Build-time metadata exposed as package-level variables. Stamped via
`-ldflags "-X ..."` during release builds; useful values are kept as
fallbacks for plain `go build`.

## File

- `buildinfo.go` — sole file.

## Public API

### Variables

```go
var (
    Version   = "dev"
    Commit    = "unknown"
    BuildDate = "unknown"
)
```

These are *vars* on purpose so the linker can override them. The
project's `Makefile` injects them with:

```
-ldflags "-X github.com/ilyapiatykh/seine/internal/buildinfo.Version=$(VERSION) \
          -X github.com/ilyapiatykh/seine/internal/buildinfo.Commit=$(COMMIT)  \
          -X github.com/ilyapiatykh/seine/internal/buildinfo.BuildDate=$(BUILD_DATE)"
```

`VERSION` is `git describe --tags --always --dirty`, `COMMIT` is the
short SHA, `BUILD_DATE` is `date -u +%Y-%m-%dT%H:%M:%SZ`.

### `func Short() string`

Returns `"<version> (<short-commit>)"` for use in startup logs and
`--version` output. The commit is truncated to seven characters.

### `func GoModule() string`

Returns the module path the binary was compiled against, read from
`runtime/debug.ReadBuildInfo`. Empty string if build info is
unavailable (which never happens in normal Go builds).

## Used by

- `cmd/seine-server` and `cmd/seine-agent` for the `--version` flag
  and the "starting" log line.
- `internal/agentcore` to stamp `RegisterRequest.AgentVersion` so the
  server can record which agent build registered.
- `cmd/seine-server` and `cmd/seine-agent` to populate
  `otel.Config.ServiceVersion`.
