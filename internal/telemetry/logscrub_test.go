package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// memLogs keeps the records it's exported.
type memLogs struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (m *memLogs) Export(_ context.Context, rs []sdklog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rs {
		m.records = append(m.records, r.Clone())
	}
	return nil
}
func (m *memLogs) Shutdown(context.Context) error   { return nil }
func (m *memLogs) ForceFlush(context.Context) error { return nil }

func (m *memLogs) get() []sdklog.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sdklog.Record(nil), m.records...)
}

func attrsOf(r sdklog.Record) map[string]attribute.Value {
	out := map[string]attribute.Value{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = kv.Value
		return true
	})
	return out
}

func dumpRecord(r sdklog.Record) string {
	return fmt.Sprintf("%s %s %v %v", r.Body().Emit(), r.EventName(), attrsOf(r), r.SeverityText())
}

// scrubbedLogger logs through LogHandler to a scrubbed in-memory exporter,
// with base (stderr in production) beside it.
func scrubbedLogger(base slog.Handler, min slog.Level) (*slog.Logger, *memLogs) {
	mem := &memLogs{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(ScrubLogs(mem))))
	return slog.New(LogHandler(base, lp, min)), mem
}

// TestScrubLogs: what code logs leaves only through the allowlist, its
// text scrubbed.
func TestScrubLogs(t *testing.T) {
	const secret = "wsl_9f8e7d6c5b4a39281706f5e4d3c2b1a0"
	const code = "k3f9x2abq7mn" // an invite code: too short for scrubText
	var raw bytes.Buffer
	log, mem := scrubbedLogger(slog.NewJSONHandler(&raw, nil), slog.LevelInfo)
	log.With("tenant", "t-"+code, "link", secret).Warn("streams: failed for https://app.example.com/v1/stream?link="+secret+" from someone@example.com",
		"method", "GET",
		"route", "/api/apps/v1/agent/sessions/{handle}/inbox",
		"path", "/api/v1/invites/"+code,
		"status", 500,
		"ms", 12,
		"took", 3*time.Second,
		"app", "agentchat",
		"user", "u-"+code,
		"code", code,
		"client", "curl "+code,
		"why", `refused "`+code+`" from 203.0.113.9`,
		"err", errors.New(`Post "https://app.example.com/hook?key=`+secret+`": EOF`),
		slog.Group("req", "app", code),
		"app_list", []string{code},
	)
	if !strings.Contains(raw.String(), code) || !strings.Contains(raw.String(), secret) {
		t.Fatalf("the base handler didn't get the line as logged: %s", raw.String())
	}
	rs := mem.get()
	if len(rs) != 1 {
		t.Fatalf("%d records exported, want 1", len(rs))
	}
	r := rs[0]
	if d := dumpRecord(r); strings.Contains(d, secret) || strings.Contains(d, code) || strings.Contains(d, "someone@") || strings.Contains(d, "203.0.113.9") {
		t.Errorf("a secret got past the scrubber: %s", d)
	}
	if got, want := r.Body().AsString(), "streams: failed for https://app.example.com from [email]"; got != want {
		t.Errorf("body %q, want %q", got, want)
	}
	got := attrsOf(r)
	want := map[string]string{
		"method":            "GET",
		"route":             "/api/apps/v1/agent/sessions/{handle}/inbox",
		"status":            "500",
		"ms":                "12",
		"took":              "3000000000",
		"app":               "agentchat",
		"why":               `refused "…" from [ip]`,
		"exception.message": `Post "https://app.example.com": EOF`,
		"exception.type":    "*errors.errorString",
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g.Emit() != v {
			t.Errorf("%s = %q (present %v), want %q", k, g.Emit(), ok, v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s exported: it isn't on the allowlist", k)
		}
	}
	if r.Severity().String() != "WARN" {
		t.Errorf("severity %v", r.Severity())
	}
}

// TestLogHandlerLevels: stderr's handler gets what it always did; the
// export gets min and above.
func TestLogHandlerLevels(t *testing.T) {
	var raw bytes.Buffer
	log, mem := scrubbedLogger(slog.NewTextHandler(&raw, &slog.HandlerOptions{Level: slog.LevelDebug}), slog.LevelWarn)
	log.Debug("d")
	log.Info("i")
	log.Warn("w")
	log.Error("e")
	if n := strings.Count(raw.String(), "msg="); n != 4 {
		t.Errorf("base got %d lines, want 4: %s", n, raw.String())
	}
	var bodies []string
	for _, r := range mem.get() {
		bodies = append(bodies, r.Body().AsString())
	}
	if strings.Join(bodies, ",") != "w,e" {
		t.Errorf("exported %v, want [w e]", bodies)
	}

	// Without a provider it's the base handler itself.
	base := slog.NewTextHandler(io.Discard, nil)
	if LogHandler(base, nil, slog.LevelInfo) != slog.Handler(base) {
		t.Error("LogHandler with no provider isn't the base handler")
	}
}

// TestLogsLinkToSpans: a line logged with a span's context carries its
// trace and span IDs.
func TestLogsLinkToSpans(t *testing.T) {
	log, mem := scrubbedLogger(slog.NewTextHandler(io.Discard, nil), slog.LevelInfo)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
	ctx, sp := tp.Tracer("test").Start(context.Background(), "op")
	log.InfoContext(ctx, "inside")
	sp.End()
	log.Info("outside")
	rs := mem.get()
	if len(rs) != 2 {
		t.Fatalf("%d records", len(rs))
	}
	if rs[0].TraceID() != sp.SpanContext().TraceID() || rs[0].SpanID() != sp.SpanContext().SpanID() {
		t.Errorf("record not linked to its span: %v/%v", rs[0].TraceID(), rs[0].SpanID())
	}
	if rs[1].TraceID().IsValid() {
		t.Errorf("a line without a span has trace %v", rs[1].TraceID())
	}
}

// TestStartLogs: the exporter is on only with an endpoint, off with
// OTEL_LOGS_EXPORTER=none, and posts to the general endpoint's path plus
// /v1/logs (an endpoint at /api/ingest takes /api/ingest/v1/logs, as it
// takes /api/ingest/v1/traces).
func TestStartLogs(t *testing.T) {
	ctx := context.Background()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	t.Setenv("OTEL_LOGS_EXPORTER", "")
	if lp, _, err := StartLogs(ctx, "wakeshift", "test"); lp != nil || err != nil {
		t.Errorf("no endpoint: provider %v, err %v", lp, err)
	}

	paths := make(chan string, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path + " " + r.Header.Get("x-api-key")
	}))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL+"/api/ingest")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-api-key=k")

	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	if lp, _, err := StartLogs(ctx, "wakeshift", "test"); lp != nil || err != nil {
		t.Errorf("OTEL_LOGS_EXPORTER=none: provider %v, err %v", lp, err)
	}
	t.Setenv("OTEL_LOGS_EXPORTER", "console")
	if _, _, err := StartLogs(ctx, "wakeshift", "test"); err == nil {
		t.Error("OTEL_LOGS_EXPORTER=console: no error")
	}

	t.Setenv("OTEL_LOGS_EXPORTER", "")
	lp, shutdown, err := StartLogs(ctx, "wakeshift", "test")
	if err != nil || lp == nil {
		t.Fatalf("provider %v, err %v", lp, err)
	}
	slog.New(LogHandler(slog.NewTextHandler(io.Discard, nil), lp, slog.LevelInfo)).Info("hello")
	if err := shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-paths:
		if p != "/api/ingest/v1/logs k" {
			t.Errorf("posted to %q, want /api/ingest/v1/logs with the header", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing exported")
	}
}
