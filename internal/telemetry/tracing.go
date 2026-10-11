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

// TracingConfig configura l'esportazione delle tracce via OTLP/HTTP.
type TracingConfig struct {
	Enabled bool
	// Endpoint del collector, es. otel-collector:4318 o https://otel.example.com:4318.
	// Vuoto: OTEL_EXPORTER_OTLP_ENDPOINT o OTEL_EXPORTER_OTLP_TRACES_ENDPOINT.
	Endpoint string
	// Insecure usa http invece di https (solo con un endpoint senza schema).
	Insecure bool
	// SampleRatio è la frazione delle tracce campionate (0..1), se la richiesta non ne
	// porta già una decisione.
	SampleRatio float64
	Service     string
	Version     string
	// Instance identifica il processo (es. executor_id).
	Instance string
}

// propagator è il formato W3C Trace Context, usato anche per passare la traccia dalla
// richiesta all'API al workflow eseguito dal worker.
var propagator = propagation.TraceContext{}

// SetupTracing configura il tracer globale. Senza tracing abilitato le span sono no-op.
// Restituisce la funzione che esporta le span rimaste e arresta il provider.
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
		return nil, fmt.Errorf("exporter OTLP: %w", err)
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

// Tracer restituisce il tracer di manent.
func Tracer() trace.Tracer { return otel.Tracer("github.com/joeg-ita/manent") }

// TraceParent restituisce l'intestazione traceparent della span corrente, o "".
func TraceParent(ctx context.Context) string {
	c := propagation.MapCarrier{}
	propagator.Inject(ctx, c)
	return c.Get("traceparent")
}

// WithTraceParent restituisce ctx con la span remota indicata da traceparent come genitore.
func WithTraceParent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return propagator.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}

// Extract legge la traccia dalle intestazioni di una richiesta HTTP.
func Extract(ctx context.Context, h propagation.TextMapCarrier) context.Context {
	return propagator.Extract(ctx, h)
}
