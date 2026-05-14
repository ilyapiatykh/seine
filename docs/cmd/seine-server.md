# Command `seine-server`

`cmd/seine-server` is the entry point of the management plane process.
It is a thin orchestrator: every non-trivial concern is delegated to a
package under `internal/`. The binary's job is to parse configuration,
construct the dependency graph in the right order, hand control to the
gRPC server and coordinate graceful shutdown.

## Source

A single file: `cmd/seine-server/main.go`.

## What it composes

In order of construction:

1. **Logging** — `logging.Setup` installs the project-wide `log/slog`
   handler. The level and format come from `--log-level` and
   `--log-format`.
2. **Process context** — `signal.NotifyContext` is wired to
   `SIGINT`/`SIGTERM`, then enriched with the structured logger via
   `logging.WithLogger` so downstream code can call
   `logging.FromContext`.
3. **OpenTelemetry** — `otel.Setup` configures OTLP/gRPC exporters or
   no-op providers. The returned `ShutdownFunc` is `defer`red.
4. **Persistence** — `store.Open(dbPath)` opens (and migrates) the
   SQLite database holding the agent registry.
5. **Spec source** — `specsource.New(gitsource.Config{...})` opens the
   Git working directory and prepares the poller. `Watcher.Run` is
   started in a goroutine to begin pulling.
6. **Service** — `controlplane.NewServer` ties the store, the spec
   provider and the bootstrap token into a gRPC service implementation.
7. **gRPC transport** — `grpc.NewServer` is created with two stats /
   interceptor hooks:
   - `otelgrpc.NewServerHandler` for auto-tracing of every RPC.
   - `controlplane.AuthInterceptor` for bearer-token auth, with
     `controlplane.SkipAuthMethods()` exempting `Register`.
   The service is attached via `cps.AttachTo(srv)`.
8. **Listener** — `net.Listen("tcp", listenAddr)` opens the configured
   bind address. `srv.Serve` runs synchronously until the listener
   closes.
9. **Shutdown coordinator** — a goroutine waits for `ctx.Done()` and
   calls `srv.GracefulStop`. The deferred `watcher.Close`, `st.Close`
   and `otelShutdown` calls run on return.

## Configuration

All flags accept a paired environment variable. The list below is
authoritative; refer to the Go file for default values that are not
documented here.

### Logging

| Flag | Env | Default | Notes |
| --- | --- | --- | --- |
| `--log-level` | `SEINE_LOG_LEVEL` | `info` | `debug` enables source attribution. |
| `--log-format` | `SEINE_LOG_FORMAT` | `text` | `json` for log aggregators. |

### gRPC and persistence

| Flag | Env | Default | Notes |
| --- | --- | --- | --- |
| `--listen` | `SEINE_LISTEN` | `:8443` | Bind address for gRPC. |
| `--db` | `SEINE_DB` | `seine.db` | SQLite database file path. |
| `--bootstrap-token` | `SEINE_BOOTSTRAP_TOKEN` | _(required)_ | Compared against `RegisterRequest.BootstrapToken`. |

### Git source

| Flag | Env | Default | Notes |
| --- | --- | --- | --- |
| `--git-url` | `SEINE_GIT_URL` | _(required)_ | `https://`, `ssh://` or `git://`. |
| `--git-branch` | `SEINE_GIT_BRANCH` | `main` | |
| `--git-path` | `SEINE_GIT_PATH` | `network.yaml` | Path inside the repo. |
| `--git-workdir` | `SEINE_GIT_WORKDIR` | _(tempdir)_ | Local cache; tempdir if empty. |
| `--git-interval` | `SEINE_GIT_INTERVAL` | `30s` | Poll interval. |
| `--git-token` | `SEINE_GIT_TOKEN` | _(empty)_ | HTTPS token; chosen first if set. |
| `--git-username` | `SEINE_GIT_USERNAME` | `git` | Paired with `--git-token`. |
| `--git-ssh-key` | `SEINE_GIT_SSH_KEY` | _(empty)_ | OpenSSH key path; used when no token. |

`buildGitAuth` selects the first matching auth: token → SSH key →
`gitsource.NoAuth`.

### OpenTelemetry

| Flag | Env | Default | Notes |
| --- | --- | --- | --- |
| `--otlp-endpoint` | `SEINE_OTLP_ENDPOINT` | _(empty)_ | Empty disables exporters. |
| `--otlp-insecure` | `SEINE_OTLP_INSECURE` | `true` | Skips TLS for OTLP/gRPC. |

### Diagnostics

| Flag | Effect |
| --- | --- |
| `--version` | Prints `seine-server <version> (<commit>)` and exits. |

## Required flags and their failure modes

If `--bootstrap-token` is empty `run` returns
`"--bootstrap-token (or SEINE_BOOTSTRAP_TOKEN) is required"`. If
`--git-url` is empty it returns the analogous message. Both errors
exit 1 via `main`. There is no support for reading either value from
a Kubernetes Secret or HashiCorp Vault — these are configured at the
orchestrator level.

## Run-time behaviour

- The spec poller tolerates remote failures with exponential backoff
  (`specsource` defaults: 2 s → 60 s).
- gRPC requests are auto-traced and bound to a per-call slog logger
  through `logging.WithLogger`.
- Every authenticated handler invocation has the relevant
  `*store.Agent` available via `controlplane.AgentFromContext`.

## Logs you can expect at startup

```
INFO starting seine-server version="dev (unknown)" listen=:8443 db=seine.db git_url=…
INFO cloning git source url=…  branch=main  workdir=…    (specsource)
INFO spec updated commit=<sha> from=""                   (specsource on first pull)
INFO gRPC server ready addr=[::]:8443
INFO agent registered name=… role=ROLE_HUB|ROLE_SPOKE    (one per Register)
```

On shutdown:

```
INFO shutdown signal received, stopping gRPC server
INFO seine-server stopped
```

## See also

- [internal/controlplane](../internal/controlplane.md) — service implementation.
- [internal/store](../internal/store.md) — persistence layer.
- [internal/specsource](../internal/specsource.md) — Git poller.
- [architecture.md](../architecture.md) — server lifecycle in context.
