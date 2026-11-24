package coretest_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
)

// How an event subscription ends, for its conversation: who ended it
// decides whether it's told and whether what waits still arrives, and its
// last one ending ends its listener.

// Unsubscribing ends the conversation's command (its link is revoked) and
// its presence at once, with no grace: subscribing again right after gives
// a command to start (not "already running"), and that command gets the
// next emit. In testing, Claude Code was told its Monitor was already
// running, retried the revoked link, and stopped.
func TestWatchAgainAfterUnwatchRearms(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "rae", Client: "claude-code"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	mon := monitorRE.FindStringSubmatch(sub.Addition.Text)
	if mon == nil {
		t.Fatalf("no command: %s", sub.Addition.Text)
	}
	first := readLines(t, mon[1])
	waitFor(t, "connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	if n, err := unsubscribe(app, call, "ci/acme"); err != nil || n != 1 {
		t.Fatalf("unsubscribe: %d %v", n, err)
	}
	if _, err := first.next(5 * time.Second); err == nil || err.Error() != "closed: unsubscribed" ||
		!strings.Contains(first.closedNote, "Nothing more to do") {
		t.Fatalf("the Monitor after unsubscribing: %v (%s)", err, first.closedNote)
	}
	if st, _ := noteStatus(app, "ci/acme"); st.Connected != 0 || st.Subscriptions != 0 {
		t.Fatalf("still counted after unsubscribing: %+v", st)
	}
	again, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil || strings.Contains(again.Addition.Text, "already running") {
		t.Fatalf("subscribing again: %+v %v", again, err)
	}
	fresh := monitorRE.FindStringSubmatch(again.Addition.Text)
	if fresh == nil || fresh[2] == mon[2] {
		t.Fatalf("no fresh command (or the revoked link again): %s", again.Addition.Text)
	}
	lines := readLines(t, fresh[1])
	waitFor(t, "connected again", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	emitNote(app, []string{"ci/acme"}, note{Text: "after subscribing again"})
	lines.expect("after subscribing again")
}

// The same for a background command (Cursor): after unsubscribing,
// subscribing again gives the command to start, not "keep it running".
func TestCursorWatchAgainAfterUnwatch(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "cy", Client: "Cursor", Harness: "cursor", Conversation: "4f0c9a52-6f0e-4bde-8a43-1d0a7e39d2a1"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	m := shellRE.FindStringSubmatch(sub.Addition.Text)
	if m == nil {
		t.Fatalf("no command: %s", sub.Addition.Text)
	}
	var shell struct{ Command string }
	if err := json.Unmarshal([]byte(m[1]), &shell); err != nil {
		t.Fatal(err)
	}
	first, _ := runMonitor(t, shell.Command)
	waitFor(t, "connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	if _, err := unsubscribe(app, call, "ci/acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.next(5 * time.Second); err == nil || err.Error() != "closed: unsubscribed" {
		t.Fatalf("the command after unsubscribing: %v", err)
	}
	again, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil || strings.Contains(again.Addition.Text, "already running") || shellRE.FindStringSubmatch(again.Addition.Text) == nil {
		t.Fatalf("subscribing again: %+v %v", again, err)
	}
}

// Unsubscribing from one of several leaves the stream to the others, and
// drops only what waited for the one it left: the conversation asked to
// stop, so nothing more of it arrives.
func TestUnsubscribingFromOneOfSeveral(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "ada", Client: "claude-code"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	if _, err := subscribe(app, call, "ci/other", subOpts{}); err != nil {
		t.Fatal(err)
	}
	mon := monitorRE.FindStringSubmatch(sub.Addition.Text)
	if mon == nil {
		t.Fatalf("no command: %s", sub.Addition.Text)
	}
	// Emitted while nothing listens: both wait.
	for _, topic := range []string{"ci/acme", "ci/other"} {
		if p, _ := emitNote(app, []string{topic}, note{Text: "failed on " + topic}); p.Matched != 1 {
			t.Fatalf("matched %d", p.Matched)
		}
	}
	if n, err := unsubscribe(app, call, "ci/other"); err != nil || n != 1 {
		t.Fatalf("unsubscribe: %d %v", n, err)
	}
	lines := readLines(t, mon[1])
	lines.expect("failed on ci/acme")
	lines.expectNothing()
	emitNote(app, []string{"ci/acme"}, note{Text: "still here"})
	lines.expect("still here")
}

// An app ending subscriptions by ID delivers what was emitted to a
// conversation that wasn't connected, then a notice for each; once its
// last one has ended, its stream ends, saying the app ended it and that
// there's nothing more to do.
func TestEndingDeliversWhatsWaitingFirst(t *testing.T) {
	c := coretest.Start(t)
	c.Streams.PingEvery = 100 * time.Millisecond
	app := noteApp(t, c, "ci")
	call := caller{User: "ada", Client: "claude-code"}
	sub, _ := subscribe(app, call, "ci/acme", subOpts{})
	call.Session = sub.Session
	other, _ := subscribe(app, call, "ci/other", subOpts{})
	mon := monitorRE.FindStringSubmatch(sub.Addition.Text)
	for _, topic := range []string{"ci/acme", "ci/other"} {
		if p, _ := emitNote(app, []string{topic}, note{Text: "failed on " + topic}); p.Matched != 1 {
			t.Fatalf("matched %d", p.Matched)
		}
	}
	for _, id := range []string{sub.ID, other.ID} {
		if ok, err := endNote(app, "ada", id); err != nil || !ok {
			t.Fatalf("end: %v %v", ok, err)
		}
	}
	// The Monitor starts afterwards.
	lines := readLines(t, mon[1])
	got := []string{}
	for range 4 {
		f, err := lines.next(5 * time.Second)
		if err != nil {
			t.Fatalf("after %q: %v", got, err)
		}
		got = append(got, f.Event.Subject)
	}
	if got[0] != "failed on ci/acme" || got[1] != "failed on ci/other" ||
		got[2] != "Your subscription to note.two {a:ci,b:acme} in Ci ended (Ci ended it). Its events won't reach this conversation any more." ||
		got[3] != "Your subscription to note.two {a:ci,b:other} in Ci ended (Ci ended it). Its events won't reach this conversation any more." {
		t.Fatalf("arrived: %q", got)
	}
	if _, err := lines.next(5 * time.Second); err == nil || err.Error() != "closed: removed" ||
		!strings.Contains(lines.closedNote, "Ci ended your subscription to note.two {a:ci,b:") || !strings.Contains(lines.closedNote, "Nothing more to do") {
		t.Fatalf("after ending them: %v (%s)", err, lines.closedNote)
	}
	// Subscribing again gives a command to start: the old one has ended.
	again, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil || strings.Contains(again.Addition.Text, "already running") || monitorRE.FindStringSubmatch(again.Addition.Text) == nil {
		t.Fatalf("subscribing again: %+v %v", again, err)
	}
}

// A Codex thread's last unsubscribe closes its connector's stream (4001,
// so the connector exits and forgets the link), and the thread no longer
// counts as watching; subscribing again gives a new beacon.
func TestCodexUnsubscribeStopsItsConnector(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const thread = "019a14bd-bbbc-7b70-a723-0abb9f38118f"
	call := caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
	claims, err := beacon.Verify(tok, thread, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatalf("beacon: %v", err)
	}
	code, redeemed := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
	if code != 200 {
		t.Fatalf("redeem: %d %v", code, redeemed)
	}
	codex := dialStream(t, redeemed["url"].(string), true)
	waitFor(t, "connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	if n, err := unsubscribe(app, call, "ci/acme"); err != nil || n != 1 {
		t.Fatalf("unsubscribe: %d %v", n, err)
	}
	var closed websocket.CloseError
	if _, err := codex.next(5 * time.Second); !errors.As(err, &closed) || closed.Code != 4001 || closed.Reason != "unsubscribed" {
		t.Fatalf("the connector's stream after unsubscribing: %v", err)
	}
	again, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil || again.Addition.Meta[beacon.MetaKey] == nil {
		t.Fatalf("subscribing again: %+v %v", again, err)
	}
}

// An app that stopped a conversation listening (its own unwatch, as
// Agent Chat's does) and then ends its subscription owes it nothing more:
// what waited is dropped and there's no notice, so neither arrives after
// its next subscribe.
func TestEndingAfterUnwatchIsQuiet(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "ada", Client: "claude-code"}
	sub, _ := subscribe(app, call, "ci/acme", subOpts{})
	if p, _ := emitNote(app, []string{"ci/acme"}, note{Text: "before the unwatch"}); p.Matched != 1 {
		t.Fatalf("matched %d", p.Matched)
	}
	tenant, _, _ := noteRow(t, c, sub.ID)
	pending := func() (n int) {
		c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE state = 'pending'`).Scan(&n)
		})
		return n
	}
	waitFor(t, "the delivery", func() bool { return pending() == 1 })
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET watching = false`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := endNote(app, "ada", sub.ID); err != nil || !ok {
		t.Fatalf("end: %v %v", ok, err)
	}
	if n := pending(); n != 0 {
		t.Fatalf("%d deliveries still wait", n)
	}
	var notices int
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM events WHERE source = 'hub'`).Scan(&notices)
	}); err != nil {
		t.Fatal(err)
	}
	if notices != 0 {
		t.Fatalf("%d notices for a conversation that stopped listening", notices)
	}
}
