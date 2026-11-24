package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestScrubText(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`Post "https://app.example.com/hooks/events?secret=s3cr3t": dial tcp: refused`, `Post "https://app.example.com": dial tcp: refused`},
		{`fetching https://user:pw@example.com:8443/a/b?link=x#frag failed`, `fetching https://example.com:8443 failed`},
		{`invalid input syntax for type uuid: "not-a-uuid"`, `invalid input syntax for type uuid: "…"`},
		{`no account for someone@example.com`, `no account for [email]`},
		{`bad link wsl_9f8e7d6c5b4a39281706f5e4d3c2b1a0`, `bad link [redacted]`},
		{`tenant 3f2b9c1e-4d5a-4b6c-8d7e-9f0a1b2c3d4e not found`, `tenant 3f2b9c1e-4d5a-4b6c-8d7e-9f0a1b2c3d4e not found`},
		{`calling https://a-very-long-hostname-for-webhooks.example.com/x`, `calling https://a-very-long-hostname-for-webhooks.example.com`},
		{`GET /v1/publish/{token}`, `GET /v1/publish/{token}`},
		{`calling https://203.0.113.9:8443/hook?k=v failed`, `calling https://[ip]:8443 failed`},
		{`calling http://[2001:db8::1]/x`, `calling http://[ip]`},
		{`http: TLS handshake error from 203.0.113.9:51234: EOF`, `http: TLS handshake error from [ip]:51234: EOF`},
		{`http: panic serving [2001:db8::7]:443: boom`, `http: panic serving [[ip]]:443: boom`},
		{`dial tcp 10.1.2.3:5432 at 12:30:45`, `dial tcp [ip]:5432 at 12:30:45`},
		// A placeholder smuggled in the input can't pick another URL's
		// origin or panic.
		{"\x007\x00 and https://example.com/x", "7 and https://example.com"},
	} {
		if got := scrubText(c.in); got != c.want {
			t.Errorf("scrubText(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// TestScrubSpans: what an instrumentation library or our own code puts on a
// span leaves only through the allowlist, scrubbed.
func TestScrubSpans(t *testing.T) {
	// A link's token is long enough for scrubText to know; an invite code
	// (12 characters) isn't, so only the allowlist and names keep it in.
	const secret = "wsl_9f8e7d6c5b4a39281706f5e4d3c2b1a0"
	const code = "k3f9x2abq7mn"
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(Scrub(exp)))
	_, sp := tp.Tracer("test").Start(context.Background(), "GET /api/v1/invites/"+code,
		trace.WithLinks(trace.Link{Attributes: []attribute.KeyValue{attribute.String("link.note", secret)}}))
	sp.SetAttributes(
		attribute.String("http.route", "/v1/publish/{token}"),
		attribute.Int("http.response.status_code", 401),
		attribute.String("url.path", "/api/v1/invites/"+code),
		attribute.String("url.query", "link="+secret),
		attribute.String("url.full", "https://app.example.com/hook?key="+secret),
		attribute.String("client.address", "203.0.113.9"),
		attribute.String("user_agent.original", "agent "+secret),
		attribute.String("http.request.header.authorization", "Bearer "+secret),
		attribute.String("event.payload", "message from "+secret),
	)
	sp.RecordError(errors.New(`Post "https://app.example.com/hook?key=` + secret + `": EOF`))
	sp.AddEvent("note", trace.WithAttributes(attribute.String("text", secret)))
	sp.SetStatus(codes.Error, "refused "+secret)
	sp.End()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("%d spans", len(spans))
	}
	if dump := fmt.Sprintf("%#v", spans); strings.Contains(dump, secret) || strings.Contains(dump, code) || strings.Contains(dump, "203.0.113.9") {
		t.Fatalf("a secret or address got out: %s", dump)
	}
	got := map[attribute.Key]string{}
	for _, kv := range spans[0].Attributes {
		got[kv.Key] = kv.Value.Emit()
	}
	want := map[attribute.Key]string{
		"http.route":                "/v1/publish/{token}",
		"http.response.status_code": "401",
		"url.full":                  "https://app.example.com",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("attributes %v, want %v", got, want)
	}
	if n := spans[0].Name; n != "GET" {
		t.Errorf("name %q, want GET", n)
	}
	if d := spans[0].Status.Description; d != "refused [redacted]" {
		t.Errorf("status %q", d)
	}
	var msg string
	for _, e := range spans[0].Events {
		for _, kv := range e.Attributes {
			if kv.Key == "exception.message" {
				msg = kv.Value.AsString()
			}
		}
	}
	if msg != `Post "https://app.example.com": EOF` {
		t.Errorf("exception message %q", msg)
	}
}

func TestScrubName(t *testing.T) {
	for _, c := range []struct {
		name, route, want string
	}{
		{"POST /v1/publish/{token}", "/v1/publish/{token}", "POST /v1/publish/{token}"},
		{"POST /v1/publish/abc", "/v1/publish/{token}", "POST"},
		{"GET", "", "GET"},
		{"pipeline.run", "", "pipeline.run"},
		{"GET /join/k3f9x2abq7mn", "", "GET"},
		{"/v1/stream?link=x", "", "span"},
		{"x GET /v1/publish/{token}", "/v1/publish/{token}", "x"},
	} {
		exp := tracetest.NewInMemoryExporter()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(Scrub(exp)))
		_, sp := tp.Tracer("test").Start(context.Background(), c.name)
		if c.route != "" {
			sp.SetAttributes(attribute.String("http.route", c.route))
		}
		sp.End()
		if got := exp.GetSpans()[0].Name; got != c.want {
			t.Errorf("%q (route %q): %q, want %q", c.name, c.route, got, c.want)
		}
	}
}
