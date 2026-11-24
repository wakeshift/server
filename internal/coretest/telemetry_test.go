package coretest

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/telemetry"
)

// TestTracesCarryNoCredentials: the core's credentials in URLs (stream
// links, publish URLs, invite codes, sign-in codes, Capy's registration
// links and webhook watch URLs) and headers are on the
// spans tracing makes, and none of them gets past telemetry.Scrub.
func TestTracesCarryNoCredentials(t *testing.T) {
	raw := tracetest.NewInMemoryExporter()
	scrubbed := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(raw), sdktrace.WithSyncer(telemetry.Scrub(scrubbed)))
	c := StartWith(t, func(o *core.Options) { o.Tracing = tp })

	// Each request carries its own secret, so a leak names its route.
	secrets := map[string]string{}
	send := func(method, path, secret string) {
		t.Helper()
		req, _ := http.NewRequest(method, c.URL+strings.ReplaceAll(path, "SECRET", secret), nil)
		req.Header.Set("Authorization", "Bearer hdr"+secret)
		req.Header.Set("Cookie", "session=ck"+secret)
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		secrets[secret] = method + " " + path
	}
	send("POST", "/v1/publish/SECRET", "pubtok3x9a")
	send("GET", "/v1/publish/SECRET", "pubget7k2m")
	send("POST", "/v1//publish/SECRET", "pubdbl4q8z")
	send("GET", "/v1/stream?link=SECRET", "lnkws5r1v")
	send("GET", "/v1/stream/poll?link=SECRET", "lnkpoll6t")
	send("POST", "/v1/stream/ack?link=SECRET", "lnkack8w3")
	send("GET", "/v1/stream/wait?link=SECRET", "lnkwait2p")
	send("GET", "/api/v1/invites/SECRET", "inv9h4kq")
	send("GET", "/auth/github/callback?code=SECRET&state=SECRET", "ghcode5n7")
	send("GET", "/auth/google/callback?code=SECRET&state=SECRET", "gocode3j8")
	send("GET", "/console/SECRET?token=SECRET", "spa6m2xc")
	// A Capy thread registers its webhook watch and gets a delivery there.
	capyWebhookCanaries(t, c, secrets)
	// A Grok Bot sets up its webhook and gets a delivery there.
	grokWebhookCanaries(t, c, secrets)

	// The test proves something only if the raw spans have the secrets
	// (those in paths: otelhttp records no query today, but a later
	// version or another library may).
	rawDump := fmt.Sprintf("%#v", raw.GetSpans())
	for _, s := range []string{"pubtok3x9a", "pubdbl4q8z", "inv9h4kq"} {
		if !strings.Contains(rawDump, s) {
			t.Fatalf("raw spans don't carry %s (%s): the test isn't seeing what tracing records", s, secrets[s])
		}
	}
	spans := scrubbed.GetSpans()
	if len(spans) < len(secrets) {
		t.Fatalf("%d spans for %d requests", len(spans), len(secrets))
	}
	for _, sp := range spans {
		dump := fmt.Sprintf("%#v", sp)
		for s, route := range secrets {
			if strings.Contains(dump, s) {
				t.Errorf("%s: its secret got past the scrubber: %s", route, dump)
			}
		}
	}
	var named bool
	for _, sp := range spans {
		named = named || sp.Name == "POST /v1/publish/{token}"
	}
	if !named {
		t.Errorf("no span named by its route; names: %v", names(spans))
	}
}

func names(spans tracetest.SpanStubs) []string {
	var out []string
	for _, sp := range spans {
		out = append(out, sp.Name)
	}
	return out
}

// TestTracesIgnoreCallersContext: the core is public, so a caller's
// traceparent neither decides sampling nor puts the request in the
// caller's trace, and its tracestate doesn't leave the process.
func TestTracesIgnoreCallersContext(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(telemetry.Scrub(exp)))
	c := StartWith(t, func(o *core.Options) { o.Tracing = tp })

	const callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	for _, flags := range []string{"00", "01"} {
		req, _ := http.NewRequest("GET", c.URL+"/api/v1/invites/abc", nil)
		req.Header.Set("traceparent", "00-"+callerTrace+"-00f067aa0ba902b7-"+flags)
		req.Header.Set("tracestate", "vendor=callerchosen7q")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("%d spans, want 2 (a caller's unsampled flag mustn't drop a request)", len(spans))
	}
	for _, sp := range spans {
		if sp.SpanContext.TraceID().String() == callerTrace || sp.Parent.IsValid() {
			t.Errorf("span joined the caller's trace: %v (parent %v)", sp.SpanContext.TraceID(), sp.Parent.TraceID())
		}
		if dump := fmt.Sprintf("%#v", sp); strings.Contains(dump, "callerchosen7q") {
			t.Errorf("the caller's tracestate got out: %s", dump)
		}
	}
}
