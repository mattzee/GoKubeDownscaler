// Package tracing sets up OpenTelemetry tracing for the downscaler.
//
// Tracing is off unless enabled. When enabled, spans are exported over OTLP
// gRPC and everything is configured through the standard OpenTelemetry
// environment variables, so no downscaler-specific settings are needed:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_TRACES_ENDPOINT  collector address
//	OTEL_EXPORTER_OTLP_INSECURE                                       plaintext to the collector
//	OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES                       resource
//	OTEL_TRACES_SAMPLER, OTEL_TRACES_SAMPLER_ARG                      sampling
package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	// DefaultServiceName is the service.name used when OTEL_SERVICE_NAME is not set.
	DefaultServiceName = "kubedownscaler"
	// InstrumentationName names the tracer the downscaler's own spans come from.
	InstrumentationName = "github.com/caas-team/gokubedownscaler"
)

// Shutdown flushes buffered spans and stops the exporter.
type Shutdown func(ctx context.Context) error

// Setup installs the global tracer provider and propagator. When enabled is
// false it leaves OpenTelemetry's no-op provider in place and returns a no-op
// Shutdown, so instrumented code costs next to nothing.
func Setup(ctx context.Context, enabled bool) (Shutdown, error) {
	if !enabled {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	// Later options win: OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override the default name.
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", DefaultServiceName)),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build trace resource: %w", err)
	}

	// The sampler comes from OTEL_TRACES_SAMPLER; the SDK defaults to parentbased_always_on.
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	return provider.Shutdown, nil
}

// Tracer returns the downscaler's tracer from the global provider.
func Tracer() trace.Tracer {
	return otel.Tracer(InstrumentationName)
}
