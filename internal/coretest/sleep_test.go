package coretest_test

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// A listener survives a sleep: a conversation's link that expired while
// nothing used it (a laptop asleep) is renewed within a day, once, so the
// listener carries on; a hooks-connected conversation whose listener is
// gone gets a beacon again; a Monitor survives both a sleep and its link's
// last day.

// linkOf is the workspace and stored link ID a stream link (or a URL
// carrying one) names.
func linkOf(t *testing.T, link string) (tenant, id string) {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	tok := u.Query().Get("link")
	b, _ := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[0])
	var c struct{ T, L string }
	if err := json.Unmarshal(b, &c); err != nil || c.T == "" || c.L == "" {
		t.Fatalf("not a stream link: %v", err)
	}
	return c.T, c.L
}

// inLinkTenant runs sql in the link's workspace with the link's ID as $1.
func inLinkTenant(t *testing.T, c *coretest.Core, link, sql string, args ...any) {
	t.Helper()
	tenant, id := linkOf(t, link)
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, append([]any{id}, args...)...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// sleepPast makes a link look unused for ago: its expiry that far back.
func sleepPast(t *testing.T, c *coretest.Core, link string, ago time.Duration) {
	t.Helper()
	inLinkTenant(t, c, link, `UPDATE stream_links SET expires_at = now() - $2::interval WHERE id = $1`, ago.String())
}

// hooksConnect subscribes a Claude Code conversation (s1) whose
// Wakeshift hooks redeem the result's beacon, and polls once as the hooks
// do: the session's handle and its link.
func hooksConnect(t *testing.T, c *coretest.Core, app *wakeshift.Client, call caller, conversation, topic string) (caller, string) {
	t.Helper()
	sub, err := subscribe(app, call, topic, subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	link := redeemFrom(t, c, sub.Addition, conversation, call.Meta["claudecode/toolUseId"].(string))
	if _, err := (&connector.HTTPPoller{Issuers: []string{c.URL}}).Poll(ctx, link, 0, false); err != nil {
		t.Fatal(err)
	}
	return call, link
}

// redeemFrom redeems the beacon in a result's _meta, bound to the
// conversation and call, as the hooks do: the link.
func redeemFrom(t *testing.T, c *coretest.Core, a addition, conversation, call string) string {
	t.Helper()
	tok, _ := a.Meta[beacon.MetaKey].(string)
	claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: conversation, Call: call}, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatalf("no beacon (%v): %+v", err, a)
	}
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, conversation)
	link, _ := out["url"].(string)
	if code != 200 || link == "" {
		t.Fatalf("redeem: %d %v", code, out)
	}
	return link
}

