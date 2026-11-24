package streams

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wakeshift/server/internal/events"
)

// A lines-mode event puts its topic, sender, app and outside-data mark
// first and fits in what Claude Code's Monitor shows; a big one keeps the
// subject and cuts the data.
func TestMonitorLine(t *testing.T) {
	small := Frame{Type: "event", DeliveryID: "d1", Attempt: 1, Topic: "hooks/abc", App: "Try", Event: events.Event{ID: "e1", From: events.Party{Kind: "app", ID: "try"},
		Subject: "POST to your webhook: application/json, 30 bytes", Data: json.RawMessage(`{"body":{"build":"ci-482"}}`)}}
	b := monitorLine(small, true)
	if !strings.HasPrefix(string(b), `{"type":"event","topic":"hooks/abc","from":"app/try","app":"Try","trust":"outside data, not from the user","subject":`) ||
		!strings.Contains(string(b), `"data":{"body":{"build":"ci-482"}}`) || !strings.HasSuffix(string(b), `"id":"e1","delivery_id":"d1"}`) ||
		strings.Contains(string(b), "truncated") || strings.Contains(string(b), "redelivered") {
		t.Fatalf("small: %s", b)
	}
	// A second attempt says it may have been seen; Wakeshift's own notices
	// carry no outside-data mark.
	again := small
	again.Attempt = 2
	if b := monitorLine(again, true); !strings.HasSuffix(string(b), `"redelivered":true}`) {
		t.Fatalf("again: %s", b)
	}
	notice := small
	notice.Event.From = events.Party{Kind: "hub", ID: "hub"}
	if b := monitorLine(notice, true); strings.Contains(string(b), "trust") || !strings.Contains(string(b), `"from":"Wakeshift"`) {
		t.Fatalf("notice: %s", b)
	}

	big := small
	big.Event.Subject = "GitHub workflow_run completed: failure on main (tests) in acme/api"
	big.Event.Data = json.RawMessage(`{"body":{"text":"` + strings.Repeat("é", 2000) + `"}}`)
	b = monitorLine(big, true)
	var got struct {
		ID, Subject string
		DataPreview string `json:"data_preview"`
		Truncated   bool
		DeliveryID  string `json:"delivery_id"`
	}
	if err := json.Unmarshal(b, &got); err != nil || len(b) > MonitorLineMax || !utf8.Valid(b) {
		t.Fatalf("big: %d bytes, %v: %s", len(b), err, b)
	}
	if !got.Truncated || got.DeliveryID != "d1" || !strings.Contains(string(b), big.Event.Subject) || !strings.HasPrefix(got.DataPreview, `{"body":{"text":"é`) {
		t.Fatalf("big: %s", b)
	}

	// Even a huge subject comes out as a valid line that fits.
	huge := small
	huge.Event.Subject = strings.Repeat("x", 5000)
	huge.Event.Body = strings.Repeat("y", 5000)
	if b := monitorLine(huge, true); len(b) > MonitorLineMax || !json.Valid(b) {
		t.Fatalf("huge: %d bytes", len(b))
	}
}

