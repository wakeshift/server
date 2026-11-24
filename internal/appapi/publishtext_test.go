package appapi

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/streams"
)

// A publish URL's text is the event's default text, the same line every
// SDK writes for an event defined without text (default-text.json).
func TestPublishTextIsTheDefaultText(t *testing.T) {
	raw, err := os.ReadFile("../../sdk/typescript/test/default-text.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name, Description, Want string
			Args                    json.RawMessage
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture.Cases) == 0 {
		t.Fatalf("fixture: %v", err)
	}
	for _, c := range fixture.Cases {
		if got := publishText(apps.EventType{Name: "x.y", Description: c.Description}, c.Args); got != c.Want {
			t.Errorf("%s: got %q, want %q", c.Name, got, c.Want)
		}
	}
	// No description: the event's name, never the payload.
	if got := publishText(apps.EventType{Name: "ci.finished"}, []byte(`{}`)); got != "ci.finished" {
		t.Errorf("no description: %q", got)
	}
}

// A ~130-byte payload posted to a publish URL, as Claude Code's Monitor (a
// lines-mode stream) prints it. Before, the subject was the event's name and
// the first 200 characters of the payload, so the payload came twice and the
// line ran past MonitorLineMax: the data was dropped for a preview. Now the
// subject is the event's default text, and the payload arrives whole as data.
func TestAPublishURLsPayloadArrivesWholeOnAMonitorLine(t *testing.T) {
	et := apps.EventType{
		Name:        "ci.finished",
		Description: "A CI run on a repo finished.",
		Arguments:   json.RawMessage(`{"type":"object","properties":{"repo":{"type":"string"}},"required":["repo"]}`),
		Topic:       "ev/ci.finished/{repo}", // as the SDKs make it
	}
	args := []byte(`{"repo":"acme-api"}`)
	payload := json.RawMessage(`{"conclusion":"failure","branch":"main","run":"https://ci.example.com/acme/api/runs/48213","failed_step":"integration tests (shard 3)"}`)
	if n := len(payload); n < 120 || n > 140 {
		t.Fatalf("the payload is %d bytes, want about 130", n)
	}
	topic, err := emitTopic(et, args, "")
	if err != nil {
		t.Fatal(err)
	}
	line := func(subject string) (map[string]any, int) {
		f := streams.Frame{Type: "event", Attempt: 1, Topic: topic, App: "Shipyard CI", Event: events.Event{
			ID: "5d2e8a4c-1b3f-4e6a-8c9d-0f1e2d3c4b5a", From: events.Party{Kind: "app", ID: "shipyard-ci"}, Subject: subject, Data: payload}}
		b := streams.MonitorLine(f)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil || len(b) > streams.MonitorLineMax {
			t.Fatalf("%d bytes, %v: %s", len(b), err, b)
		}
		return m, len(b)
	}

	// Before: "ci.finished: {…the payload…}", as webhookText wrote it.
	before, n := line(et.Name + ": " + string(payload))
	if before["truncated"] != true || before["data"] != nil || before["data_preview"] == nil {
		t.Fatalf("before (%d bytes): %v", n, before)
	}
	t.Logf("before: %d bytes, truncated, data dropped for a preview", n)

	// After: the default text, and the payload whole.
	subject := publishText(et, args)
	if subject != "A CI run on a repo finished (repo acme-api)" {
		t.Fatalf("subject: %q", subject)
	}
	after, n := line(subject)
	got, _ := json.Marshal(after["data"])
	var want any
	json.Unmarshal(payload, &want)
	wantJSON, _ := json.Marshal(want)
	if after["truncated"] != nil || after["data_preview"] != nil || string(got) != string(wantJSON) || after["subject"] != subject ||
		strings.Contains(subject, "failure") {
		t.Fatalf("after (%d bytes): %v", n, after)
	}
	t.Logf("after: %d bytes, the payload whole as data, %d bytes spare", n, streams.MonitorLineMax-n)
}
