# Architecture

This document maps the packages described in the per-package references
to the runtime processes they form and to the data flow that binds them
together. It is written for a reviewer who has read the introduction and
the literature review of the thesis and now wants to understand how the
implementation realises those design decisions.

## Processes and packages

A seine deployment is composed of two long-lived process types and a
Git repository.

```
   Git repository (network.yaml)
         │
         │  pulled by …
         │
         ├────────────────────────────────────┐
         ▼                                    ▼
 ┌───────────────────┐            ┌─────────────────────┐
 │   seine-server    │ ◄─ gRPC ─► │     seine-agent     │
 │ (management plane)│            │    (data plane)     │
 │                   │            │                     │
 │ buildinfo         │            │ buildinfo           │
 │ logging           │            │ logging             │
 │ otel              │            │ otel                │
 │ gitsource         │            │ gitsource           │
 │ specsource ◄──┐   │            │ specsource ◄──┐     │
 │ spec ─────────┘   │            │ spec ─────────┘     │
 │ store (SQLite)    │            │ wg (Linux kernel)   │
 │ controlplane      │            │ topology            │
 │                   │            │ netpolicy (hubs)    │
 │                   │            │ agentcore           │
 └───────────────────┘            └─────────────────────┘
```

Both processes share the cross-cutting packages (`buildinfo`, `logging`,
`otel`) and the GitOps stack (`gitsource`, `specsource`, `spec`).
Beyond that, each process composes a different subset:

- The server combines a SQLite-backed runtime registry (`store`) with a
  gRPC service (`controlplane`).
- The agent owns the local WireGuard interface (`wg`), uses
  `topology` to compute its desired peer set, applies hub-side
  policies through `netpolicy` (only when running as a hub), and is
  glued together by `agentcore`.

## The two source-of-truth surfaces

The system carefully separates two kinds of state:

1. **Desired state ("the spec")**. The declarative `Document` parsed
   from `network.yaml` on the tracked branch of the Git repository.
   Hubs, agents, groups and ACL rules live here. The spec is the
   authoritative source of intent. Both the server and every agent
   pull it independently through `specsource`.

2. **Runtime state ("the registry")**. Public keys, currently
   advertised endpoints and last-heartbeat timestamps. The server
   persists these in SQLite via `store`; agents publish them through
   gRPC `Register` and `Heartbeat` calls. They are not in the spec
   because they are not declarative — they change as agents come up,
   restart or move.

This split is what justifies the gRPC API surface. The spec alone is
not enough to bring up WireGuard tunnels because spokes do not know
each other's public keys until they have generated them; conversely,
the registry alone cannot describe network policy because there is no
operator-authored intent in it. Both inputs are joined inside the
agent's `topology.PeersFor`.

## Lifecycle of a `seine-agent`

The diagram below summarises one agent's main-loop iteration. Numbers
correspond to the steps explained underneath.

```
                ┌─────────────────────────┐
                │    process startup      │
                └───────────┬─────────────┘
                            ▼
   (1)  os.MkdirAll(stateDir)
   (2)  wg.LoadOrGenerate(stateDir/private.key)
   (3)  grpc.NewClient(--control-plane)
   (4)  loadOrRegister
                            │
        ┌───────────────────┴───────────────────┐
        │ token file present?                   │
        │   yes → use it                        │
        │   no  → cp.Register(BootstrapToken)   │
        │         persist returned auth token   │
        └───────────────────┬───────────────────┘
                            ▼
   (5)  waitForFirstSpec  (specsource has at least one snapshot)
   (6)  topology.FindSelf(doc, name)
   (7)  wg.New(iface)  → wg.Up
   (8)  if hub: netpolicy.EnsureIPForwarding + NewFirewall
                            │
                            ▼
   (9)  reconcile loop  ──────────  every reconcileInterval OR
        │                          on specsource.Updates()
        │
        ├ heartbeat       cp.Heartbeat(authToken, advertisedEndpoint)
        │                 ↳ recover from Unauthenticated by re-register
        │
        ├ desired peers   topology.PeersFor(doc, self, resp.Peers, ka)
        │
        ├ apply           wg.Interface.ApplyPeers(desired) → Diff
        │
        └ if hub: ACL     netpolicy.Compile(doc) → Firewall.Reconcile
                            │
                            ▼ (ctx cancelled)
   (10) wg.Interface.Down
        if hub: Firewall.Teardown
        otelShutdown
```

