package coretest_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/mcpevents/event"
)

// SEP-3415 (Event Occurrences): an occurrence's eventId is the app's own
// ID for the event (its emit's id) when it gave one, for spec clients:
// the same on every subscription it reaches, and on a poll. The dialects
// whose webhook-id is the eventId (draft, openclaw, chatgpt) keep the
// delivery's own ID for both, so a webhook-id is never a value the app
// chose, and never shared by two subscriptions; the spec's webhook-id is
// its own per message whatever the eventId. An emit without an id, or with
// one that can't be an eventId, gets the delivery's ID everywhere, and a
// repeat of an id within a day is the same emit, delivered once.
func TestMCPEventsAppEventID(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app, endpoint := chatEndpoint(t, c, "chat")
	msgID := regexp.MustCompile(`^msg_[0-9a-f]{32}$`)
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

	type sub struct {
		user, client, ua, dialect string
		cb                        *fakeChatGPT
	}
	subs := []*sub{
		{user: "spec1", client: "acme-agent", ua: "acme-agent/2", dialect: event.DialectSpec},
		{user: "spec2", client: "acme-agent", ua: "acme-agent/2", dialect: event.DialectSpec},
		{user: "draft", client: "tanstack-ai-mcp", ua: "node", dialect: event.DialectDraft},
		{user: "vercel", client: "my-app", ua: "ai-sdk/2.0.97 runtime/node.js/22", dialect: event.DialectDraft},
		{user: "openclaw", client: "openclaw", ua: "undici", dialect: event.DialectOpenClaw},
		{user: "chatgpt", client: "openai-mcp", ua: "openai-mcp/1.0.0", dialect: event.DialectChatGPT},
	}
	for _, s := range subs {
		s.cb = newFakeChatGPT(t)
		p := subscribeParams("eng", s.cb.url(), s.cb.secret())
		p["_meta"] = map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": s.client, "version": "1"}}
		if res := rpcAs(t, endpoint, s.user, s.ua, nil, "events/subscribe", p); res.Error != nil {
			t.Fatalf("%s: subscribe: %+v", s.user, res.Error)
		}
	}
	// A poll subscription, from now.
	if res := draft(t, endpoint, "u1", "events/poll", pollParams("eng", nil, nil)); res.Error != nil || res.Result["cursor"] != "c1.0" {
		t.Fatalf("first poll: %+v", res)
	}

	emit := func(id any, text string) {
		t.Helper()
		body := map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"},
			"payload": map[string]any{"text": text, "from": "ada"}, "text": "message " + text}
		if id != nil {
			body["id"] = id
		}
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", body, nil); code != 200 {
			t.Fatalf("emit: %d %s", code, e)
		}
	}
	emit("evt_1MqqbKLz", "one")
	emit("evt_1MqqbKLz", "one") // a retry: the same emit
	emit(nil, "two")
	emit("has a space", "three")
	for _, s := range subs {
		waitFor(t, s.user+"'s three events", func() bool { return len(s.cb.got()) >= 3 })
	}
	webhookIDs := map[string]string{}
	for _, s := range subs {
		got := s.cb.got()
		if len(got) != 3 {
			t.Fatalf("%s: %d events, want 3 (the repeated id is one emit)", s.user, len(got))
		}
		for i, e := range got {
			if webhookIDs[e.ID] != "" {
				t.Fatalf("%s and %s share webhook-id %q", webhookIDs[e.ID], s.user, e.ID)
			}
			webhookIDs[e.ID] = s.user
			if e.ID == "evt_1MqqbKLz" || e.ID == "has a space" {
				t.Fatalf("%s: the app's id became the webhook-id", s.user)
			}
			want := ""
			if s.dialect == event.DialectSpec && i == 0 {
				want = "evt_1MqqbKLz"
			}
			switch {
			case want != "" && e.Body.EventID != want:
				t.Errorf("%s (%s), event %d: eventId %q, want the app's %q", s.user, s.dialect, i, e.Body.EventID, want)
			case want == "" && !uuid.MatchString(e.Body.EventID):
				t.Errorf("%s (%s), event %d: eventId %q, want the delivery's", s.user, s.dialect, i, e.Body.EventID)
			}
			if s.dialect == event.DialectSpec {
				if !msgID.MatchString(e.ID) {
					t.Errorf("%s: spec webhook-id %q", s.user, e.ID)
				}
			} else if e.ID != e.Body.EventID || !uuid.MatchString(e.ID) {
				t.Errorf("%s (%s): webhook-id %q, eventId %q: want both the delivery's ID", s.user, s.dialect, e.ID, e.Body.EventID)
			}
		}
	}

	// A poll: the app's id, then the delivery's.
	res := pollUntil(t, endpoint, "c1.0", 3, nil)
	var ids []string
	for _, e := range res["events"].([]any) {
		ids = append(ids, fmt.Sprint(e.(map[string]any)["eventId"]))
	}
	if len(ids) != 3 || ids[0] != "evt_1MqqbKLz" || !uuid.MatchString(ids[1]) || !uuid.MatchString(ids[2]) {
		t.Fatalf("polled eventIds: %v", ids)
	}
}