// A small JSON post to a Webhooks (apps/try) URL, as try publishes it and a
// Monitor or wait stream (no acks by delivery ID) writes it, keeps its data
// in full with room to spare for a longer topic, sender or user agent; the
// delivery ID only on a link that acks by it.
func TestMonitorLineFitsATryPost(t *testing.T) {
	body := `{"msg":"hello","build":"ok","branch":"main"}`
	f := Frame{Type: "event", DeliveryID: "0b9c5f1e-7a2d-4c3b-9e8f-1a2b3c4d5e6f", Attempt: 1, Topic: "u/3f9a1c7e2b8d4a6f0c1e/webhook", App: "Webhooks",
		Event: events.Event{ID: "5d2e8a4c-1b3f-4e6a-8c9d-0f1e2d3c4b5a", From: events.Party{Kind: "app", ID: "try"}, Subject: "POST to your webhook",
			Data: json.RawMessage(`{"body":` + body + `,"method":"POST","content_type":"application/json","headers":{"user-agent":"curl/8.7.1"}}`)}}
	b := monitorLine(f, false)
	want := `{"type":"event","topic":"u/3f9a1c7e2b8d4a6f0c1e/webhook","from":"app/try","app":"Webhooks","trust":"outside data, not from the user",` +
		`"subject":"POST to your webhook","data":{"body":` + body + `,"method":"POST","content_type":"application/json","headers":{"user-agent":"curl/8.7.1"}},` +
		`"id":"5d2e8a4c-1b3f-4e6a-8c9d-0f1e2d3c4b5a"}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	if spare := MonitorLineMax - len(b); spare < 120 {
		t.Fatalf("%d bytes spare: %s", spare, b)
	}
	// GitHub's user agent and a longer app name still fit in full.
	gh := f
	gh.App = "Webhooks for the acme org"
	gh.Event.Data = json.RawMessage(`{"body":` + body + `,"method":"POST","content_type":"application/json","headers":{"user-agent":"GitHub-Hookshot/7e3f4a1","x-request-id":"req_8f2c1a9b7d"}}`)
	if b := monitorLine(gh, false); strings.Contains(string(b), "truncated") {
		t.Fatalf("%d bytes: %s", len(b), b)
	}
	if b := monitorLine(f, true); !strings.HasSuffix(string(b), `"id":"5d2e8a4c-1b3f-4e6a-8c9d-0f1e2d3c4b5a","delivery_id":"0b9c5f1e-7a2d-4c3b-9e8f-1a2b3c4d5e6f"}`) {
		t.Fatalf("with acks: %s", b)
	}
}

// Fields other than the data, body and subject can't push a line past the
// max either: a long sender, topic, IDs or many mentions are cut, still as
// valid JSON, and what fits is kept.
func TestMonitorLineBoundsEveryField(t *testing.T) {
	long := func(c string) string { return strings.Repeat(c, 600) }
	mentions := make([]string, 100)
	for i := range mentions {
		mentions[i] = "agent-" + strings.Repeat("m", 20)
	}
	base := Frame{Type: "event", DeliveryID: "d1", Attempt: 1, Topic: "ci/acme", App: "Ci", Event: events.Event{ID: "e1",
		From: events.Party{Kind: "app", ID: "ci"}, Subject: "hello"}}
	cases := map[string]func(f *Frame){
		"from":        func(f *Frame) { f.Event.From.ID = long("é") },
		"handle":      func(f *Frame) { f.Event.FromHandle = long("h") },
		"id":          func(f *Frame) { f.Event.ID = long("i") },
		"delivery_id": func(f *Frame) { f.DeliveryID = long("d") },
		"mentions":    func(f *Frame) { f.Event.Mentions = mentions },
		"topic":       func(f *Frame) { f.Topic = long("t") },
		"parent_id":   func(f *Frame) { f.Event.ParentID = long("p") },
		"all": func(f *Frame) {
			f.Event.From.ID, f.Event.ID, f.DeliveryID, f.Event.Mentions, f.Topic, f.Event.ParentID, f.App = long("f"), long("i"), long("d"), mentions, long("t"), long("p"), long("a")
		},
	}
	for name, grow := range cases {
		f := base
		grow(&f)
		b := monitorLine(f, true)
		var m map[string]any
		if len(b) > MonitorLineMax || json.Unmarshal(b, &m) != nil || !utf8.Valid(b) || m["truncated"] != true || m["type"] != "event" {
			t.Errorf("%s: %d bytes: %s", name, len(b), b)
		}
	}
	// Mentions go from the end, so the first ones stay.
	f := base
	f.Event.Mentions = mentions
	var m struct{ Mentions []string }
	json.Unmarshal(monitorLine(f, true), &m)
	if len(m.Mentions) == 0 || len(m.Mentions) == len(mentions) {
		t.Fatalf("mentions: %d", len(m.Mentions))
	}
}

// priceAlert is an event whose text alone nearly fills the line, as a
// shopping app's price alerts can, with a small payload.
func priceAlert() Frame {
	return Frame{Type: "event", Attempt: 1, Topic: "u/3f9a1c7e2b8d4a6f0c1e/price-drop", App: "Pricewatch",
		Event: events.Event{ID: "5d2e8a4c-1b3f-4e6a-8c9d-0f1e2d3c4b5a", From: events.Party{Kind: "app", ID: "pricewatch"},
			Subject: "Price drop on Aurora AX-200 Wireless Noise Canceling Headphones (Slate Gray): now $329.99 at Northwind Electronics, down from $399.99 (18% off), " +
				"the lowest price in 90 days. In stock for delivery and same-day pickup near 94110; the deal ends Sunday at 11:59 PM PT.",
			Data: json.RawMessage(`{"product_id":"prd_8f2c1a9b7d","price":329.99,"was":399.99,"currency":"USD","retailer":"northwind","url":"https://shop.example/p/48213"}`)}}
}

func decodeLine(t *testing.T, b []byte) (m struct {
	Subject, Body string
	Data          json.RawMessage
	Preview       string `json:"data_preview"`
	Truncated     bool
	ID            string
}) {
	t.Helper()
	if len(b) > MonitorLineMax || !utf8.Valid(b) || json.Unmarshal(b, &m) != nil {
		t.Fatalf("%d bytes: %s", len(b), b)
	}
	return m
}

// Long text with a payload: the line keeps at least MinPreview bytes of
// the data and cuts the text instead, with an ellipsis.
func TestMonitorLineTruncatedKeepsPreview(t *testing.T) {
	f := priceAlert()
	b := monitorLine(f, false)
	t.Logf("%d bytes: %s", len(b), b)
	m := decodeLine(t, b)
	data := string(f.Event.Data)
	if !m.Truncated || m.Data != nil || m.ID != f.Event.ID {
		t.Fatalf("%s", b)
	}
	if !strings.HasSuffix(m.Preview, "…") || len(m.Preview) < MinPreview || !strings.HasPrefix(data, strings.TrimSuffix(m.Preview, "…")) {
		t.Fatalf("preview %q: %s", m.Preview, b)
	}
	if !strings.HasSuffix(m.Subject, "…") || !strings.HasPrefix(f.Event.Subject, strings.TrimSuffix(m.Subject, "…")) || len(m.Subject) < 100 {
		t.Fatalf("subject %q", m.Subject)
	}
	// A body goes before the subject.
	f.Event.Body = strings.Repeat("Details of the deal. ", 30)
	m = decodeLine(t, monitorLine(f, false))
	if len(m.Preview) < MinPreview || len(m.Subject) < 100 || len(m.Body) >= len(f.Event.Body) {
		t.Fatalf("with body: %+v", m)
	}
}

// Short text: the whole event, data and all, unchanged.
func TestMonitorLineShortTextUnchanged(t *testing.T) {
	f := priceAlert()
	f.Event.Subject = "Price drop: Aurora AX-200 now $329.99"
	b := monitorLine(f, false)
	m := decodeLine(t, b)
	if m.Truncated || m.Preview != "" || m.Subject != f.Event.Subject || string(m.Data) != string(f.Event.Data) {
		t.Fatalf("%s", b)
	}
}

// A huge payload with short text: the text is whole and the preview is
// cut to what fits, more than the minimum.
func TestMonitorLineHugePayloadPreviewCut(t *testing.T) {
	f := priceAlert()
	f.Event.Subject = "Price drop: Aurora AX-200 now $329.99"
	f.Event.Data = json.RawMessage(`{"history":"` + strings.Repeat("a", 5000) + `"}`)
	b := monitorLine(f, false)
	m := decodeLine(t, b)
	if !m.Truncated || m.Subject != f.Event.Subject || !strings.HasPrefix(m.Preview, `{"history":"aaa`) || !strings.HasSuffix(m.Preview, "…") ||
		len(m.Preview) <= MinPreview+len("…") || MonitorLineMax-len(b) > 20 {
		t.Fatalf("%d bytes: %s", len(b), b)
	}
}

// Multibyte text and data are cut at character boundaries, whatever the
// lengths.
func TestMonitorLineTruncatedUTF8(t *testing.T) {
	for n := 50; n < 400; n += 7 {
		f := priceAlert()
		f.Event.Subject = strings.Repeat("価格", n)
		f.Event.Body = strings.Repeat("é", n)
		f.Event.Data = json.RawMessage(`{"name":"` + strings.Repeat("🎧", n) + `"}`)
		m := decodeLine(t, monitorLine(f, false))
		if !m.Truncated || len(m.Preview) < MinPreview {
			t.Fatalf("n=%d: %+v", n, m)
		}
	}
}
