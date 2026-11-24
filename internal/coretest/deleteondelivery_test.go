package coretest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Delete on delivery (wakeshift.EventDef.DeleteOnDelivery): an occurrence's
// content is deleted from every table once it's delivered, through each
// path; an event that doesn't ask keeps it.

const (
	secretText = "the launch code is 1234"
	plainText  = "lunch is at noon"
)

// newVaultApp is an app with two event types on the same arguments:
// chat.message, deleted on delivery, and chat.note, kept. Its MCP endpoint
// serves both (Attach) for native subscribers.
func newVaultApp(t *testing.T, c *coretest.Core) (*sdkEventsApp, *wakeshift.Event[chanArgs, chanMsg]) {
	t.Helper()
	s := &sdkEventsApp{app: bareApp(t, c, "vault")}
	text := func(m chanMsg, a chanArgs) string { return m.From + " in #" + a.Channel + ": " + m.Text }
	var err error
	if s.message, err = wakeshift.DefineEvent(s.app, "chat.message", wakeshift.EventDef[chanArgs, chanMsg]{
		Description: "A new message in a channel.", Text: text, DeleteOnDelivery: true, Public: true,
	}); err != nil {
		t.Fatal(err)
	}
	note, err := wakeshift.DefineEvent(s.app, "chat.note", wakeshift.EventDef[chanArgs, chanMsg]{Description: "A note in a channel.", Text: text, Public: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.app.PublishManifest(ctx, wakeshift.Manifest{Description: "Messages that aren't kept."}); err != nil {
		t.Fatal(err)
	}
	// The flag reached the core with the event type.
	a, err := c.Apps.Get(ctx, "vault")
	if err != nil {
		t.Fatal(err)
	}
	if et, err := a.Manifest.EventType("chat.message"); err != nil || !et.DeleteOnDelivery {
		t.Fatalf("chat.message in the core's manifest: %+v %v", et, err)
	}
	if et, _ := a.Manifest.EventType("chat.note"); et.DeleteOnDelivery {
		t.Fatal("chat.note is flagged")
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "vault", Version: "1"}, nil)
	attached := s.app.Attach(srv, wakeshift.AttachOptions{Identify: identifyUser})
	s.mcp = serveAttached(t, srv, attached, tokenIsUser)
	return s, note
}

// tablesHolding names every table with a row (visible in the workspace:
// its own rows and platform-level ones) whose text holds needle.
func tablesHolding(t *testing.T, c *coretest.Core, tenant, needle string) []string {
	t.Helper()
	var out []string
	err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, _ := tx.Query(ctx, `SELECT table_name::text FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY 1`)
		tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, name := range tables {
			var found bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+pgx.Identifier{name}.Sanitize()+` x WHERE strpos(x::text, $1) > 0)`, needle).Scan(&found); err != nil {
				return err
			}
			if found {
				out = append(out, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// platformCount counts rows of a platform-level table.
func platformCount(t *testing.T, c *coretest.Core, query string) int {
	t.Helper()
	var n int
	if err := c.DB.Pool.QueryRow(ctx, query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A conversation's messages wait for its listener (a Claude Code Monitor's
// wait, here): the flagged one's content is gone from every table once
// handed over and confirmed, the other's is kept. Before that, nothing is deleted from the
// workspace (only the publish's own copy, once copied). A re-publish with
// the same ID is still a duplicate, and a later subscriber's replay gets
// nothing rather than an empty message.
func TestDeleteOnDeliveryHeldMessages(t *testing.T) {
	c := coretest.Start(t)
	s, note := newVaultApp(t, c)
	call := map[string]any{"user": "u1", "client": "claude-code"}
	var sub eventSubResult
	if code, e := appAPI(t, c, s.app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call,
		"name": "chat.message", "arguments": map[string]any{"channel": "eng"}}, &sub); code != 200 {
		t.Fatalf("subscribe: %d %s", code, e)
	}
	call["session"] = sub.Session
	if code, e := appAPI(t, c, s.app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call,
		"name": "chat.note", "arguments": map[string]any{"channel": "eng"}}, nil); code != 200 {
		t.Fatalf("subscribe to notes: %d %s", code, e)
	}
	tenant := userTenant(t, c, "vault", "u1")

	if got, err := s.message.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "ada", Text: secretText}, wakeshift.WithID("m1")); err != nil || got.Matched != 1 {
		t.Fatalf("emit: %+v %v", got, err)
	}
	if _, err := note.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "ada", Text: plainText}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both queued", func() bool { return deliveryCount(t, c, tenant) == 2 })
	// Undelivered, it's kept in the workspace; the publish's copy went once
	// it was copied there.
	if got := tablesHolding(t, c, tenant, secretText); strings.Join(got, ",") != "events" {
		t.Fatalf("before delivery, the message is in: %v", got)
	}

	if r, _ := annotate(s.app, caller{User: "u1", Client: "claude-code", Session: sub.Session}); strings.Contains(r.Text, "ada") {
		t.Fatalf("a result handed it over: %q", r.Text)
	}
	if got := takeByWait(t, sub.Addition.Text); !strings.Contains(got, secretText) || !strings.Contains(got, plainText) {
		t.Fatalf("the wait: %q", got)
	}
	if got := tablesHolding(t, c, tenant, secretText); len(got) != 0 {
		t.Fatalf("delivered, the message is still in: %v", got)
	}
	if got := tablesHolding(t, c, tenant, plainText); len(got) == 0 {
		t.Fatal("the note, which doesn't ask, was deleted")
	}
	// Metadata stays: the event, its delivery, and the activity's record.
	if n := count(t, c, tenant, `SELECT count(*) FROM events WHERE redacted_at IS NOT NULL AND subject = '' AND data IS NULL`); n != 1 {
		t.Fatalf("redacted events: %d", n)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE state = 'acked'`); n != 2 {
		t.Fatalf("delivered: %d", n)
	}
	if r, _ := annotate(s.app, caller{User: "u1", Client: "claude-code", Session: sub.Session}); strings.Contains(r.Text, "ada") {
		t.Fatalf("shown again: %q", r.Text)
	}

	// The same ID again is a duplicate, matched as before, and wakes no one.
	if got, err := s.message.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "ada", Text: secretText}, wakeshift.WithID("m1")); err != nil || !got.Duplicate || got.Matched != 1 {
		t.Fatalf("a repeated emit: %+v %v", got, err)
	}
	// A later subscriber asking for a replay gets nothing: the latest
	// message's content is gone.
	if code, e := appAPI(t, c, s.app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u2", "client": "strict-agent"},
		"name": "chat.message", "arguments": map[string]any{"channel": "eng"}, "replay": "1h"}, nil); code != 200 {
		t.Fatalf("u2 subscribe: %d %s", code, e)
	}
	time.Sleep(200 * time.Millisecond)
	u2 := userTenant(t, c, "vault", "u2")
	if n := deliveryCount(t, c, u2); n != 0 {
		t.Fatalf("u2 replayed %d", n)
	}
	if n := deliveryCount(t, c, tenant); n != 2 {
		t.Fatalf("u1's deliveries: %d", n)
	}
	if got := tablesHolding(t, c, u2, secretText); len(got) != 0 {
		t.Fatalf("u2's workspace holds the message: %v", got)
	}
	// A copy still to be made of a publish whose content is gone makes
	// nothing, rather than an empty message.
	if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO topic_fanout (app_id, source_id, tenant_id, subscriptions, created_at)
		VALUES ('vault', 'm1', $1, '{}', now() - interval '1 hour')`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := appapi.FinishFanouts(ctx, c.DB); err != nil {
		t.Fatal(err)
	}
	if n := platformCount(t, c, `SELECT count(*) FROM topic_fanout`); n != 0 {
		t.Fatalf("copies left: %d", n)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM events WHERE type LIKE 'topics.%'`); n != 2 {
		t.Fatalf("events: %d", n)
	}
}

// A publish nobody got keeps its content for replay: a subscriber that
// asks gets it, and then it's gone there too.
func TestDeleteOnDeliveryReplay(t *testing.T) {
	c := coretest.Start(t)
	s, _ := newVaultApp(t, c)
	if got, err := s.message.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "ada", Text: secretText}); err != nil || got.Matched != 0 {
		t.Fatalf("emit: %+v %v", got, err)
	}
	if n := platformCount(t, c, `SELECT count(*) FROM topic_events WHERE text <> '' AND redacted_at IS NULL`); n != 1 {
		t.Fatalf("the unmatched publish: %d", n)
	}
	call := map[string]any{"user": "u1", "client": "claude-code"}
	var sub eventSubResult
	if code, e := appAPI(t, c, s.app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call,
		"name": "chat.message", "arguments": map[string]any{"channel": "eng"}, "replay": "1h"}, &sub); code != 200 {
		t.Fatalf("subscribe: %d %s", code, e)
	}
	tenant := userTenant(t, c, "vault", "u1")
	waitFor(t, "the replay queued", func() bool { return deliveryCount(t, c, tenant) == 1 })
	if n := platformCount(t, c, `SELECT count(*) FROM topic_events WHERE text = '' AND data IS NULL AND redacted_at IS NOT NULL`); n != 1 {
		t.Fatalf("the publish's copy after the replay: %d", n)
	}
	if got := takeByWait(t, sub.Addition.Text); !strings.Contains(got, secretText) {
		t.Fatalf("the replay: %q", got)
	}
	if got := tablesHolding(t, c, tenant, secretText); len(got) != 0 {
		t.Fatalf("delivered, the message is still in: %v", got)
	}
}

// Through an MCP Events webhook (ChatGPT, a draft client), the content
// goes once the receiver acknowledges it (a 2xx).
func TestDeleteOnDeliveryWebhook(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	s, note := newVaultApp(t, c)
	cb := newFakeChatGPT(t)
	for _, name := range []string{"chat.message", "chat.note"} {
		params := map[string]any{"name": name, "arguments": map[string]any{"channel": "eng"},
			"delivery": map[string]any{"mode": "webhook", "url": cb.url(), "secret": cb.secret()}, "ttlMs": 3600000}
		if _, code, msg := s.call(t, "u1", draftUA, "events/subscribe", params); code != 0 {
			t.Fatalf("subscribe %s: %d %s", name, code, msg)
		}
	}
	tenant := userTenant(t, c, "vault", "u1")
	if _, err := s.message.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "ada", Text: secretText}); err != nil {
		t.Fatal(err)
	}
	if _, err := note.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "ada", Text: plainText}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both delivered", func() bool { return len(cb.got()) == 2 })
	// Each goes to its own webhook, so in either order.
	got := cb.got()
	if got[0].Body.Name != "chat.message" {
		got[0], got[1] = got[1], got[0]
	}
	if e := got[0]; e.Body.Name != "chat.message" || e.Body.Data["text"] != secretText {
		t.Fatalf("delivered: %+v", e.Body)
	}
	waitFor(t, "the message's content deleted", func() bool { return len(tablesHolding(t, c, tenant, secretText)) == 0 })
	if got := tablesHolding(t, c, tenant, plainText); len(got) == 0 {
		t.Fatal("the note, which doesn't ask, was deleted")
	}
	// Each is acknowledged once its webhook's 2xx is back, which can be
	// after the other's content went.
	waitFor(t, "both acknowledged", func() bool {
		return count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE state = 'acked'`) == 2
	})
}

