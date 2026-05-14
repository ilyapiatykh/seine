# Package `internal/agentcore`

The brain of the `seine-agent` binary. Owns the agent's bearer token
and WireGuard keypair, performs the initial `Register` call, brings up
the local WireGuard interface, and runs the reconciliation loop that
keeps kernel state aligned with the spec.

## File

- `agent.go` — sole file.

## Responsibilities

`agentcore` is deliberately the only place that knows how the agent's
lifecycle and the cross-package interactions fit together. Everything
else is a single-purpose package; this package wires them up.

In one sentence: it composes `wg`, `topology`, `netpolicy`,
`specsource` and the gRPC `cpv1.ControlPlaneClient` into one supervised
loop driven by a `context.Context`.

## Public API

### `type Mode`

```go
type Mode string

const (
    ModeSpoke Mode = "spoke"
    ModeHub   Mode = "hub"
)
```

The agent's personality. Cross-checked at startup against the role
declared for `Name` in the spec; a mismatch is fatal.

### `type Config`

```go
type Config struct {
    Name                  string                 // identity in the spec, required
    Mode                  Mode                   // spoke or hub, required
    Spec                  *specsource.Watcher    // required; provides Current() and Updates()
    ControlPlaneAddr      string                 // host:port, required
    BootstrapToken        string                 // required on first run; optional after
    AdvertisedEndpoint    string                 // required for hubs; usually empty for spokes
    InterfaceName         string                 // default "seine0"
    StateDir              string                 // default /var/lib/seine/<Name>
    ReconcileInterval     time.Duration          // default 30s
    SpecFirstLoadTimeout  time.Duration          // default 60s
    RegisterRetryInterval time.Duration          // default 5s
}
```

Defaults are filled by `Config.applyDefaults` inside `New`. The state
directory holds the WG private key and the bearer token, both at
`0600` permissions.

### `type Agent`

Constructed by `New`; runs by `Run`. Holds the `Config`, the loaded
`wg.Keypair` and bearer token, the gRPC client connection, the `wg.Interface`,
the resolved `topology.Self`, an optional `*netpolicy.Firewall` for hubs,
and OpenTelemetry instruments (`reconcile.total`, `reconcile.duration`).

### `func New(cfg Config) (*Agent, error)`

Validates `Name`, `Mode`, `Spec` and `ControlPlaneAddr`, applies
defaults, computes file paths under `StateDir`, and registers
OpenTelemetry instruments via `otel.Meter`. Does no I/O beyond the
meter registration.

### `func (a *Agent) Run(ctx context.Context) error`

Drives the full lifecycle until `ctx` is cancelled. Returns `nil` on
graceful shutdown, an error on any setup failure.

The function is sequential at startup; once the loop is reached it
runs until shutdown.

## Startup sequence

1. **Per-process logger.** Wrap the context's slog logger with
   `component=agentcore`, `name`, `mode` attributes via
   `logging.WithLogger`. All downstream logs are tagged.
2. **State directory.** `os.MkdirAll(StateDir, 0700)`.
3. **Keypair.** `wg.LoadOrGenerate(<StateDir>/private.key)` returns
   either the persisted key or a new one (written with `0600`).
4. **gRPC client.** `grpc.NewClient` with insecure credentials and
   `otelgrpc.NewClientHandler` for auto-tracing. The connection is
   lazy; the first call dials.
5. **Auth token.** `loadOrRegister`:
   - If `<StateDir>/auth.token` exists, reuse it.
   - Otherwise call `Register` with `BootstrapToken`. Permanent errors
     (`PermissionDenied`, `Unauthenticated`, `InvalidArgument`) abort
     immediately because retrying cannot fix them. Transient errors
     are retried at `RegisterRetryInterval`.
   - On success the token is persisted to disk.
6. **First spec.** `waitForFirstSpec` blocks on `Spec.Updates()` until
   `Spec.Current()` returns a parseable document, with a hard cap of
   `SpecFirstLoadTimeout`.
7. **Self.** `topology.FindSelf(doc, Name)`. The role implied by the
   spec is cross-checked against the configured `Mode`.
