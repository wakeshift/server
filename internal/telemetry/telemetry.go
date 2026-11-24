// Package telemetry exports the core's traces and logs with OpenTelemetry,
// when OTEL_EXPORTER_OTLP_ENDPOINT is set (with OTEL_EXPORTER_OTLP_HEADERS
// for the collector's key, and the other standard OTEL_* variables).
//
// Nothing leaves the process unscrubbed: every span passes through Scrub
// and every log record through ScrubLogs before the exporter sees it,
// which keep only attributes on an allowlist and strip URLs, quoted
// values, emails and tokens from the text they keep.
// Stream links, publish URLs and invite codes are credentials carried in
// URLs, and errors can quote event content or a database row's values, so
// the allowlist is the only way an attribute gets out: adding a key to it
// is security-sensitive.
package telemetry

import (
	"context"
	"net/http"
	"os"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Start exports traces as service (unless OTEL_SERVICE_NAME names
// another) at version, and returns the provider for Handler and a shutdown
// that flushes what's left. Without OTEL_EXPORTER_OTLP_ENDPOINT it returns
// a nil provider and a no-op shutdown: nothing is traced.
func Start(ctx context.Context, service, version string) (trace.TracerProvider, func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return nil, func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	res, err := newResource(ctx, service, version)
	if err != nil {
		return nil, nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(Scrub(exp)), sdktrace.WithResource(res))
	return tp, tp.Shutdown, nil
}

// newResource is what every exported span and log record says about the
// process: service, version, and deployment.environment.name (from
// OTEL_RESOURCE_ATTRIBUTES).
func newResource(ctx context.Context, service, version string) (*resource.Resource, error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(semconv.ServiceName(service), semconv.ServiceVersion(version)))
	if err != nil {
		return nil, err
	}
	fromEnv, err := resource.New(ctx, resource.WithFromEnv())
	if err != nil {
		return nil, err
	}
	return resource.Merge(res, fromEnv)
}

// Handler traces h's requests with tp, or returns h unchanged when tp is
// nil. Spans are named by the route's pattern ("POST /v1/publish/{token}"),
// never the path.
func Handler(tp trace.TracerProvider, h http.Handler) http.Handler {
	if tp == nil {
		return h
	}
	// otelhttp names the span by the pattern the mux matched but sets
	// http.route only before it has: set it once the mux has (scrubName
	// keeps a name only if it's the method and route).
	routed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		if _, route, ok := strings.Cut(r.Pattern, "/"); ok {
			trace.SpanFromContext(r.Context()).SetAttributes(semconv.HTTPRoute("/" + route))
		}
	})
	return otelhttp.NewHandler(routed, "wakeshift",
		otelhttp.WithTracerProvider(tp),
		// W3C trace context is read from callers (not baggage: it's
		// whatever a caller chose to send), but the core is public, so a
		// caller mustn't choose which requests are sampled or put them in
		// its trace: every request is a new root, sampled by us, and a
		// caller's context is kept only as a link (its tracestate dropped
		// by Scrub).
		otelhttp.WithPropagators(propagation.TraceContext{}),
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }),
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/health" && r.URL.Path != "/ready" }),
	)
}
