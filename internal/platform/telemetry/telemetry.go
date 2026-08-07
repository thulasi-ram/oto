package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/thulasiram/oto/internal/platform/config"
)

// Telemetry owns the process's metric registry and trace provider.
//
// oto is an alerting product; its own observability is expected to be exemplary,
// so the registry is explicit rather than the global default and every metric is
// registered through it.
type Telemetry struct {
	Registry *prometheus.Registry
	Tracer   trace.Tracer

	shutdown []func(context.Context) error
}

// Setup builds the Prometheus registry and, when enabled, the OTLP trace pipeline.
func Setup(ctx context.Context, cfg config.Config) (*Telemetry, error) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	t := &Telemetry{Registry: reg, Tracer: noop.NewTracerProvider().Tracer("oto")}

	if !cfg.Telemetry.TracingEnabled {
		return t, nil
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.Service),
		semconv.ServiceVersion(cfg.Version),
		attribute.String("deployment.environment", cfg.Env),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Telemetry.OTLPEndpoint)}
	if cfg.Telemetry.OTLPInsecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}

	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: otlp exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Telemetry.TraceSampleRate))),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	t.Tracer = tp.Tracer("github.com/thulasiram/oto")
	t.shutdown = append(t.shutdown, tp.Shutdown)
	return t, nil
}

// MetricsHandler serves the Prometheus exposition format for this registry.
func (t *Telemetry) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(t.Registry, promhttp.HandlerOpts{
		Registry:          t.Registry,
		EnableOpenMetrics: true,
	})
}

// Shutdown flushes and stops every pipeline.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	for _, fn := range t.shutdown {
		if err := fn(ctx); err != nil {
			return fmt.Errorf("telemetry: shutdown: %w", err)
		}
	}
	return nil
}
