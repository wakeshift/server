package coretest_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// An event's publish URL emits that event with the arguments it was made
// for: its body is the payload, checked against the event's schema; it
// takes the same idempotency keys, size and rate limits as a topic's;
// it stops at once when revoked, or when the app stops offering the
// event; and the token never reaches the log.
func TestEventPublishURL(t *testing.T) {
	var logs lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	type channel struct {
		Channel string `json:"channel"`
	}
	type message struct {
		Text string `json:"text"`
		From string `json:"from"`
	}
	msg, err := wakeshift.DefineEvent(app, "chat.message", wakeshift.EventDef[channel, message]{
		Description: "A message in a channel.", Public: true,
		Text: func(m message, _ channel) string { return m.From + ": " + m.Text },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Serving its tools, the app merges its definition into the core's
	// manifest, in place of chatEvents', the first time it needs it: before
	// the subscription, which is then to it.
	app.Attach(mcp.NewServer(&mcp.Implementation{Name: "chat", Version: "1"}, nil), wakeshift.AttachOptions{})
	if _, err := msg.Status(ctx, channel{Channel: "eng"}); err != nil {
		t.Fatal(err)
	}
	var sub eventSubResult
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"},
		"name": "chat.message", "arguments": json.RawMessage(`{"channel":"eng"}`)}, &sub); code != 200 {
		t.Fatalf("subscribe: %d", code)
	}
	tenant := userTenant(t, c, "chat", "u1")

	pu, err := msg.CreatePublishURL(ctx, channel{Channel: "eng"})
	if err != nil || !strings.HasPrefix(pu.URL, c.URL+"/v1/publish/wsp_") || pu.Event != "chat.message" || pu.ID == "" {
		t.Fatalf("publish URL: %+v %v", pu, err)
	}
	token := pu.URL[strings.LastIndex(pu.URL, "/")+1:]

	// A payload that matches: delivered as the event, once per key.
	hdr := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "k1"}
	if code, out := post(t, pu.URL, `{"text": "deployed", "from": "ci"}`, hdr); code != 202 || out["matched"] != 1.0 || out["text_trimmed"] != nil {
		t.Fatalf("POST: %d %v", code, out)
	}
	if code, out := post(t, pu.URL, `{"text": "deployed", "from": "ci"}`, hdr); code != 202 || out["duplicate"] != true {
		t.Fatalf("POST again: %d %v", code, out)
	}
	waitFor(t, "the delivery", func() bool { return deliveryCount(t, c, tenant) == 1 })
	// The agent reads the event's default text (its description with the
	// URL's arguments: text doesn't run) and the payload whole, as data.
	var subject string
	var data []byte
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT e.subject, e.data FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id`).Scan(&subject, &data)
	}); err != nil || subject != "A message in a channel (channel eng)" || !strings.Contains(string(data), `"from": "ci"`) {
		t.Fatalf("delivered %q %s %v", subject, data, err)
	}
	// The sender's delivery ID (here its Idempotency-Key), without
	// Wakeshift's prefix, is the upstream ID a spec client gets as eventId.
	var upstream string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(e.attrs->>'app_event_id', '') FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id`).Scan(&upstream)
	}); err != nil || upstream != "k1" {
		t.Fatalf("the upstream ID: %q %v", upstream, err)
	}

	// Anything else is refused, and nothing is emitted.
	for _, body := range []string{`{"text": 7, "from": "ci"}`, `{"from": "ci"}`, `plain text`, `[1, 2]`, ``} {
		if code, out := post(t, pu.URL, body, nil); code != 400 || out["error"].(map[string]any)["code"] != "bad_payload" {
			t.Fatalf("POST %q: %d %v", body, code, out)
		}
	}
	if code, _ := post(t, pu.URL, `{"text": "`+strings.Repeat("x", 64<<10)+`", "from": "ci"}`, nil); code != 413 {
		t.Fatalf("too large: %d", code)
	}
	if n := deliveryCount(t, c, tenant); n != 1 {
		t.Fatalf("%d deliveries after refused posts, want 1", n)
	}

	// Made only for an event the app offers, with arguments that name a
	// topic; a personal event's for one subscriber.
	for _, tc := range []struct {
		body map[string]any
		code string
	}{
		{map[string]any{"name": "chat.nope", "arguments": map[string]any{"channel": "eng"}}, "unknown_event"},
		{map[string]any{"name": "chat.message", "arguments": map[string]any{}}, "bad_arguments"},
		{map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"}, "to": "u1"}, "bad_arguments"},
	} {
		if _, code := appAPI(t, c, app, "/api/apps/v1/events/publish-urls", tc.body, nil); code != tc.code {
			t.Fatalf("%v: %q, want %q", tc.body, code, tc.code)
		}
	}

	// Revoked: 410 at once. Another app can't revoke it.
	other := eventsApp(t, c, "other")
	if err := other.RevokePublishURL(ctx, pu.ID); !wakeshift.IsCode(err, "unknown_publish_url") {
		t.Fatalf("another app revoking: %v", err)
	}
	if err := app.RevokePublishURL(ctx, pu.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := post(t, pu.URL, `{"text": "late", "from": "ci"}`, nil); code != 410 {
		t.Fatalf("after revoking: %d", code)
	}
	// The app's log names the event, as making it did, never the topic
	// inside the core (a personal event's is derived from its subscriber).
	var detail map[string]string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT detail FROM app_log WHERE app_id = 'chat' AND action = $1`, apps.LogPublishURLRevoked).Scan(&detail); err != nil ||
		detail["event"] != "chat.message" || detail["publish_url"] != pu.ID || detail["topic"] != "" {
		t.Fatalf("revoked's log entry: %v %v", detail, err)
	}

	// Rate limited like a topic's: 60 posts a minute.
	busy, err := msg.CreatePublishURL(ctx, channel{Channel: "busy"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 60 {
		if code, _ := post(t, busy.URL, `{"text": "x", "from": "ci"}`, nil); code != 202 {
			t.Fatalf("post %d: %d", i, code)
		}
	}
	if code, _ := post(t, busy.URL, `{"text": "x", "from": "ci"}`, nil); code != 429 {
		t.Fatalf("over the limit: %d", code)
	}

	// The app stops offering the event: its URLs stop too.
	if err := c.Apps.SetManifest(ctx, "chat", apps.Manifest{}); err != nil {
		t.Fatal(err)
	}
	if code, out := post(t, busy.URL, `{"text": "x", "from": "ci"}`, nil); code != 410 || out["error"].(map[string]any)["code"] != "unknown_event" {
		t.Fatalf("after the event left the manifest: %d %v", code, out)
	}

	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), busy.URL) {
		t.Fatal("the log has a publish token")
	}
}