8. **WireGuard.** `wg.New(InterfaceName)` and `bringUp` configure the
   private key, listen port (hub-only; from the spec endpoint or
   `hubListenPort`), the tunnel address derived from `Self.TunnelIP`
   plus the overlay prefix length, and the MTU.
9. **Hub-only.** `netpolicy.EnsureIPForwarding` and
   `netpolicy.NewFirewall(InterfaceName)`. The firewall's `Teardown`
   is registered with a deferred handler.
10. **Loop.** `reconcileLoop(ctx)`.

## Reconciliation loop

```go
tick := time.NewTicker(ReconcileInterval)
reconcileOnce(ctx)            // immediate first run
for {
    select {
    case <-ctx.Done():
        return nil
    case <-tick.C:
    case <-Spec.Updates():
        // spec change observed; reconcile early
    }
    started := time.Now()
    err := reconcileOnce(ctx)
    record metrics(result, dur)
}
```

Either a tick or a spec-update event drives the next cycle. Errors
are logged at warn but do not stop the loop. OpenTelemetry counters
record success/failure and a histogram records the cycle duration.

### `reconcileOnce`

```
md := authorization=Bearer <authToken>
resp, err := cp.Heartbeat(md, {Endpoint: AdvertisedEndpoint})

# Self-heal: if the server forgot us (token rotation, DB wipe),
# re-register transparently — but only when we still have a bootstrap
# token to present.
if err is Unauthenticated and BootstrapToken != "":
    register(ctx)
    return nil
elif err != nil:
    return wrapped("heartbeat", err)

doc, commit := Spec.Current()

desired := topology.PeersFor(doc, self, resp.Peers, doc.Spec.WireGuard.PersistentKeepalive)
diff    := iface.ApplyPeers(ctx, desired)

if firewall != nil:                       # hub mode
    compiled := netpolicy.Compile(doc)
    firewall.Reconcile(ctx, compiled)
```

Note that an `Unauthenticated` heartbeat does not retry the cycle:
re-registering is enough work for one tick, and the next tick picks
up reconciliation against the new token.

## Shutdown

Driven entirely by `ctx`:

- `ctx.Done` is selected in the loop, which returns `nil`.
- The deferred `iface.Down` (with a short fresh context) deletes the
  WG netdev.
- The deferred `firewall.Teardown` (hub only) removes the FORWARD
  jump and deletes `SEINE-FWD`.
- The deferred `conn.Close` releases the gRPC client.

The OpenTelemetry shutdown is owned by `cmd/seine-agent`'s `main`,
which calls it after `agent.Run` returns.

## OpenTelemetry signals

Instruments registered in `New`:

| Name | Type | Unit | Attributes |
| --- | --- | --- | --- |
| `seine.reconcile.total` | Int64Counter | (count) | `result` ∈ `{success, failure}` |
| `seine.reconcile.duration` | Float64Histogram | seconds | none |

The OTel meter resolves to no-op providers when telemetry is disabled,
so the metric calls are always safe.

The gRPC client is auto-traced via `otelgrpc.NewClientHandler`, which
wraps every `cp.Register` and `cp.Heartbeat` call in a span.

## Failure-mode summary

| Symptom | Outcome |
| --- | --- |
| Permanent registration error (`PermissionDenied`, `Unauthenticated`, `InvalidArgument`) | Process exits with the error. |
| Transient registration error | Retried every `RegisterRetryInterval` until ctx cancel. |
| First spec not loaded within `SpecFirstLoadTimeout` | Process exits. |
| `--mode=hub` but spec lists self as `agents` (or vice versa) | Process exits. |
| `wg.New` returns "unsupported OS" | Process exits — Linux only. |
| `EnsureIPForwarding` cannot write `/proc/sys/...` and value is not already 1 | Process exits with a hint about `--privileged`. |
| `Heartbeat` returns `Unauthenticated` mid-loop | Re-register if `BootstrapToken` is set, else log and skip. |
| `topology.PeersFor` reports a missing hub | Cycle logs warn; retried next tick. |
| `wg.ApplyPeers` or `Firewall.Reconcile` fails | Cycle logs warn; retried next tick. |

## Used by

- `cmd/seine-agent` — the only caller; constructs an `Agent` per
  process and calls `Run`.
