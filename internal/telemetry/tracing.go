package telemetry

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TracingConfig configures the export of traces over OTLP/HTTP.
type TracingConfig struct {
	Enabled bool
	// Endpoint of the collector, e.g. otel-collector:4318 or https://otel.example.com:4318.
	// Empty: OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT.
	Endpoint string
	// Insecure uses http instead of https (only with an endpoint without scheme).
	Insecure bool
	// SampleRatio is the fraction of sampled traces (0..1), if the request does not already carry
	// a decision.
	SampleRatio float64
	Service     string
	Version     string
	// Instance identifies the process (e.g. executor_id).
	Instance string
}

// propagator is the W3C Trace Context format, also used to pass the trace from the API
// request to the workflow run by the worker.
var propagator = propagation.TraceContext{}

// SetupTracing configures the global tracer. Without tracing enabled spans are no-ops.
// It returns the function that exports the remaining spans and stops the provider.
func SetupTracing(ctx context.Context, cfg TracingConfig) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagator)
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	var opts []otlptracehttp.Option
	switch {
	case strings.Contains(cfg.Endpoint, "://"):
		opts = append(opts, otlptracehttp.WithEndpointURL(cfg.Endpoint))
	case cfg.Endpoint != "":
		opts = append(opts, otlptracehttp.WithEndpoint(cfg.Endpoint))
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("OTLP exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", cfg.Service),
		attribute.String("service.version", cfg.Version),
		attribute.String("service.instance.id", cfg.Instance),
	))
	if err != nil {
		return nil, err
	}
	ratio := cfg.SampleRatio
	if ratio <= 0 || ratio > 1 {
		ratio = 1
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// Tracer returns manent's tracer.
func Tracer() trace.Tracer { return otel.Tracer("github.com/manent-deployer/manent") }

// TraceParent returns the traceparent header of the current span, or "".
func TraceParent(ctx context.Context) string {
	c := propagation.MapCarrier{}
	propagator.Inject(ctx, c)
	return c.Get("traceparent")
}

// WithTraceParent returns ctx with the remote span given by traceparent as parent.
func WithTraceParent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return propagator.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}

// Extract reads the trace from the headers of an HTTP request.
func Extract(ctx context.Context, h propagation.TextMapCarrier) context.Context {
	return propagator.Extract(ctx, h)
}
