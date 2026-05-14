# seine

A distributed VPN service for SMB networks: a GitOps-style orchestrator
on top of WireGuard tunnels. The declarative network specification lives
in a Git repository; agents pull it and reconcile the local WireGuard
interface against the desired state.

## How it works

```
                  Git repository (network.yaml)
                              │
                              │ pull
              ┌───────────────┴───────────────┐
              ▼                               ▼
      ┌───────────────┐               ┌───────────────┐
      │ seine-server  │ ◄───gRPC──►   │  seine-agent  │
      │ - SQLite      │               │ - WG manage   │
      │ - peer regis- │               │ - reconciler  │
      │   try         │               │ - hub | spoke │
      └───────────────┘               └───────┬───────┘
                                              │
                                       WireGuard tunnels
                                       (overlay 100.64.0.0/10)
```

Three components:

- **seine-server** is the control-plane. It pulls the spec from Git,
  authenticates agent registrations against an operator-issued bootstrap
  token, and serves a small gRPC API (`Register`, `Heartbeat`) backed by
  a SQLite registry of agent runtime state.
- **seine-agent** runs on every node in either `spoke` or `hub` mode.
  It generates a WireGuard keypair, registers with the server, pulls
  the spec from Git itself, and runs a reconciliation loop that
  recomputes the desired peer set on each tick and applies the diff via
  `wgctrl`.
- **WireGuard topology** is hub-and-spoke with a full mesh between
  hubs. Spokes route all overlay traffic through their assigned hub;
  hubs forward between each other. NAT traversal is intentionally out
  of scope — multi-hub forwarding takes its place.

ACL is declarative: groups in YAML and `from → to → allow|deny` rules.
Hubs enforce the policy via a curated `iptables` chain (`SEINE-FWD`)
that intercepts WG-to-WG forwarding.

## Repository layout

```
api/proto/seine/controlplane/v1/   gRPC API (.proto + generated stubs)
cmd/seine-server                   management-plane entrypoint
cmd/seine-agent                    data-plane entrypoint (spoke or hub)
internal/spec                      declarative spec types + validation
internal/gitsource                 go-git wrapper (clone/pull)
internal/specsource                periodic Git-pulling cache
internal/store                     SQLite-backed runtime registry
internal/controlplane              gRPC server + bearer-token auth
internal/wg                        WireGuard interface abstraction
internal/topology                  desired peer-set computation
internal/netpolicy                 hub IP forwarding + ACL via iptables
internal/agentcore                 agent main loop (register/reconcile)
internal/otel                      OpenTelemetry providers (OTLP/gRPC)
internal/logging, internal/buildinfo  small shared utilities
deployments/                       Dockerfiles + docker-compose demo
examples/                          reference and demo network specs
```

## Quick start (demo)

The demo brings up a control plane, a tiny git server seeded with the
spec, two hubs (`hub-eu`, `hub-us`) and two spokes (`spoke-cloud` on
hub-eu, `spoke-office` on hub-us). It demonstrates GitOps reconciliation
and multi-hub forwarding end-to-end on the `100.64.0.0/10` overlay.

Prerequisites: a Linux host (or Docker Desktop with a recent kernel) and
the WireGuard kernel module available on the host.

```bash
make demo-up         # build and start the stack
make demo-status     # docker compose ps
make demo-verify     # ping 100.64.2.10 from spoke-cloud (cloud → office)
make demo-logs       # tail logs of all services
make demo-clean      # tear down and remove volumes
```

The first `make demo-up` build takes a couple of minutes because the
Go binaries are compiled inside the builder image. Subsequent
invocations are fast as long as `go.mod` does not change.

To exercise the GitOps loop, edit a copy of `examples/demo/network.yaml`
and `git push` it into the demo's `gitserver` container — the running
agents pick up the change on their next pull.

## Configuration

Both binaries are configured by command-line flags or matching
`SEINE_*` environment variables. Common flags:

