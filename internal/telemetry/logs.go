package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// StartLogs exports log records as service at version when OTLP is
// configured (OTEL_EXPORTER_OTLP_ENDPOINT, or
// OTEL_EXPORTER_OTLP_LOGS_ENDPOINT) and OTEL_LOGS_EXPORTER isn't "none",
// every record through ScrubLogs first. It returns the provider for
// LogHandler and a shutdown that flushes what's left; without an endpoint,
// a nil provider and a no-op shutdown: nothing is exported.
//
// The exporter appends /v1/logs to OTEL_EXPORTER_OTLP_ENDPOINT's path, as
// the trace exporter appends /v1/traces, so one endpoint serves both.
func StartLogs(ctx context.Context, service, version string) (log.LoggerProvider, func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	switch v := os.Getenv("OTEL_LOGS_EXPORTER"); v {
	case "none":
		return nil, noop, nil
	case "", "otlp":
	default:
		return nil, nil, fmt.Errorf("OTEL_LOGS_EXPORTER is %q: only otlp and none are supported", v)
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") == "" {
		return nil, noop, nil
	}
	exp, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	res, err := newResource(ctx, service, version)
	if err != nil {
		return nil, nil, err
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(ScrubLogs(exp))), sdklog.WithResource(res))
	return lp, lp.Shutdown, nil
}

// LogHandler is base, unchanged, plus a copy of each record at min or
// above sent to lp (which must scrub: StartLogs's does). A record logged
// with a context carrying a span is linked to it. With a nil lp it's base.
func LogHandler(base slog.Handler, lp log.LoggerProvider, min slog.Leveler) slog.Handler {
	if lp == nil {
		return base
	}
	return fanout{base: base, export: otelslog.NewHandler("github.com/wakeshift/server", otelslog.WithLoggerProvider(lp)), min: min}
}

// fanout sends records to base as base would take them, and to export
// those at min or above.
type fanout struct {
	base, export slog.Handler
	min          slog.Leveler
}

func (f fanout) exports(ctx context.Context, l slog.Level) bool {
	return l >= f.min.Level() && f.export.Enabled(ctx, l)
}

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	return f.base.Enabled(ctx, l) || f.exports(ctx, l)
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	if f.base.Enabled(ctx, r.Level) {
		errs = append(errs, f.base.Handle(ctx, r.Clone()))
	}
	if f.exports(ctx, r.Level) {
		errs = append(errs, f.export.Handle(ctx, r))
	}
	return errors.Join(errs...)
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	return fanout{base: f.base.WithAttrs(attrs), export: f.export.WithAttrs(attrs), min: f.min}
}

func (f fanout) WithGroup(name string) slog.Handler {
	return fanout{base: f.base.WithGroup(name), export: f.export.WithGroup(name), min: f.min}
}