// A publish nobody got keeps its content for its day (for replay), then
// the expiry pass deletes it; one that doesn't ask is left alone.
func TestDeleteOnDeliveryUnmatchedExpires(t *testing.T) {
	c := coretest.Start(t)
	s, note := newVaultApp(t, c)
	if _, err := s.message.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "ada", Text: secretText}); err != nil {
		t.Fatal(err)
	}
	if _, err := note.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "ada", Text: plainText}); err != nil {
		t.Fatal(err)
	}
	expire := func() int64 {
		n, err := appapi.ExpirePublishContent(ctx, c.DB)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := expire(); n != 0 {
		t.Fatalf("expired within its day: %d", n)
	}
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE topic_events SET published_at = now() - interval '25 hours'`); err != nil {
		t.Fatal(err)
	}
	if n := expire(); n != 1 {
		t.Fatalf("expired %d", n)
	}
	if n := platformCount(t, c, `SELECT count(*) FROM topic_events WHERE text = '' AND data IS NULL AND redacted_at IS NOT NULL`); n != 1 {
		t.Fatalf("redacted publishes: %d", n)
	}
	if n := platformCount(t, c, `SELECT count(*) FROM topic_events WHERE strpos(text, 'lunch') > 0`); n != 1 {
		t.Fatal("the note's publish lost its content")
	}
}

// Any publish over a day old goes in the expiry pass, even if its app
// never publishes again (which would otherwise delete it); a newer one
// stays, for replay.
func TestOldPublishesExpire(t *testing.T) {
	c := coretest.Start(t)
	_, note := newVaultApp(t, c)
	if _, err := note.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "ada", Text: plainText}); err != nil {
		t.Fatal(err)
	}
	expire := func() int64 {
		n, err := appapi.ExpireOldPublishes(ctx, c.DB, 100)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := expire(); n != 0 {
		t.Fatalf("expired within its day: %d", n)
	}
	// The app publishes nothing more.
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE topic_events SET published_at = now() - interval '25 hours'`); err != nil {
		t.Fatal(err)
	}
	if n := expire(); n != 1 {
		t.Fatalf("expired %d, want the old publish", n)
	}
	if n := platformCount(t, c, `SELECT count(*) FROM topic_events`); n != 0 {
		t.Fatalf("publishes left: %d", n)
	}
	// A new one stays for its day.
	if _, err := note.Emit(ctx, chanArgs{Channel: "eng"}, chanMsg{From: "bob", Text: "a fresh note"}); err != nil {
		t.Fatal(err)
	}
	if n := expire(); n != 0 || platformCount(t, c, `SELECT count(*) FROM topic_events`) != 1 {
		t.Fatalf("a new publish expired: %d", n)
	}
}
