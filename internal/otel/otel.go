// Package otel wires OpenTelemetry providers (traces, metrics, logs) for
// the seine binaries.
//
// The package is intentionally narrow: a single Setup() call returns the
// global providers configured to export over OTLP/gRPC and a shutdown
// closure callers must invoke before exiting. If no OTLP endpoint is
// configured, Setup installs no-op providers so callers can use OTel
// instruments unconditionally without worrying about nil checks.
package otel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// Config controls OTel provider construction.
type Config struct {
	// ServiceName labels every signal with service.name. Required.
	ServiceName string

	// ServiceVersion is added as service.version on the resource.
	ServiceVersion string

	// OTLPEndpoint is host:port of an OTLP/gRPC collector (e.g.
	// "localhost:4317"). Empty disables exporters and installs no-op
	// providers — callers can still use OTel APIs but signals go
	// nowhere.
	OTLPEndpoint string

	// Insecure controls whether the gRPC connection skips TLS. Demo
	// deployments typically set this; production should not.
	Insecure bool
}

// ShutdownFunc gracefully drains buffered telemetry. It must be invoked
// before the process exits or signals may be lost.
type ShutdownFunc func(context.Context) error

// Setup configures the global OTel providers. The returned shutdown func
// is non-nil even when an exporter could not be created; in that case it
// is a no-op so callers do not have to special-case errors.
func Setup(ctx context.Context, cfg Config) (ShutdownFunc, error) {
	if cfg.ServiceName == "" {
		return noopShutdown, errors.New("otel: ServiceName is required")
	}
	if cfg.OTLPEndpoint == "" {
		// No collector configured — nothing to set up. The default
		// OTel SDK no-op providers are already installed.
		return noopShutdown, nil
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
			semconv.ServiceVersionKey.String(cfg.ServiceVersion),
		),
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithHost(),
	)
	if err != nil {
		return noopShutdown, fmt.Errorf("otel: build resource: %w", err)
	}

	traceShutdown, err := setupTracer(ctx, cfg, res)
	if err != nil {
		return noopShutdown, err
	}
	meterShutdown, err := setupMeter(ctx, cfg, res)
	if err != nil {
		_ = traceShutdown(ctx)
		return noopShutdown, err
	}
	logShutdown, err := setupLogger(ctx, cfg, res)
	if err != nil {
		_ = traceShutdown(ctx)
		_ = meterShutdown(ctx)
		return noopShutdown, err
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	shutdown := func(ctx context.Context) error {
		// Combine all three; do not short-circuit on the first failure
		// or we may leak the others.
		var errs []error
		if err := traceShutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := meterShutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := logShutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}
	return shutdown, nil
}

// SlogHandler returns a slog.Handler that bridges to the global OTel
// LoggerProvider. The result is suitable for slog.New so all log lines
// are exported as OTLP logs in addition to whatever handler was already
// installed. If OTel was not configured, the bridge is still safe to use
// (it forwards into the no-op provider).
func SlogHandler(serviceName string) slog.Handler {
	return otelslog.NewHandler(serviceName)
}

func setupTracer(ctx context.Context, cfg Config, res *resource.Resource) (ShutdownFunc, error) {
	exp, err := otlptrace.New(ctx, otlpTraceClient(cfg))
	if err != nil {
		return noopShutdown, fmt.Errorf("otel: trace exporter: %w", err)
	}
	tp := trace.NewTracerProvider(
		trace.WithBatcher(exp, trace.WithBatchTimeout(2*time.Second)),
		trace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func setupMeter(ctx context.Context, cfg Config, res *resource.Resource) (ShutdownFunc, error) {
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.Insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	exp, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return noopShutdown, fmt.Errorf("otel: metric exporter: %w", err)
	}
	mp := metric.NewMeterProvider(
		metric.WithReader(metric.NewPeriodicReader(exp, metric.WithInterval(15*time.Second))),
		metric.WithResource(res),
	)
	otel.SetMeterProvider(mp)
	return mp.Shutdown, nil
}

func setupLogger(ctx context.Context, cfg Config, res *resource.Resource) (ShutdownFunc, error) {
	opts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.Insecure {
		opts = append(opts, otlploggrpc.WithInsecure())
	}
	exp, err := otlploggrpc.New(ctx, opts...)
	if err != nil {
		return noopShutdown, fmt.Errorf("otel: log exporter: %w", err)
	}
	lp := log.NewLoggerProvider(
		log.WithProcessor(log.NewBatchProcessor(exp)),
		log.WithResource(res),
	)
	// Register globally so otelslog and any other bridges resolve it.
	otellog.SetLoggerProvider(lp)
	return lp.Shutdown, nil
}

func otlpTraceClient(cfg Config) otlptrace.Client {
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	return otlptracegrpc.NewClient(opts...)
}

func noopShutdown(context.Context) error { return nil }
