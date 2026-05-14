# seine code reference

This directory contains the reference documentation for every Go package
under `cmd/` and `internal/`. The intent is to give a reviewer who has
not seen the codebase enough material to:

- understand the responsibility of each package without reading its
  source first,
- follow the runtime control flow across package boundaries,
- and locate the entry point for any concrete feature mentioned in the
  thesis (GitOps reconciliation, multi-hub mesh, ACL, observability).

## Where to start

| If you want to … | Read |
| --- | --- |
| Get the big picture | [architecture.md](architecture.md) |
| Understand the management plane process | [cmd/seine-server.md](cmd/seine-server.md) |
| Understand the data plane process | [cmd/seine-agent.md](cmd/seine-agent.md) |
| Look up a specific package | the [package index](#package-index) below |

## Package index

### Binaries (`cmd/`)

- [seine-server](cmd/seine-server.md) — management plane process: bootstrap, gRPC server, spec watcher.
- [seine-agent](cmd/seine-agent.md) — data plane process: bootstrap, reconciliation loop driver.

### Libraries (`internal/`)

| Package | One-line summary |
| --- | --- |
| [agentcore](internal/agentcore.md) | Lifecycle of one `seine-agent` process: register, bring up, reconcile. |
| [buildinfo](internal/buildinfo.md) | Build-time metadata stamped via `-ldflags`. |
| [controlplane](internal/controlplane.md) | gRPC service implementation and bearer-token authentication. |
| [gitsource](internal/gitsource.md) | go-git wrapper that yields `(commit SHA, file bytes)` snapshots. |
| [logging](internal/logging.md) | Project-wide `log/slog` setup and per-context propagation. |
| [netpolicy](internal/netpolicy.md) | Hub IP forwarding and ACL via an iptables chain (Linux). |
| [otel](internal/otel.md) | OpenTelemetry providers (traces, metrics, logs) over OTLP/gRPC. |
| [spec](internal/spec.md) | YAML schema, parser and validator for the network spec. |
| [specsource](internal/specsource.md) | Periodic Git pull + parse + cache (provides `SpecProvider`). |
| [store](internal/store.md) | SQLite-backed registry of agent runtime state. |
| [topology](internal/topology.md) | Computes the desired WireGuard peer set per agent. |
| [wg](internal/wg.md) | WireGuard interface abstraction (Linux kernel backend). |

## Reading order suggestion

The packages form a small dependency DAG. A natural reading order that
matches how data flows through a running system is:

1. **[spec](internal/spec.md)** — the declarative source of truth.
2. **[gitsource](internal/gitsource.md)** + **[specsource](internal/specsource.md)** — how the spec gets into a process.
3. **[store](internal/store.md)** — what the management plane persists.
4. **[controlplane](internal/controlplane.md)** — the gRPC server tying spec and store together.
5. **[wg](internal/wg.md)** — the data-plane primitive the agent drives.
6. **[topology](internal/topology.md)** — how spec + registry combine into a desired peer set.
7. **[netpolicy](internal/netpolicy.md)** — how hubs translate ACL rules into iptables.
8. **[agentcore](internal/agentcore.md)** — the agent's lifecycle and reconciliation loop.
9. **[logging](internal/logging.md)**, **[otel](internal/otel.md)**, **[buildinfo](internal/buildinfo.md)** — cross-cutting concerns.

## Conventions used in this documentation

- File paths and Go identifiers are rendered in `monospace`.
- Public Go API is the user-facing surface (capitalised identifiers in
  Go); private helpers are mentioned only when their behaviour leaks
  into a public contract.
- Error contracts are described at the call-site level (what the caller
  must handle) rather than transcribed from individual `fmt.Errorf`
  strings.
- The phrase "the spec" always refers to a `*spec.Document` parsed from
  the YAML committed to the GitOps repository.
