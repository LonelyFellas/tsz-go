// Package observability wires optional OpenTelemetry tracing. It is deliberately
// off unless a collector endpoint is configured, so the instrumentation hooks
// are always present in the code path but cost nothing until you opt in.
package observability

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// TracingConfig configures OTLP/HTTP trace export.
type TracingConfig struct {
	// Endpoint is the collector address as host:port. Empty disables tracing.
	Endpoint    string
	ServiceName string
	Env         string
	// Insecure uses plain HTTP (no TLS) to reach the collector.
	Insecure bool
}

func noopShutdown(context.Context) error { return nil }

// InitTracer installs a global OTLP tracer provider when cfg.Endpoint is set,
// and returns a shutdown that flushes and stops the exporter (call it on server
// shutdown). When the endpoint is empty, tracing stays disabled: the global
// tracer remains a no-op, so otelgin spans are nearly free and nothing is
// exported. The returned shutdown is always safe to call.
func InitTracer(ctx context.Context, cfg TracingConfig) (func(context.Context) error, error) {
	if cfg.Endpoint == "" {
		return noopShutdown, nil
	}

	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return noopShutdown, fmt.Errorf("otlp trace exporter: %w", err)
	}

	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(cfg.ServiceName),
		attribute.String("deployment.environment", cfg.Env),
	))
	if err != nil {
		return noopShutdown, fmt.Errorf("otel resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return tp.Shutdown, nil
}
