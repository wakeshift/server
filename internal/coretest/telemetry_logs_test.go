package coretest

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/telemetry"
)

// memLogs keeps the log records it's exported.
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

// dumpLog is everything a record exports but its resource and scope (ours).
func dumpLog(r sdklog.Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%s|%s|%s", r.Severity(), r.SeverityText(), r.EventName(), r.TraceID(), r.SpanID(), r.Body().Emit())
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		fmt.Fprintf(&b, "|%s=%s", kv.Key, kv.Value.Emit())
		return true
	})
	return b.String()
}

func logAttr(r sdklog.Record, key string) string {
	var v string
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		if string(kv.Key) == key {
			v = kv.Value.Emit()
			return false
		}
		return true
	})
	return v
}

// TestLogsCarryNoCredentials: the core's credentials in URLs (stream
// links, publish URLs, invite codes, sign-in codes, Capy's registration
// links and webhook watch URLs) and headers, an event's
// text and payload, and a user's ID, email and client reach the core's
// log lines or its requests, and none of them gets past
// telemetry.ScrubLogs. stderr's handler still gets every line as logged.
func TestLogsCarryNoCredentials(t *testing.T) {
	var raw syncBuffer
	exported := &memLogs{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(telemetry.ScrubLogs(exported))))
	prev := slog.Default()
	// Debug and up, both ways: the strictest export.
	slog.SetDefault(slog.New(telemetry.LogHandler(slog.NewJSONHandler(&raw, &slog.HandlerOptions{Level: slog.LevelDebug}), lp, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	c := StartWith(t, func(o *core.Options) { o.Tracing = sdktrace.NewTracerProvider() })

	// Each canary is its own, so a leak names where it came from.
	canaries := map[string]string{}
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
		canaries[secret] = method + " " + path
		canaries["hdr"+secret] = method + " " + path + " (Authorization)"
		canaries["ck"+secret] = method + " " + path + " (Cookie)"
	}
	send("POST", "/v1/publish/SECRET", "pubtok3x9a")
	send("GET", "/v1/publish/SECRET", "pubget7k2m")
	send("POST", "/v1//publish/SECRET", "pubdbl4q8z")
	send("GET", "/v1/stream?link=SECRET", "lnkws5r1v")
	send("GET", "/v1/stream/poll?link=SECRET", "lnkpoll6t")
	send("POST", "/v1/stream/ack?link=SECRET", "lnkack8w3")
	send("GET", "/v1/stream/wait?link=SECRET", "lnkwait2p")
	send("GET", "/api/v1/invites/SECRET", "inv9h4kq")
	send("POST", "/api/v1/invites/SECRET", "invpost5c")
	send("GET", "/auth/github/callback?code=SECRET&state=SECRET", "ghcode5n7")
	send("GET", "/auth/google/callback?code=SECRET&state=SECRET", "gocode3j8")
	send("GET", "/console/SECRET?token=SECRET", "spa6m2xc")
	send("GET", "/api/apps/v1/agent/sessions/wsc_SECRET/inbox", "hdl8v3pd")

	// A user signs in (logged by ID); an app's user subscribes from a
	// client (logged, with its claim) and the app emits to them.
	const email = "canary-k7q2@example.com"
	canaries[email] = "a user's email"
	u := c.SignIn(email)
	var s struct{ User struct{ ID string } }
	u.Do("GET", "/api/v1/session", nil, &s)
	if s.User.ID == "" {
		t.Fatal("no user ID")
	}
	canaries[s.User.ID] = "a user's ID"
	app := c.NoteApp("ci")
	call := Caller{User: "usr-canary9x", Client: "cli-canary4z", Harness: "hns-canary2w"}
	canaries["usr-canary9x"] = "an app's user ID"
	canaries["cli-canary4z"] = "an MCP client's name"
	canaries["canary2w"] = "a client's claimed harness"
	if _, err := Subscribe(app, call, "ci/acme/web", SubOpts{}); err != nil {
		t.Fatal(err)
	}
	canaries["txt-canary6h"] = "an event's text"
	canaries["pay-canary3m"] = "an event's payload"
	if _, err := EmitNote(app, []string{"ci/acme/web"}, Note{Text: "build failed: txt-canary6h", Data: map[string]any{"detail": "pay-canary3m"}}); err != nil {
		t.Fatal(err)
	}

	// A Capy thread registers its webhook watch and gets a delivery there:
	// the registration link and the watch URL are credentials.
	capyWebhookCanaries(t, c, canaries)
	// A Grok Bot sets up its webhook and gets a delivery there: the tokens,
	// challenges and the webhook's URL and key are credentials.
	grokWebhookCanaries(t, c, canaries)

	// The test proves something only if stderr gets the canaries the core
	// legitimately logs.
	for _, s := range []string{"spa6m2xc", "hdl8v3pd", s.User.ID, "cli-canary4z"} {
		if !strings.Contains(raw.String(), s) {
			t.Fatalf("stderr's lines don't carry %s (%s): the test isn't seeing what the core logs", s, canaries[s])
		}
	}
	// The watch URL gets the delivery before the deliverer logs it.
	records := exported.get()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !slices.ContainsFunc(records, capyDelivered); time.Sleep(50 * time.Millisecond) {
		records = exported.get()
	}
	var routed, signedIn, linked, hooked bool
	for _, r := range records {
		dump := dumpLog(r)
		for s, from := range canaries {
			if strings.Contains(dump, s) {
				t.Errorf("%s: got past the log scrubber: %s", from, dump)
			}
		}
		if r.Body().AsString() == "http" && logAttr(r, "route") == "/v1/publish/{token}" {
			routed = true
			linked = linked || r.TraceID().IsValid()
		}
		signedIn = signedIn || r.Body().AsString() == "auth: signed in"
		hooked = hooked || capyDelivered(r)
	}
	if len(records) < len(canaries)/3 {
		t.Fatalf("%d records exported", len(records))
	}
	if !routed || !signedIn || !hooked {
		t.Errorf("missing records: a request by its route %v, a sign-in %v, a Capy webhook delivery %v", routed, signedIn, hooked)
	}
	if !linked {
		t.Error("a request's log line isn't linked to its trace")
	}
}

// capyDelivered is the deliverer's record of the Capy watch URL's 202.
func capyDelivered(r sdklog.Record) bool {
	return r.Body().AsString() == "agent webhooks: delivered" && logAttr(r, "status") == "202"
}