// The plugin's waiter after a sleep longer than its link's life: its next
// poll gets no deliveries and the link's successor (which a connector
// already saves), the successor gets what arrived meanwhile, and the old
// link is used up.
func TestHooksListenerSurvivesASleep(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	_, link := hooksConnect(t, c, app, caller{User: "bo", Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_1"}}, "s1", "ci/acme")
	sleepPast(t, c, link, 3*time.Hour)
	if _, err := emitNote(app, []string{"ci/acme"}, note{Text: "while you slept"}); err != nil {
		t.Fatal(err)
	}
	p := &connector.HTTPPoller{Issuers: []string{c.URL}}
	b, err := p.Poll(ctx, link, 0, true)
	if err != nil || b.Link == "" || b.Link == link || len(b.Deliveries) != 0 {
		t.Fatalf("the first poll after the sleep: %+v %v", b, err)
	}
	got, err := p.Poll(ctx, b.Link, 10*time.Second, true)
	if err != nil || len(got.Deliveries) != 1 || got.Deliveries[0].Event["subject"] != "while you slept" {
		t.Fatalf("the renewed link: %+v %v", got, err)
	}
	if _, err := p.Poll(ctx, link, 0, false); err != connector.ErrLinkGone {
		t.Fatalf("the old link again: %v", err)
	}
}

// A hooks-connected conversation whose listener is gone (its link expired
// past renewing, or a connector from before renewals dropped it) while a
// message waits: the app's next result carries a beacon again (one per
// tool every few minutes), whose redeem reconnects it and gets the
// message. While the listener is fine, results carry none, as before.
func TestLostListenerGetsABeacon(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call, link := hooksConnect(t, c, app, caller{User: "lo", Client: "claude-code", Meta: map[string]any{"claudecode/toolUseId": "toolu_1"}}, "s1", "ci/acme")
	if _, err := emitNote(app, []string{"ci/acme"}, note{Text: "waiting"}); err != nil {
		t.Fatal(err)
	}
	tenant, _ := linkOf(t, link)
	waited := func() { // the message has waited a while
		t.Helper()
		waitFor(t, "the message queued", func() bool {
			var n int
			c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE state = 'pending'`).Scan(&n)
			})
			return n > 0
		})
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE deliveries SET available_at = now() - interval '5 minutes' WHERE state = 'pending'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	waited()
	call.Meta = map[string]any{"claudecode/toolUseId": "toolu_2"}
	call.Tool = "post"
	if r, err := annotate(app, call); err != nil || r.Meta[beacon.MetaKey] != nil {
		t.Fatalf("a healthy listener's result: %+v %v", r, err)
	}

	// Its link expired over a day ago: gone for good.
	sleepPast(t, c, link, 25*time.Hour)
	r, err := annotate(app, call)
	if err != nil || r.Meta[beacon.MetaKey] == nil || strings.Contains(r.Text, "call") {
		t.Fatalf("a lost listener's result: %+v %v", r, err)
	}
	call2 := call
	call2.Meta = map[string]any{"claudecode/toolUseId": "toolu_3"}
	if again, _ := annotate(app, call2); again.Meta[beacon.MetaKey] != nil {
		t.Fatalf("a beacon on every result: %+v", again)
	}
	fresh := redeemFrom(t, c, r, "s1", "toolu_2")
	got, err := (&connector.HTTPPoller{Issuers: []string{c.URL}}).Poll(ctx, fresh, 5*time.Second, true)
	if err != nil || len(got.Deliveries) != 1 || got.Deliveries[0].Event["subject"] != "waiting" {
		t.Fatalf("reconnected: %+v %v", got, err)
	}
	// Listening again: no beacon (even with a message waiting).
	if _, err := emitNote(app, []string{"ci/acme"}, note{Text: "next"}); err != nil {
		t.Fatal(err)
	}
	waited()
	call.Tool = "join"
	if r, _ := annotate(app, call); r.Meta[beacon.MetaKey] != nil {
		t.Fatalf("after reconnecting: %+v", r)
	}
}

// loop is a Monitor command running in sh, with what it prints.
type loop struct {
	t     *testing.T
	lines chan string
	kill  func()
}

func runLoop(t *testing.T, cmd string) *loop {
	t.Helper()
	sh := exec.Command("sh", "-c", cmd)
	sh.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := sh.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sh.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	l := &loop{t: t, lines: make(chan string, 100), kill: func() {
		once.Do(func() { syscall.Kill(-sh.Process.Pid, syscall.SIGKILL); sh.Wait() })
	}}
	t.Cleanup(l.kill)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if sc.Text() != "" {
				l.lines <- sc.Text()
			}
		}
		close(l.lines)
	}()
	return l
}

// next is the next line the command prints of type typ (others skipped).
func (l *loop) next(typ string, within time.Duration) (map[string]any, error) {
	l.t.Helper()
	deadline := time.After(within)
	for {
		select {
		case s, ok := <-l.lines:
			if !ok {
				return nil, errors.New("the command ended")
			}
			var m map[string]any
			json.Unmarshal([]byte(s), &m)
			if m["type"] == typ {
				return m, nil
			}
			if m["type"] == "hub.closed" {
				return m, errors.New("closed: " + s)
			}
		case <-deadline:
			return nil, errors.New("no " + typ + " line")
		}
	}
}

// A Monitor survives a sleep longer than its link's life and its link's
// last day: the command's first round after the sleep renews the link and
// moves to its successor, so what arrived meanwhile comes; it saves the
// new link to its link file, so the same command started again keeps
// working, while the old link says it was renewed. On the link's last day
// a round moves to the next link, and saves that too.
func TestMonitorSurvivesASleepAndItsLastDay(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	sub, err := subscribe(app, caller{User: "mo", Client: "claude-code"}, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	m := monitorRE.FindStringSubmatch(sub.Addition.Text)
	if m == nil {
		t.Fatalf("no Monitor command: %s", sub.Addition.Text)
	}
	// Restarts after secs seconds, not 25 minutes, with rounds of 2s, not
	// 50s (the server keeps a round's timeout in the urls it hands out).
	short := func(cmd string, secs string) string {
		if !strings.Contains(cmd, "-ge 1500 ]") {
			t.Fatalf("no restart in %s", cmd)
		}
		return strings.ReplaceAll(strings.Replace(cmd, "-ge 1500 ]", "-ge "+secs+" ]", 1), "timeout=50s", "timeout=2s")
	}
	original := short(m[1], "3")
	link := m[2]

	// Asleep for 40 minutes before the command runs (or while it ran).
	sleepPast(t, c, link, 40*time.Minute)
	emitNote(app, []string{"ci/acme"}, note{Text: "after the sleep"})
	run := runLoop(t, original)
	if e, err := run.next("event", 15*time.Second); err != nil || e["subject"] != "after the sleep" {
		t.Fatalf("after the sleep: %v %v", e, err)
	}
	restart, err := run.next("hub.restart", 20*time.Second)
	renewed := readLinkFile(t, link)
	if err != nil || restart["url"] != nil {
		t.Fatalf("the restart line: %v %v", restart, err)
	}
	if _, a := linkOf(t, link); func() bool { _, b := linkOf(t, renewed); return a == b }() {
		t.Fatalf("the link file still holds the old link: %s", renewed)
	}
	run.kill()

	// Started again on the old link: it was renewed, and says so.
	setLink(t, link, link)
	old := runLoop(t, original)
	if closed, err := old.next("hub.closed", 15*time.Second); err != nil || closed["reason"] != "renewed" {
		t.Fatalf("the old url: %v %v", closed, err)
	}
	old.kill()

	// Started again as it was, on the link it saved.
	setLink(t, link, renewed)
	again := short(m[1], "8")
	run = runLoop(t, again)
	time.Sleep(500 * time.Millisecond)
	emitNote(app, []string{"ci/acme"}, note{Text: "after the restart"})
	if e, err := run.next("event", 15*time.Second); err != nil || e["subject"] != "after the restart" {
		t.Fatalf("after the restart: %v %v", e, err)
	}

	// Its link's last day: the next round's link is a new one.
	inLinkTenant(t, c, renewed, `UPDATE stream_links SET slide_until = now() + interval '10 minutes', expires_at = now() + interval '5 minutes' WHERE id = $1`)
	emitNote(app, []string{"ci/acme"}, note{Text: "on the last day"})
	if e, err := run.next("event", 15*time.Second); err != nil || e["subject"] != "on the last day" {
		t.Fatalf("on the last day: %v %v", e, err)
	}
	restart, err = run.next("hub.restart", 20*time.Second)
	nextDay := readLinkFile(t, link)
	if err != nil || restart["url"] != nil || func() bool { _, a := linkOf(t, nextDay); _, b := linkOf(t, renewed); return a == b }() {
		t.Fatalf("the restart line after the last day: %v %v (the file: %s)", restart, err, nextDay)
	}
	run.kill()
	// A day on: the renewed link has ended for good, and the new one works.
	inLinkTenant(t, c, renewed, `UPDATE stream_links SET slide_until = now() - interval '1 minute', expires_at = now() - interval '1 minute' WHERE id = $1`)
	run = runLoop(t, short(m[1], "8"))
	time.Sleep(500 * time.Millisecond)
	emitNote(app, []string{"ci/acme"}, note{Text: "the next day"})
	if e, err := run.next("event", 15*time.Second); err != nil || e["subject"] != "the next day" {
		t.Fatalf("the next day: %v %v", e, err)
	}
}
