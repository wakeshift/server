package coretest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

// A publish while a subscription with replay is being made reaches it
// once: either live (the publish waits for the subscription) or as the
// replay, never neither. The subscribe is held mid-way by a lock on its
// session, and the publish runs meanwhile.
func TestTopicsPublishDuringSubscribe(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "race")
	first, err := subscribe(app, caller{Client: "cursor-vscode"}, "warmup", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call := caller{Client: "cursor-vscode", Session: first.Session}
	var tenant string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE app_id = 'race' AND external_id = $1`,
		"~conversation:"+strings.TrimPrefix(first.Session, "wsc_")).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	hold, err := c.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	hold.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant)
	if _, err := hold.Exec(ctx, `SELECT 1 FROM sessions FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	subscribed := make(chan error, 1)
	go func() {
		_, err := subscribe(app, call, "race/topic", subOpts{Replay: 5 * time.Minute})
		subscribed <- err
	}()
	time.Sleep(300 * time.Millisecond) // the subscribe has begun, and waits for the session
	published := make(chan error, 1)
	go func() {
		_, err := emitNote(app, []string{"race/topic"}, note{Text: "published meanwhile"})
		published <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-subscribed; err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := <-published; err != nil {
		t.Fatalf("publish: %v", err)
	}
	meanwhile := `SELECT count(*) FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id WHERE e.subject = 'published meanwhile'`
	waitFor(t, "the event published during the subscribe", func() bool { return count(t, c, tenant, meanwhile) > 0 })
	time.Sleep(300 * time.Millisecond)
	if n := count(t, c, tenant, meanwhile); n != 1 {
		t.Fatalf("deliveries of the event published during the subscribe: %d", n)
	}
	time.Sleep(300 * time.Millisecond)
	if note, _ := annotate(app, call); strings.Contains(note.Text, "published meanwhile") {
		t.Fatalf("delivered twice: %q", note.Text)
	}
}

// stream is a WebSocket on a stream link, read as a harness's consumer does.
type stream struct {
	t      *testing.T
	ws     *websocket.Conn // dialStream's (nil for lines)
	frames chan frame
	err    chan error
	// closedNote: the note of the hub.closed line a lines stream ended with
	// (set before err gets the end).
	closedNote string
}

// dialStream reads in the background (a read that times out would close
// the connection), acknowledging each event on an ack link.
func dialStream(t *testing.T, url string, ack bool) *stream {
	t.Helper()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	s := &stream{t: t, ws: ws, frames: make(chan frame, 100), err: make(chan error, 1)}
	go func() {
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				s.err <- err
				return
			}
			var f frame
			json.Unmarshal(b, &f)
			if f.Type != "event" {
				continue
			}
			if ack {
				a, _ := json.Marshal(map[string]string{"type": "ack", "delivery_id": f.DeliveryID})
				ws.Write(ctx, websocket.MessageText, a)
			}
			s.frames <- f
		}
	}()
	return s
}

type frame struct {
	Type       string `json:"type"`
	DeliveryID string `json:"delivery_id"`
	Event      struct {
		ID      string            `json:"id"`
		Subject string            `json:"subject"`
		Data    json.RawMessage   `json:"data"`
		Attrs   map[string]string `json:"attrs"`
		Trust   string            `json:"trust"`
		From    struct{ Kind, ID string }
	} `json:"event"`
}

// next is the next event on the stream (acknowledging it on an ack link),
// or an error (a close, or nothing within wait).
func (s *stream) next(wait time.Duration) (frame, error) {
	s.t.Helper()
	select {
	case f := <-s.frames:
		return f, nil
	case err := <-s.err:
		return frame{}, err
	case <-time.After(wait):
		return frame{}, errors.New("nothing arrived")
	}
}

