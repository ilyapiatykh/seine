# Package `internal/netpolicy`

Hub-side policy enforcement on Linux: enables IP forwarding so the
kernel can route between WireGuard peers on the same interface, and
reconciles the spec's ACL rules into a curated `iptables` chain. Spokes
do not use this package — there is nothing for them to enforce.

## Files

- `netpolicy.go` — `Rule`, `Compiled`, `Compile`. Cross-platform.
- `forwarding_linux.go` — `EnsureIPForwarding` (Linux).
- `forwarding_other.go` — stub for non-Linux.
- `firewall_linux.go` — `Firewall`, `NewFirewall`, `Reconcile`,
  `Teardown` (Linux).
- `firewall_other.go` — stub for non-Linux.

## Design overview

The thesis settles on declarative ACL: groups are listed in YAML, and
rules state which groups may communicate. Since spoke-to-spoke
traffic always transits a hub, the hub is the natural enforcement
point. The package implements that enforcement with two artefacts:

1. **`net.ipv4.ip_forward = 1`** — turning the hub host (or container)
   into a router so packets entering the WG interface and destined for
   another peer are forwarded rather than dropped.
2. **A custom iptables chain `SEINE-FWD`** — installed in the `filter`
   table with `FORWARD` jumping into it for `seine0 → seine0` traffic.
   Rules are flushed and rewritten on every reconcile so the live
   ruleset always matches the spec.

## Cross-platform surface

### `type Rule`

```go
type Rule struct {
    Source netip.Addr
    Dest   netip.Addr
    Allow  bool
}
```

A flat allow / deny decision between two tunnel IPs. `Compile` expands
the spec's group rules into this form.

### `type Compiled`

```go
type Compiled struct {
    Allows  []Rule         // explicitly permitted (src, dst) pairs
    Denies  []Rule         // explicitly denied pairs
    HubIPs  []netip.Addr   // tunnel IPs of all hubs
    Overlay netip.Prefix   // network-wide CIDR
}
```

The `Firewall` consumes a `*Compiled`. Splitting compilation from
application keeps `Compile` testable without iptables and isolates
platform-specific code behind a small interface boundary.

### `func Compile(doc *spec.Document) (*Compiled, error)`

Expands group-based ACL rules into per-IP-pair decisions:

```
groupMembers ← { groupName → []tunnelIP } from spec.Agents
for each ACL rule r in doc.Spec.ACLs:
    for each (fromGroup, toGroup) in cartesian(r.From, r.To):
        for each (src, dst) in cartesian(fromGroup, toGroup):
            if src == dst: continue            # self-loops invisible to kernel
            if r.Action == allow:
                allows ← (src, dst)             # deduplicated
            else:
                denies ← (src, dst)
```

Hub tunnel IPs are collected separately. They are excluded from group
expansion (hubs are transit, not endpoints) and reused unconditionally
by the firewall to keep diagnostics (`ping <hub-ip>`) working.

The output is sorted lexicographically (by source then destination) so
that successive runs of `Compile` produce identical iptables rule
sequences and counters do not flap unnecessarily.

Error cases: invalid `spec.cidr`, invalid agent or hub tunnel IP.
Both are caught earlier by `spec.Validate`, so in practice `Compile`
fails only on a programming bug.

## Linux backend

### `func EnsureIPForwarding(ctx) error`

Reads `/proc/sys/net/ipv4/ip_forward`; if it is already `1` the
function logs at debug and returns. Otherwise it writes `"1\n"`. The
common containerised case is short-circuited because Docker enables
forwarding on bridge networks, leaving `/proc/sys` read-only — the
read path detects the desired state and returns without writing.

A failure to write produces a hint about `--privileged` /
`--sysctl net.ipv4.ip_forward=1` so operators know how to fix it.

### `type Firewall`

```go
type Firewall struct {
    ipt   *iptables.IPTables
    iface string
}

func NewFirewall(ifaceName string) (*Firewall, error)
```

Holds an `iptables` client and the WireGuard interface name. Construction
opens the iptables socket; failures usually mean the process lacks
`CAP_NET_ADMIN`, and the error string surfaces that hint.

### `func (f *Firewall) Reconcile(ctx, *Compiled) error`

Implements "ensure custom chain → ensure FORWARD jump → flush →
populate":

1. **Ensure chain.** If `SEINE-FWD` does not exist in `filter`, create
   it.
2. **Ensure FORWARD jump.** Install `-I FORWARD 1 -i <iface> -o
   <iface> -j SEINE-FWD` if absent. Inserting at position 1 keeps it
   ahead of any rules other components might have added.
3. **Clear the chain.** `ClearChain("filter", "SEINE-FWD")`.
4. **Populate** in the following order (order matters because the
   first match wins):
   1. For every hub IP `H`: `-s H -j ACCEPT` and `-d H -j ACCEPT`.
   2. For every explicit deny rule `(src, dst)`: `-s src -d dst -j DROP`.
   3. For every allow rule: `-s src -d dst -j ACCEPT`.
   4. Default deny inside the overlay: `-s <overlay> -d <overlay> -j DROP`.
   5. Safety net: `-j RETURN` (so non-overlay traffic, which the
      FORWARD filter should already have excluded, does not loop).

The resulting chain is deterministic with respect to the input, which
means an unchanged spec produces an unchanged chain on every cycle —
useful for log readability and for not perturbing live counters more
than necessary.

### `func (f *Firewall) Teardown(ctx)`

Best-effort cleanup on shutdown:

1. Remove the FORWARD jump if present.
2. Flush and delete `SEINE-FWD` if present.

Errors are logged at warn level rather than returned because a noisy
shutdown should not mask the agent's primary exit code.

## Non-Linux stubs

`forwarding_other.go` and `firewall_other.go` provide compile-time
stubs that return clear "only supported on Linux" errors. They exist
so that the codebase cross-compiles on macOS and Windows; running an
agent in `--mode=hub` on those systems fails at process start instead
of at the point of first iptables call.

## Resulting iptables (example from the demo)

```
Chain SEINE-FWD (1 references)
target     source         destination
ACCEPT     100.64.0.1     0.0.0.0/0           # hub-eu allow-all
ACCEPT     0.0.0.0/0      100.64.0.1
ACCEPT     100.64.0.2     0.0.0.0/0           # hub-us allow-all
ACCEPT     0.0.0.0/0      100.64.0.2
ACCEPT     100.64.1.10    100.64.2.10         # cloud → office allow
ACCEPT     100.64.2.10    100.64.1.10         # office → cloud allow
DROP       100.64.0.0/10  100.64.0.0/10       # default deny
RETURN     0.0.0.0/0      0.0.0.0/0           # safety net

Chain FORWARD
target       source     destination
SEINE-FWD    0.0.0.0/0  0.0.0.0/0    -i seine0 -o seine0    # jump rule
```

## Concurrency

`Firewall` is intended to be driven by a single goroutine (the agent's
reconcile loop). The underlying iptables CLI is not safe for parallel
mutations from the same process; `agentcore` enforces single-driver
access by construction.

## Used by

- `agentcore` — the only consumer. Hub-mode agents call
  `EnsureIPForwarding` once at startup and `NewFirewall` /
  `Firewall.Reconcile` on every cycle. The firewall is `Teardown`'d
  in a deferred shutdown handler.
