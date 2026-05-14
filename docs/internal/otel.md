# Package `internal/otel`

OpenTelemetry providers (traces, metrics, logs) wired for OTLP/gRPC
export. A single `Setup` call returns the global providers and a
shutdown closure callers must invoke before the process exits.

## File

- `otel.go` — sole file.

## Design

The package is intentionally narrow:

- One entry point (`Setup`) that returns one shutdown function.
- One configuration struct (`Config`) with no implicit defaults.
- No-op installation when no OTLP endpoint is configured, so callers
  can use OTel APIs unconditionally without nil checks.

It is not a generic OTel toolkit; it serves the two seine binaries.

## Public API

### `type Config`

```go
type Config struct {
    ServiceName    string  // labels every signal; required
    ServiceVersion string  // service.version on the resource
    OTLPEndpoint   string  // host:port of an OTLP/gRPC collector; empty disables export
    Insecure       bool    // skip TLS for the OTLP gRPC connection
}
```

`OTLPEndpoint == ""` is the no-op path. The function returns a no-op
shutdown so the call site does not need to special-case it.

### `type ShutdownFunc`

```go
type ShutdownFunc func(context.Context) error
```

Drains buffered telemetry. Both binaries register it through `defer`
with a 5-second timeout context.

### `func Setup(ctx context.Context, cfg Config) (ShutdownFunc, error)`

When `OTLPEndpoint` is non-empty, builds three SDK providers and
registers them globally:

1. **Tracer.** `trace.NewTracerProvider` with a batched OTLP/gRPC
   exporter and a 2 s batch timeout. Installed via
   `otel.SetTracerProvider`.
2. **Meter.** `metric.NewMeterProvider` with a periodic reader
   (15 s interval) feeding the OTLP/gRPC metric exporter. Installed
   via `otel.SetMeterProvider`.
3. **Logger.** `log.NewLoggerProvider` with a batch processor on the
   OTLP/gRPC log exporter. Installed via `otellog.SetLoggerProvider`
   (the global `go.opentelemetry.io/otel/log/global` package), which
   is what bridges like `otelslog` resolve.

It also installs a composite text-map propagator
(`TraceContext` + `Baggage`) so traces survive cross-process gRPC
calls.

The returned shutdown closure invokes the trace, meter and log
shutdowns in sequence, joining any errors with `errors.Join`. It does
not stop on the first failure so a slow exporter cannot leak the
others.

When `OTLPEndpoint == ""` the function does not touch the global
providers; OTel's built-in no-op providers stay in place.

### `func SlogHandler(serviceName string) slog.Handler`

Returns an `otelslog.NewHandler` configured with the given service
name. Useful when a process wants to bridge its `log/slog` output to
OTLP logs in addition to the local handler installed by `logging.Setup`.
Currently the binaries use the global `LoggerProvider` registration
indirectly (through the OTel slog bridge wherever components ask for
it); this helper is exported so other packages or tests can wire the
bridge explicitly.

## Resource attributes

When `OTLPEndpoint` is set, the resource attached to every signal is
built from:

- `service.name = ServiceName`
- `service.version = ServiceVersion`
- `resource.WithFromEnv()` — picks up `OTEL_RESOURCE_ATTRIBUTES`.
- `resource.WithProcess()` — process pid, executable name, etc.
- `resource.WithHost()` — host name.

## How instruments are used

- `internal/agentcore` declares two instruments at construction:
  `seine.reconcile.total` (counter) and `seine.reconcile.duration`
  (histogram). They resolve through the global `MeterProvider`, so
  they are no-ops when OTel is disabled.
- gRPC server (`cmd/seine-server`) installs
  `otelgrpc.NewServerHandler` as a `grpc.StatsHandler`, which produces
  one span per inbound RPC.
- gRPC client (`internal/agentcore`) installs
  `otelgrpc.NewClientHandler` analogously, producing one span per
  outbound `Register` / `Heartbeat`.

## Configuration in the binaries

Both `cmd/seine-server` and `cmd/seine-agent` expose:

- `--otlp-endpoint` (env `SEINE_OTLP_ENDPOINT`) — `host:port` of an
  OTLP/gRPC collector. Empty disables.
- `--otlp-insecure` (env `SEINE_OTLP_INSECURE`) — `true` by default
  for the demo; should be `false` against a TLS-fronted collector in
  production.

## Used by

- `cmd/seine-server` and `cmd/seine-agent` — the two `Setup` callers.
- `internal/agentcore` — uses `otel.Meter` for metric instruments.
- gRPC instrumentation in the binaries via
  `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc`.