func (s *stream) expect(subject string) frame {
	s.t.Helper()
	f, err := s.next(5 * time.Second)
	if err != nil || f.Event.Subject != subject {
		s.t.Fatalf("expected %q, got %q (%v)", subject, f.Event.Subject, err)
	}
	return f
}

func (s *stream) expectNothing() {
	s.t.Helper()
	if f, err := s.next(700 * time.Millisecond); err == nil {
		s.t.Fatalf("expected nothing, got %q", f.Event.Subject)
	}
}

// monitorRE is the command a Claude Code result tells Monitor to run (the
// session's wait in a loop: harnesses.MonitorLoop), and its first url.
var monitorRE = cmdRE{re: regexp.MustCompile(`with this command: ` + loopCmd(`c=; p=\$PPID; `) + ` \.`)}

// readLines reads what a Monitor shows: a stream link's lines over plain
// HTTP, as curl -sN prints them, or (for a Monitor command) what the
// command prints, run in sh as Monitor runs it, until the test ends.
func readLines(t *testing.T, url string) *stream {
	t.Helper()
	if strings.HasPrefix(url, harnesses.LinkStart) {
		s, _ := runMonitor(t, url)
		return s
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("lines %s: %v %v", url, resp, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return readBody(t, resp)
}

// runMonitor runs a Monitor command, and returns what it prints and a
// func that kills it (its curl too).
func runMonitor(t *testing.T, cmd string) (*stream, func()) {
	t.Helper()
	c := exec.Command("sh", "-c", cmd)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	kill := func() {
		once.Do(func() {
			syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			c.Wait()
		})
	}
	t.Cleanup(kill)
	return readFrom(t, out), kill
}

// readBody reads an open lines response.
func readBody(t *testing.T, resp *http.Response) *stream { return readFrom(t, resp.Body) }

// readFrom reads lines as a Monitor shows them.
func readFrom(t *testing.T, r io.Reader) *stream {
	s := &stream{t: t, frames: make(chan frame, 100), err: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			// Lines mode is a compact line for Monitor: the event's fields
			// at the top level.
			if len(sc.Bytes()) == 0 {
				continue // a keepalive (Monitor skips empty lines)
			}
			var l struct {
				Type, ID, Topic, From, Trust, Subject string
				Data                                  json.RawMessage
				DeliveryID                            string `json:"delivery_id"`
			}
			json.Unmarshal(sc.Bytes(), &l)
			if l.Type == "event" {
				var f frame
				f.Type, f.DeliveryID, f.Event.ID, f.Event.Subject, f.Event.Data, f.Event.Trust = l.Type, l.DeliveryID, l.ID, l.Subject, l.Data, l.Trust
				f.Event.Attrs = map[string]string{"topic": l.Topic}
				f.Event.From.Kind, f.Event.From.ID, _ = strings.Cut(l.From, "/")
				s.frames <- f
			}
			var end struct{ Type, Reason, Note string }
			if json.Unmarshal(sc.Bytes(), &end); end.Type == "hub.closed" {
				s.closedNote = end.Note
				s.err <- fmt.Errorf("closed: %s", end.Reason)
				return
			}
		}
		s.err <- sc.Err()
	}()
	return s
}

func post(t *testing.T, url, body string, header map[string]string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

// The whole flow: an app subscribes a Codex thread and a Claude Code
// conversation to an event from inside its tools, emits, and both are
// woken through their harness's path (the Codex one through its connector
// session); filters, once and replay behave; unsubscribing and ending a
// subscription stop its events.
func TestEventsEndToEnd(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const thread = "019a14bd-bbbc-7b70-a723-0abb9f38118e"
	codexCall := caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
	claudeCall := caller{User: "bob", Client: "claude-code"}

	// Codex: the result carries a beacon for the connector, which redeems
	// it for the thread's own stream.
	codexSub, err := subscribe(app, codexCall, "ci/acme/api", subOpts{Filter: map[string]any{"branch": "main"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(codexSub.Addition.Text, `"Wakeshift: connected"`) || !strings.Contains(codexSub.Addition.Text, "/install/codex.sh | sh") ||
		!strings.Contains(codexSub.Addition.Text, "~/.codex/hooks.json") || strings.Contains(codexSub.Addition.Text, "session_key") || !handleRE.MatchString(codexSub.Session) {
		t.Fatalf("codex result: %+v", codexSub)
	}
	tok, _ := codexSub.Addition.Meta[beacon.MetaKey].(string)
	claims, err := beacon.Verify(tok, thread, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatalf("beacon: %v", err)
	}
	code, redeemed := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
	if code != 200 {
		t.Fatalf("redeem: %d %v", code, redeemed)
	}
	codex := dialStream(t, redeemed["url"].(string), true)

	// Claude Code: the result carries a stream link and Monitor steps.
	claudeSub, err := subscribe(app, claudeCall, "ci/acme/api", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	mon := monitorRE.FindStringSubmatch(claudeSub.Addition.Text)
	if mon == nil || !strings.Contains(claudeSub.Addition.Text, "Monitor") || !handleRE.MatchString(claudeSub.Session) ||
		!strings.Contains(claudeSub.Addition.Text, `session_key="`+claudeSub.Session+`"`) || len(claudeSub.Addition.Meta) != 0 {
		t.Fatalf("claude code result: %+v", claudeSub)
	}
	claude := readLines(t, mon[1])
	waitFor(t, "connected", func() bool {
		s, _ := subscribe(app, caller{User: "bob", Client: "claude-code", Session: claudeSub.Session}, "ci/acme/api", subOpts{})
		return strings.Contains(s.Addition.Text, "already running")
	})
	// Subscribing again with the handle is the same subscription.
	again, _ := subscribe(app, caller{User: "bob", Client: "claude-code", Session: claudeSub.Session}, "ci/acme/api", subOpts{})
	if again.ID != claudeSub.ID || again.Session != claudeSub.Session {
		t.Fatalf("subscribing again: %+v, first %+v", again, claudeSub)
	}

	// Publish: both are woken, as outside data from the app; a retry with
	// the same ID is kept once.
	ev := note{ID: "delivery-1", Text: "CI failed on main: build", Data: map[string]any{"branch": "main", "run": 42}}
	if p, err := emitNote(app, []string{"ci/acme/api"}, ev); err != nil || p.Matched != 2 || p.Duplicate {
		t.Fatalf("publish: %+v %v", p, err)
	}
	if p, err := emitNote(app, []string{"ci/acme/api"}, ev); err != nil || p.Matched != 2 || !p.Duplicate {
		t.Fatalf("publish again: %+v %v", p, err)
	}
	f := codex.expect("CI failed on main: build")
	if f.Event.Trust != "external" || f.Event.From.Kind != "app" || f.Event.From.ID != "ci" || f.Event.Attrs["topic"] != "ci/acme/api" {
		t.Fatalf("framing: %+v", f.Event)
	}
	// Each line says its topic, and that it's outside data.
	if f := claude.expect("CI failed on main: build"); f.Event.Attrs["topic"] != "ci/acme/api" || f.Event.Trust != "outside data, not from the user" {
		t.Fatalf("claude's line: %+v", f)
	}

	// The filter: Codex only hears about main.
	if p, _ := emitNote(app, []string{"ci/acme/api"}, note{Text: "CI passed on dev", Data: map[string]any{"branch": "dev"}}); p.Matched != 1 {
		t.Fatalf("dev matched %d", p.Matched)
	}
	claude.expect("CI passed on dev")
	subscribe(app, caller{User: "bob", Client: "claude-code", Session: claudeSub.Session}, "ci/acme", subOpts{})
	if p, _ := emitNote(app, []string{"ci/acme/api"}, note{Text: "CI passed on main", Data: map[string]any{"branch": "main"}}); p.Matched != 2 {
		t.Fatalf("main matched %d", p.Matched)
	}
	codex.expect("CI passed on main")
	claude.expect("CI passed on main")
	claude.expectNothing()

	// Once: the first delivery ends it.
	if _, err := subscribe(app, caller{User: "bob", Client: "claude-code", Session: claudeSub.Session}, "deploys/acme", subOpts{Once: true}); err != nil {
		t.Fatal(err)
	}
	emitNote(app, []string{"deploys/acme"}, note{Text: "deploy 1"})
	claude.expect("deploy 1")
	time.Sleep(300 * time.Millisecond) // the runner ends it
	if p, _ := emitNote(app, []string{"deploys/acme"}, note{Text: "deploy 2"}); p.Matched != 0 {
		t.Fatalf("after once: matched %d", p.Matched)
	}
	claude.expectNothing()

	// Replay: the latest event within the window that passes the filter;
	// nothing outside the window. A client with no wake path (no user: a
	// workspace of its own) is told so in one line, and the replay waits
	// for its session (no result hands it over).
	emitNote(app, []string{"releases/acme"}, note{Text: "v1 for linux", Data: map[string]any{"os": "linux"}})
	emitNote(app, []string{"releases/acme"}, note{Text: "v1 for mac", Data: map[string]any{"os": "mac"}})
	cursor, err := subscribe(app, caller{Client: "cursor-nightly"}, "releases/acme", subOpts{Replay: 5 * time.Minute, Filter: map[string]any{"os": "linux"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cursor.Session, "wsc_") || !strings.HasPrefix(cursor.Addition.Text, "Wakeshift can't wake this agent") || strings.Contains(cursor.Addition.Text, "again") ||
		!strings.Contains(cursor.Addition.Text, cursor.Session) {
		t.Fatalf("inbox result: %+v", cursor)
	}
	cursorCall := caller{Client: "cursor-nightly", Session: cursor.Session}
	replayed := func(sub noteSub) []string {
		tenant, id := topicSubRow(t, c, sub.ID)
		var got []string
		c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT e.subject FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id WHERE d.subscription_id = $1`, id)
			if err != nil {
				return err
			}
			got, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		})
		return got
	}
	waitFor(t, "the replay", func() bool { return len(replayed(cursor)) > 0 })
	time.Sleep(300 * time.Millisecond)
	if got := replayed(cursor); len(got) != 1 || got[0] != "v1 for linux" {
		t.Fatalf("replayed: %q", got)
	}
	heldNotShown(t, c, app, cursorCall, cursor.ID, "v1 for linux")
	c.DB.Pool.Exec(ctx, `UPDATE topic_events SET published_at = published_at - interval '1 hour' WHERE app_id = 'ci'`)
	late, _ := subscribe(app, caller{Client: "cursor-nightly"}, "releases/acme", subOpts{Replay: 5 * time.Minute})
	time.Sleep(300 * time.Millisecond)
	if got := replayed(late); len(got) != 0 {
		t.Fatalf("replayed from outside the window: %q", got)
	}
	// A made-up session handle isn't a conversation: subscribing with it
	// starts a new one (never one the made-up handle names), and says so.
	made, err := subscribe(app, caller{Client: "cursor-nightly", Session: "wsc_AAAAAAAAAAAAAAAAAAAAAA"}, "releases/acme", subOpts{})
	if err != nil || made.Session == "wsc_AAAAAAAAAAAAAAAAAAAAAA" || !strings.HasPrefix(made.Session, "wsc_") ||
		!strings.Contains(made.Addition.Text, "session_key you passed isn't known") || !strings.Contains(made.Addition.Text, made.Session) {
		t.Fatalf("a made-up session: %+v %v", made, err)
	}

	// Something waits for the inbox conversation.
	if p, err := emitNote(app, []string{"releases/acme"}, note{Text: "for the inbox", Data: map[string]any{"os": "linux"}}); err != nil || p.Matched != 3 {
		t.Fatalf("emit to the inbox: %+v %v", p, err)
	}
	// While its connector runs, other results need nothing for Codex.
	if r, err := annotate(app, codexCall); err != nil || r.Text != "" || len(r.Meta) != 0 {
		t.Fatalf("annotate codex with its connector running: %+v %v", r, err)
	}
	if n, err := unsubscribe(app, caller{User: "bob", Client: "claude-code", Session: claudeSub.Session}, "ci/acme/api"); err != nil || n != 1 {
		t.Fatalf("unsubscribe: %d %v", n, err)
	}
	if p, _ := emitNote(app, []string{"ci/acme/api"}, note{Text: "too late for claude", Data: map[string]any{"branch": "main"}}); p.Matched != 1 {
		t.Fatalf("after unsubscribe: matched %d", p.Matched)
	}
	codex.expect("too late for claude")
	if n, _ := unsubscribe(app, caller{User: "bob", Client: "claude-code", Session: claudeSub.Session}, "ci/acme"); n != 1 {
		t.Fatalf("unsubscribe from the rest: %d", n)
	}
	// Its last one: the Monitor's stream ends at once, saying there's
	// nothing more to do (and not to get a new link, which would undo the
	// unsubscribe).
	if _, err := claude.next(5 * time.Second); err == nil || err.Error() != "closed: unsubscribed" ||
		!strings.Contains(claude.closedNote, "You unsubscribed from everything you watched in Ci") || !strings.Contains(claude.closedNote, "Nothing more to do") {
		t.Fatalf("claude's stream after its last unsubscribe: %v (%s)", err, claude.closedNote)
	}

	// The app ends a subscription by its ID: the conversation is told,
	// and its stream stays, since it has another subscription.
	if _, err := subscribe(app, codexCall, "u/ada/inbox", subOpts{}); err != nil {
		t.Fatal(err)
	}
	if ended, err := endNote(app, "ada", codexSub.ID); err != nil || !ended {
		t.Fatalf("end: %v %v", ended, err)
	}
	if f := codex.expect("Your subscription to note.three {a:ci,b:acme,branch:main,c:api} in Ci ended (Ci ended it). Its events won't reach this conversation any more."); f.Event.Trust != "internal" {
		t.Fatalf("the notice: %+v", f.Event)
	}
	if p, _ := emitNote(app, []string{"ci/acme/api"}, note{Text: "x", Data: map[string]any{"branch": "main"}}); p.Matched != 0 {
		t.Fatalf("after ending: matched %d", p.Matched)
	}
	if p, _ := emitNote(app, []string{"u/ada/inbox"}, note{Text: "still listening"}); p.Matched != 1 {
		t.Fatalf("the other subscription: matched %d", p.Matched)
	}
	codex.expect("still listening")

	// A ttl runs out: the subscription ends and the conversation is told.
	short, _ := subscribe(app, cursorCall, "t/short", subOpts{TTL: time.Minute})
	shortTenant, shortSub := topicSubRow(t, c, short.ID)
	c.DB.InTenant(ctx, shortTenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET expires_at = now() - interval '1 second' WHERE subscription_id = $1`, shortSub)
		return err
	})
	var expired int
	c.DB.InTenant(ctx, shortTenant, func(tx pgx.Tx) (err error) {
		expired, err = appapi.ExpireTopics(ctx, tx)
		return err
	})
	if expired != 1 {
		t.Fatalf("expired %d", expired)
	}
	// What was waiting isn't handed over in a result, and the notice waits
	// for the session as any message does.
	if strings.Contains(short.Addition.Text, "for the inbox") {
		t.Fatalf("subscribing again: %q", short.Addition.Text)
	}
	notice := `SELECT count(*) FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id
		WHERE d.state = 'pending' AND e.subject LIKE '%nothing arrived on it for 1 minute%'`
	waitFor(t, "the ttl's notice", func() bool { return count(t, c, shortTenant, notice) == 1 })
	if note, _ := annotate(app, cursorCall); note.Text != "" {
		t.Fatalf("after the ttl: %q", note.Text)
	}

	// Forgetting a user ends their subscriptions and streams.
	if ok, err := app.ForgetUser(ctx, "ada"); err != nil || !ok {
		t.Fatalf("forget: %v %v", ok, err)
	}
	var closed websocket.CloseError
	if _, err := codex.next(5 * time.Second); !errors.As(err, &closed) || closed.Code != 4001 || closed.Reason != "forgotten" {
		t.Fatalf("ada's stream after forgetting her: %v", err)
	}
	if p, _ := emitNote(app, []string{"u/ada/inbox"}, note{Text: "x"}); p.Matched != 0 {
		t.Fatalf("after forgetting: matched %d", p.Matched)
	}
}

// An event id dedupes emits for a day, as the docs say: within it a retry
// is a duplicate and stores nothing; after it, the same id emits again,
// and matched counts what it really reached.
func TestEmitIDAfterADay(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "dedupe")
	sub, err := subscribe(app, caller{Client: "goose"}, "ci/x", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var tenant string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE app_id = 'dedupe' AND external_id = $1`,
		"~conversation:"+strings.TrimPrefix(sub.Session, "wsc_")).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	stored := func() (n int) {
		t.Helper()
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM events WHERE source = 'app:dedupe'`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ev := note{ID: "d-1", Text: "CI failed on main"}
	if p, err := emitNote(app, []string{"ci/x"}, ev); err != nil || p.Matched != 1 || p.Duplicate {
		t.Fatalf("publish: %+v %v", p, err)
	}
	if p, err := emitNote(app, []string{"ci/x"}, ev); err != nil || p.Matched != 1 || !p.Duplicate || stored() != 1 {
		t.Fatalf("retry within the day: %+v %v, %d stored", p, err, stored())
	}
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE topic_events SET published_at = now() - interval '25 hours' WHERE app_id = 'dedupe'`); err != nil {
		t.Fatal(err)
	}
	if p, err := emitNote(app, []string{"ci/x"}, ev); err != nil || p.Matched != 1 || p.Duplicate || stored() != 2 {
		t.Fatalf("after a day: %+v %v, %d stored", p, err, stored())
	}
}

// A publish deletes the app's publishes over a day old, skipping any
// another is deleting rather than waiting for them: two of an app's
// publishes at once each deleted the same old rows in their own order and
// deadlocked (40P01), failing one publish.
func TestPublishSkipsOldPublishesAnotherIsDeleting(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "pruning")
	for i := range 3 {
		if _, err := emitNote(app, []string{"ci/x"}, note{Text: fmt.Sprintf("build %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE topic_events SET published_at = now() - interval '25 hours' WHERE app_id = 'pruning'`); err != nil {
		t.Fatal(err)
	}
	old := func() (n int) {
		t.Helper()
		if err := c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_events WHERE app_id = 'pruning' AND published_at < now() - interval '1 day'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Another publish, mid-way through deleting two of them.
	other, err := c.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback(ctx)
	if _, err := other.Exec(ctx, `SELECT 1 FROM topic_events WHERE app_id = 'pruning' ORDER BY published_at LIMIT 2 FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := emitNoteCtx(short, app, []string{"ci/x"}, note{Text: "build 3"}); err != nil {
		t.Fatalf("publishing while another deletes: %v", err)
	}
	if n := old(); n != 2 {
		t.Fatalf("%d old publishes left; want the 2 the other holds", n)
	}
	other.Rollback(ctx)
	if _, err := emitNote(app, []string{"ci/x"}, note{Text: "build 4"}); err != nil {
		t.Fatal(err)
	}
	if n := old(); n != 0 {
		t.Fatalf("%d old publishes left", n)
	}
}