| Flag                     | Env var                  | Default              | Notes                                     |
|--------------------------|--------------------------|----------------------|-------------------------------------------|
| `--log-level`            | `SEINE_LOG_LEVEL`        | `info`               | `debug`, `info`, `warn`, `error`          |
| `--log-format`           | `SEINE_LOG_FORMAT`       | `text`               | `text` or `json`                          |
| `--git-url`              | `SEINE_GIT_URL`          | _(required)_         | `https://`, `ssh://` or `git://`          |
| `--git-branch`           | `SEINE_GIT_BRANCH`       | `main`               | branch tracked                            |
| `--git-path`             | `SEINE_GIT_PATH`         | `network.yaml`       | path inside the repo                      |
| `--git-interval`         | `SEINE_GIT_INTERVAL`     | `30s`                | poll cadence                              |
| `--git-token`            | `SEINE_GIT_TOKEN`        | _(empty)_            | HTTPS token auth                          |
| `--git-ssh-key`          | `SEINE_GIT_SSH_KEY`      | _(empty)_            | SSH private key path                      |
| `--otlp-endpoint`        | `SEINE_OTLP_ENDPOINT`    | _(disabled)_         | OTLP/gRPC collector host:port             |
| `--otlp-insecure`        | `SEINE_OTLP_INSECURE`    | `true`               | skip TLS for OTLP (demo default)          |

Server-only:

| Flag                     | Env var                       | Default              | Notes                                         |
|--------------------------|-------------------------------|----------------------|-----------------------------------------------|
| `--listen`               | `SEINE_LISTEN`                | `:8443`              | gRPC bind address                             |
| `--db`                   | `SEINE_DB`                    | `seine.db`           | SQLite database file                          |
| `--bootstrap-token`      | `SEINE_BOOTSTRAP_TOKEN`       | _(required)_         | shared secret agents present at first call    |

Agent-only:

| Flag                       | Env var                      | Default                       | Notes                                                          |
|----------------------------|------------------------------|-------------------------------|----------------------------------------------------------------|
| `--name`                   | `SEINE_AGENT_NAME`           | _(required)_                 | must match an agent or hub declared in the spec                |
| `--mode`                   | `SEINE_MODE`                 | `spoke`                       | `spoke` or `hub`                                               |
| `--control-plane`          | `SEINE_CONTROL_PLANE`        | _(required)_                 | management server `host:port`                                  |
| `--bootstrap-token`        | `SEINE_BOOTSTRAP_TOKEN`      | _(empty after first run)_    | only consulted on first registration                           |
| `--advertise-endpoint`     | `SEINE_ADVERTISE_ENDPOINT`   | _(required for hubs)_        | publicly reachable `host:port`                                 |
| `--interface`              | `SEINE_INTERFACE`            | `seine0`                      | WireGuard netdev name                                          |
| `--state-dir`              | `SEINE_STATE_DIR`            | `/var/lib/seine/<name>`       | holds the WG private key and auth token                        |
| `--reconcile-interval`     | `SEINE_RECONCILE_INTERVAL`   | `30s`                         | reconcile cadence (also triggered on spec change)              |

## Network spec

```yaml
apiVersion: seine.io/v1
kind: Network
metadata:
  name: corp-overlay
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

See `examples/network.yaml` for the full reference and
`examples/demo/network.yaml` for the demo scenario.

## Development

```bash
make build        # produce ./bin/seine-server and ./bin/seine-agent
make test         # go test -race -count=1 ./...
make vet          # go vet
make fmt-check    # ensure gofmt is clean
make proto        # regenerate gRPC stubs (requires protoc + go plugins)
```

The test suite is cross-platform — the Linux-only WireGuard backend is
behind build tags and is exercised in CI on Linux runners. Cross-
compilation to `linux/amd64` is part of the release flow:

```bash
GOOS=linux GOARCH=amd64 go build ./...
```

## Limitations and future work

- WireGuard backend is Linux kernel only. The package is shaped to admit
  a userspace `wireguard-go` backend for macOS / Windows / FreeBSD
  spokes; this is left as future work.
- NAT traversal is not implemented; spoke-to-spoke flows always transit
  a hub.
- Authentication is a shared bootstrap token plus per-agent opaque
  bearer tokens. mTLS for the gRPC plane is a natural next step.