Each numbered step is implemented in `internal/agentcore`. The choice
of which package owns each piece keeps the loop testable: the
reconciliation logic depends on the `wg.Interface`, `netpolicy.Firewall`
and `cpv1.ControlPlaneClient` only through their interfaces, and any
of them can be substituted in unit tests.

## Lifecycle of a `seine-server`

The server is simpler. It has no reconciliation loop of its own.

```
   (1) parse flags, set up logging and OpenTelemetry
   (2) store.Open(dbPath)
   (3) specsource.New(gitsource.Config{...})
       go specsource.Watcher.Run(ctx)
   (4) controlplane.NewServer(store, watcher, bootstrapToken)
   (5) grpc.NewServer with:
         - otelgrpc.NewServerHandler  (auto-tracing)
         - controlplane.AuthInterceptor (bearer-token auth)
       attach the controlplane service to it
   (6) net.Listen("tcp", listenAddr) and srv.Serve
   (7) on ctx.Done: srv.GracefulStop, watcher.Close, store.Close, otelShutdown
```

The server does not write to any peer's filesystem or kernel state. Its
job is to:

- Authenticate Register calls against the bootstrap token and the spec.
- Issue per-agent bearer tokens and persist their hashes.
- Update last-seen timestamps and (optionally) endpoints from
  Heartbeat.
- Return the latest peer registry so agents can finish reconciling.

## Data-flow walkthrough

The end-to-end flow that the demo exercises is:

1. **Operator commits to Git**: a YAML edit lands on the tracked
   branch.
2. **specsource pulls**: in both the server and every agent, the
   `specsource.Watcher` notices the new commit on its next poll and
   updates its in-memory snapshot. A signal is delivered on
   `Watcher.Updates()`.
3. **Agent re-reconciles early**: the reconciliation loop wakes up on
   the update signal instead of waiting for the next tick.
4. **Heartbeat**: the agent calls `cp.Heartbeat`. The server validates
   the bearer token, updates last-seen, and returns the latest peer
   registry built from `store.ListAgents` plus the spec.
5. **Compose desired state**: the agent calls `topology.PeersFor` with
   the latest spec and the registry. For a spoke this returns its hub
   only; for a hub it returns the hub mesh and its assigned spokes.
6. **Apply WG**: `wg.Interface.ApplyPeers` diffs desired against the
   current kernel state and issues a single `wgctrl.ConfigureDevice`
   carrying the additions, updates and removals.
7. **Apply ACL (hub only)**: `netpolicy.Compile` expands group rules
   into per-IP-pair allow/deny decisions; `Firewall.Reconcile` reflects
   them into the `SEINE-FWD` iptables chain.

If a step fails the rest of the cycle still runs where it makes sense
(for example a transient gRPC failure aborts the cycle but the local
WG state and iptables are left intact). The next tick retries.

## How the design serves the thesis goals

| Thesis goal | Where it is realised |
| --- | --- |
| Network as Code | `spec` defines the schema; `gitsource`/`specsource` make Git authoritative. |
| Pull-based GitOps agent | `agentcore` pulls Git directly; `specsource.Watcher` is the cache. |
| Reconciliation loop | `agentcore.reconcileLoop` + `wg.computeDiff`. |
| Multi-hub topology | `topology.hubPeers` builds the mesh; spokes route through their hub. |
| Declarative ACL | `spec.ACL` + `netpolicy.Compile` + `netpolicy.Firewall`. |
| Cloud-agnostic deployment | All processes are Go binaries with no cloud-specific dependencies. |
| OpenTelemetry observability | `otel.Setup` exports OTLP traces, metrics and logs; gRPC is auto-instrumented. |
| WireGuard performance | Linux kernel WireGuard via `wgctrl-go` and `vishvananda/netlink`. |

## Boundary and out-of-scope notes

- **NAT traversal**: not implemented. Spokes initiate to their hub; the
  hub's public endpoint is the only NAT hole-punching avenue. Inter-
  spoke flows always transit a hub.
- **mTLS**: agent ↔ server gRPC is plaintext in the demo. The codebase
  does not preclude TLS — `grpc.NewServer` accepts credentials
  options — it is simply not configured in `cmd/seine-server`.
- **Userspace WireGuard**: the `wg` package is shaped for a second
  backend (`wireguard-go`), but only the Linux kernel backend is
  wired up.
