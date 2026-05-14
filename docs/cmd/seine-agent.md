# Command `seine-agent`

`cmd/seine-agent` is the entry point of the data plane process. Like
`seine-server` it is a thin orchestrator: configuration parsing and
dependency wiring only. The substance of the agent lives in
[`internal/agentcore`](../internal/agentcore.md).

## Source

A single file: `cmd/seine-agent/main.go`.

## Personalities

The same binary runs in two personalities, selected by `--mode`:

- **spoke** — joins the overlay, generates and persists a WireGuard
  keypair, and routes all overlay traffic through its assigned hub.
  Listens on a system-assigned UDP port and connects outbound only.
- **hub** — does everything a spoke does, plus enables IP forwarding,
  binds a publicly reachable UDP port (`--advertise-endpoint`),
  participates in the hub mesh, and reconciles an iptables ACL chain
  on every cycle.

The personality is a property of the spec (`hubs[]` vs `agents[]`) as
well; `agentcore.Run` cross-checks `--mode` against `topology.FindSelf`
and refuses to start on a mismatch.

## What it composes

In order of construction:

1. **Logging** — `logging.Setup`, then `logging.WithLogger` on the
   process context.
2. **Required flag validation** — `--name`, `--control-plane`,
   `--git-url`; if `--mode=hub` then `--advertise-endpoint` is also
   required.
3. **Process context** — `signal.NotifyContext` for `SIGINT`/`SIGTERM`.
4. **OpenTelemetry** — `otel.Setup` with `service.name=seine-agent`.
5. **Spec source** — `specsource.New` and a goroutine running
   `Watcher.Run`.
6. **Agent core** — `agentcore.New` with all wiring, then
   `agent.Run(ctx)` which blocks until shutdown.

## Configuration

Flags are paired with `SEINE_*` environment variables.

### Identity

| Flag | Env | Default | Notes |
| --- | --- | --- | --- |
| `--name` | `SEINE_AGENT_NAME` | _(required)_ | Must match a hub or agent in the spec. |
| `--mode` | `SEINE_MODE` | `spoke` | `spoke` or `hub`. |
| `--control-plane` | `SEINE_CONTROL_PLANE` | _(required)_ | `host:port` of the management server. |
| `--bootstrap-token` | `SEINE_BOOTSTRAP_TOKEN` | _(empty)_ | Used only on first Register. |
| `--advertise-endpoint` | `SEINE_ADVERTISE_ENDPOINT` | _(required for hubs)_ | Public `host:port` other peers should dial. |

### Local state

| Flag | Env | Default | Notes |
| --- | --- | --- | --- |
| `--interface` | `SEINE_INTERFACE` | `seine0` | WireGuard netdev name. |
| `--state-dir` | `SEINE_STATE_DIR` | `/var/lib/seine/<name>` | Persistent files: `private.key`, `auth.token`. |
| `--reconcile-interval` | `SEINE_RECONCILE_INTERVAL` | `30s` | Cycle cadence; spec changes trigger early ticks. |

### Git source

Same flags as `seine-server`; refer to
[seine-server.md](seine-server.md#git-source).

### OpenTelemetry

Same flags as `seine-server`; refer to
[seine-server.md](seine-server.md#opentelemetry).

### Diagnostics

| Flag | Effect |
| --- | --- |
| `--log-level`, `--log-format` | Same semantics as `seine-server`. |
| `--version` | Prints `seine-agent <version> (<commit>)` and exits. |

## State directory

Two files live there, both with `0600` permissions:

- `private.key` — base64-encoded Curve25519 private key. Created on
  first run by `wg.LoadOrGenerate`.
- `auth.token` — opaque bearer token returned by the control plane on
  first `Register`. Written by `agentcore.register`.

If both files exist, the agent skips re-registration on startup. If
the auth token is later invalidated server-side, the heartbeat path
auto-recovers via re-register (only when `--bootstrap-token` is set).

## Capabilities and host requirements

The agent uses Linux netlink to manage the WireGuard interface and
iptables to manage ACL. In Docker that means:

- `--cap-add NET_ADMIN`
- `/dev/net/tun` mounted (for the WireGuard kernel module)
- For hubs: `--sysctl net.ipv4.ip_forward=1`

The included `deployments/compose/docker-compose.yml` sets these.

## Logs you can expect at startup

```
INFO starting seine-agent version=…  name=spoke-cloud  mode=spoke
INFO keypair loaded public_key=<base64>
INFO reusing stored auth token            (or "registered with control plane" on first run)
INFO cloning git source url=…  branch=main
INFO spec updated commit=<sha>
INFO created wireguard link iface=seine0
INFO interface up listen_port=… address=100.64.1.10/10 mtu=1420
INFO agent up role=ROLE_SPOKE tunnel_ip=100.64.1.10 interface=seine0
INFO peers reconciled added=1 updated=0 removed=0
```

For a hub, an additional line per cycle:

```
INFO acl reconciled allow_rules=N deny_rules=M hub_count=K
```

## Failure handling

| Symptom | Cause | Recovery |
| --- | --- | --- |
| `--bootstrap-token is empty` and no token file | First run without operator-provided token | Set `SEINE_BOOTSTRAP_TOKEN`. |
| `Register: PermissionDenied` | Name not in spec | Edit and push the spec. |
| `Heartbeat: Unauthenticated` | Server forgot the agent (DB wipe) | Auto re-register if bootstrap token is still set. |
| `topology: hub %q has not yet registered` | Hub is up but has not heartbeated yet | Cycle retries on the next tick. |
| `wg: open wgctrl` error | No WireGuard kernel module / missing `NET_ADMIN` | Fix host capabilities. |

## See also

- [internal/agentcore](../internal/agentcore.md) — main loop implementation.
- [internal/wg](../internal/wg.md) — WireGuard interface abstraction.
- [internal/topology](../internal/topology.md) — peer-set computation.
- [internal/netpolicy](../internal/netpolicy.md) — hub ACL enforcement.
