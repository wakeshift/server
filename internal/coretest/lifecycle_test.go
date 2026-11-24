package coretest_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// workspaceOf is the workspace the app keeps for a user ("" if none).
func workspaceOf(t *testing.T, c *coretest.Core, app, user string) string {
	t.Helper()
	var id string
	err := c.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE app_id = $1 AND external_id = $2`, app, user).Scan(&id)
	if err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	return id
}

// Session handles can't be guessed: with a user they're random tokens, not
// <agent>#<n>, so another conversation of the same user can't take over,
// unsubscribe or read a conversation by naming it; and they never recur
// after the user is forgotten.
func TestSessionHandlesCantBeGuessed(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "ada", Client: "claude-code"}
	a, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil || !handleRE.MatchString(a.Session) {
		t.Fatalf("first conversation: %+v %v", a, err)
	}
	b, _ := subscribe(app, call, "ci/other", subOpts{})
	if !handleRE.MatchString(b.Session) || b.Session == a.Session {
		t.Fatalf("second conversation: %+v", b)
	}
	// Something waits for the first conversation (its Monitor isn't running).
	if p, _ := emitNote(app, []string{a.Topic}, note{Text: "secret for A"}); p.Matched != 1 {
		t.Fatalf("emit: %+v", p)
	}
	waitFor(t, "the delivery", func() bool {
		return count(t, c, workspaceOf(t, c, "ci", "ada"), `SELECT count(*) FROM deliveries WHERE state = 'pending'`) == 1
	})
	seen := map[string]bool{a.Session: true, b.Session: true}
	for _, guess := range []string{"claude-code#1", "claude-code", "claude-code@" + strings.TrimPrefix(a.Session, "wsc_"), "wsc_" + strings.Repeat("a", 26), "wsc_" + strings.Repeat("A", 22)} {
		g := caller{User: "ada", Client: "claude-code", Session: guess}
		// Ordinary results say nothing of A's.
		if note, err := annotate(app, g); err != nil || strings.Contains(note.Text, "secret for A") {
			t.Fatalf("annotate with %q: %+v %v", guess, note, err)
		}
		// Unsubscribing with it ends nothing.
		if n, err := unsubscribe(app, g, "ci/acme"); err != nil || n != 0 {
			t.Fatalf("unsubscribe with %q: %d %v", guess, n, err)
		}
		// Subscribing with it is a new conversation, told so.
		s, err := subscribe(app, g, "ci/acme", subOpts{})
		if err != nil || seen[s.Session] || !handleRE.MatchString(s.Session) || !strings.Contains(s.Addition.Text, "session_key you passed isn't known") {
			t.Fatalf("subscribe with %q: %+v %v", guess, s, err)
		}
		seen[s.Session] = true
	}
	if !noteLive(t, c, a.ID) {
		t.Fatal("A's subscription ended after the guesses")
	}
	// A's message still waits for A's own listener (no result hands it over).
	if tenant, sub := topicSubRow(t, c, a.ID); count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE subscription_id = $1 AND state = 'pending'`, sub) != 1 {
		t.Fatal("A's held message is gone")
	}

	// Forgotten, the user's next conversations get new handles; the old
	// ones name nothing.
	if ok, err := app.ForgetUser(ctx, "ada"); err != nil || !ok {
		t.Fatalf("forget: %v %v", ok, err)
	}
	for range 2 {
		s, err := subscribe(app, call, "ci/acme", subOpts{})
		if err != nil || seen[s.Session] {
			t.Fatalf("after forgetting: %+v %v", s, err)
		}
		seen[s.Session] = true
	}
	old, err := subscribe(app, caller{User: "ada", Client: "claude-code", Session: a.Session}, "ci/acme", subOpts{})
	if err != nil || seen[old.Session] || !strings.Contains(old.Addition.Text, "session_key you passed isn't known") {
		t.Fatalf("an old handle after forgetting: %+v %v", old, err)
	}
}

// A handle presented by another MCP client is refused, with or without a
// user, and never makes a second subscription; ordinary results ignore it.
func TestSessionHandlesBelongToTheirClient(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	for _, user := range []string{"ada", ""} {
		sub, err := subscribe(app, caller{User: user, Client: "claude-code"}, "ci/acme", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		tenant, _ := topicSubRow(t, c, sub.ID)
		other := caller{User: user, Client: "cursor-nightly", Session: sub.Session}
		if _, err := subscribe(app, other, "ci/acme", subOpts{}); !wakeshift.IsCode(err, "other_client") {
			t.Fatalf("user %q: another client subscribing with the handle: %v", user, err)
		}
		if _, err := unsubscribe(app, other, "ci/acme"); !wakeshift.IsCode(err, "other_client") {
			t.Fatalf("user %q: another client unsubscribing with the handle: %v", user, err)
		}
		if note, err := annotate(app, other); err != nil || note.Text != "" {
			t.Fatalf("user %q: another client's annotate: %+v %v", user, note, err)
		}
		if n := count(t, c, tenant, `SELECT count(*) FROM topic_subscriptions`); n != 1 {
			t.Fatalf("user %q: %d subscriptions", user, n)
		}
		if n := count(t, c, tenant, `SELECT count(*) FROM sessions`); n != 1 {
			t.Fatalf("user %q: %d sessions", user, n)
		}
		if !noteLive(t, c, sub.ID) {
			t.Fatalf("user %q: the subscription ended", user)
		}
	}
	// Unsubscribing with a handle Wakeshift doesn't know is a no-op.
	for _, call := range []caller{{Client: "claude-code", User: "ada", Session: "claude-code#1"}, {Client: "claude-code", Session: "wsc_nope"}} {
		if n, err := unsubscribe(app, call, "ci/acme"); err != nil || n != 0 {
			t.Fatalf("unsubscribe with an unknown handle %+v: %d %v", call, n, err)
		}
	}
}

// A handle from another user's workspace is unknown, not another client's:
// the call can't learn that it exists anywhere else, and subscribing starts
// a new session of the caller's own, leaving the owner's untouched.
func TestAnotherUsersHandleIsUnknown(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	ada, err := subscribe(app, caller{User: "ada", Client: "claude-code"}, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := subscribe(app, caller{Client: "claude-code"}, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	adaTenant, _ := topicSubRow(t, c, ada.ID)
	for _, client := range []string{"claude-code", "cursor-nightly"} {
		for _, handle := range []string{ada.Session, conv.Session} {
			bob := caller{User: "bob", Client: client, Session: handle}
			if n, err := unsubscribe(app, bob, "ci/acme"); err != nil || n != 0 {
				t.Fatalf("%s unsubscribing with %s: %d %v", client, handle, n, err)
			}
			s, err := subscribe(app, bob, "ci/acme", subOpts{})
			if err != nil || s.Session == handle || !handleRE.MatchString(s.Session) || !strings.Contains(s.Addition.Text, "session_key you passed isn't known") {
				t.Fatalf("%s subscribing with %s: %+v %v", client, handle, s, err)
			}
			if tenant, _ := topicSubRow(t, c, s.ID); tenant == adaTenant {
				t.Fatalf("%s's subscription landed in ada's workspace", client)
			}
		}
	}
	if n := count(t, c, adaTenant, `SELECT count(*) FROM sessions`); n != 1 {
		t.Fatalf("ada's workspace has %d sessions", n)
	}
	for _, id := range []string{ada.ID, conv.ID} {
		if !noteLive(t, c, id) {
			t.Fatalf("%s ended", id)
		}
	}
}

// forgetUser deletes the user's workspace and everything in it, closes
// their streams, drops their private topics' recent publishes and publish
// URLs, and logs a hash for them in the app's log; again, it's false.
// Other users and conversations are untouched.
func TestForgetUserDeletesEverything(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	ada := caller{User: "ada", Client: "claude-code"}
	sub, err := subscribe(app, ada, "u/ada/inbox", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	mon := monitorRE.FindStringSubmatch(sub.Addition.Text)
	lines := readLines(t, mon[1])
	subscribe(app, caller{User: "ada", Client: "cursor-nightly"}, "ci/acme", subOpts{})
	bob, _ := subscribe(app, caller{User: "bob", Client: "claude-code"}, "ci/acme", subOpts{})
	anon, _ := subscribe(app, caller{Client: "some-client"}, "ci/acme", subOpts{})
	emitNote(app, []string{"u/ada/inbox"}, note{Text: "bob mentioned you", Data: map[string]any{"by": "bob"}})
	lines.expect("bob mentioned you")
	emitNote(app, []string{"ci/acme"}, note{Text: "build failed"})
	// A personal event's occurrence for her (u/p<hash>/…).
	personal := apps.PersonalTopic("tables/t1/turns", "ada")
	emitNote(app, []string{personal}, note{Text: "your turn, ada"})
	pu, _ := notePublishURL(app, "u/ada/inbox")
	keep, _ := notePublishURL(app, "ci/acme")
	tenant := workspaceOf(t, c, "ci", "ada")
	waitFor(t, "deliveries", func() bool { return count(t, c, tenant, `SELECT count(*) FROM deliveries`) >= 2 })
	if count(t, c, tenant, `SELECT count(*) FROM audit_log`) == 0 {
		t.Fatal("no audit rows to erase")
	}
	// Her daily usage counts (the expiry pass's rollup), which go too.
	if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO usage_daily (day, tenant_id, app_id, delivered, dead, cancelled, age_ms)
		VALUES (current_date - 1, $1, 'ci', 3, 0, 0, '{}'), (current_date - 200, $1, 'ci', 1, 0, 0, '{}')`, tenant); err != nil {
		t.Fatal(err)
	}

	if ok, err := app.ForgetUser(ctx, "ada"); err != nil || !ok {
		t.Fatalf("forget: %v %v", ok, err)
	}
	if _, err := lines.next(5 * time.Second); err == nil || !strings.HasPrefix(err.Error(), "closed") {
		t.Fatalf("ada's stream: %v", err)
	}
	// No table keeps a row for the workspace, and the workspace is gone.
	rows, _ := c.DB.Pool.Query(ctx, `SELECT table_name FROM information_schema.columns WHERE table_schema = 'public' AND column_name = 'tenant_id'`)
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tables) < 15 {
		t.Fatalf("tables %v %v", tables, err)
	}
	for _, table := range tables {
		if n := count(t, c, tenant, `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tenant); n != 0 {
			t.Errorf("%s keeps %d rows", table, n)
		}
	}
	var n int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1 OR external_id = 'ada'`, tenant).Scan(&n)
	if n != 0 {
		t.Errorf("the workspace or the user's ID is kept")
	}
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_events WHERE app_id = 'ci' AND $1 = ANY(topics)`, apps.PersonalTopic("inbox", "ada")).Scan(&n)
	if n != 0 {
		t.Errorf("%d emits of ada's notes kept", n)
	}
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM topic_events WHERE app_id = 'ci' AND $1 = ANY(topics)`, personal).Scan(&n)
	if n != 0 {
		t.Errorf("%d publishes of ada's personal events kept", n)
	}
	if code, _ := post(t, pu.URL, `{}`, nil); code != 404 {
		t.Errorf("ada's publish URL: %d", code)
	}
	if code, _ := post(t, keep.URL, `{}`, nil); code != 202 {
		t.Errorf("another publish URL: %d", code)
	}
	// The app's log says so, without the user's ID.
	var detail []byte
	if err := c.DB.Pool.QueryRow(ctx, `SELECT detail FROM app_log WHERE app_id = 'ci' AND action = 'app.user_forgotten'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var d struct {
		Subscriptions int `json:"subscriptions_ended"`
		Conversations int `json:"conversations"`
	}
	json.Unmarshal(detail, &d)
	// Nor a hash of it, which a guessable ID could be matched to.
	h := sha256.Sum256([]byte("ci:ada"))
	if d.Subscriptions != 2 || d.Conversations != 2 || strings.Contains(string(detail), `"ada"`) ||
		strings.Contains(string(detail), "sha256") || strings.Contains(string(detail), hex.EncodeToString(h[:])[:12]) {
		t.Errorf("log entry: %s", detail)
	}
	// Its subscription IDs name nothing now.
	if st, err := subStatus(app, sub, "ada"); err != nil || st.Live {
		t.Errorf("status: %+v %v", st, err)
	}
	if ended, err := endNote(app, "ada", sub.ID); err != nil || ended {
		t.Errorf("end: %v %v", ended, err)
	}
	// Again: nothing to forget.
	if ok, err := app.ForgetUser(ctx, "ada"); err != nil || ok {
		t.Fatalf("forget again: %v %v", ok, err)
	}
	// Bob and the user-less conversation are as they were.
	for _, id := range []string{bob.ID, anon.ID} {
		if !noteLive(t, c, id) {
			t.Errorf("%s ended", id)
		}
	}
	if p, _ := emitNote(app, []string{"ci/acme"}, note{Text: "again"}); p.Matched != 2 {
		t.Errorf("matched %d", p.Matched)
	}
	// Ada can come back: a new, empty workspace.
	if _, err := subscribe(app, ada, "u/ada/inbox", subOpts{}); err != nil {
		t.Fatal(err)
	}
	if again := workspaceOf(t, c, "ci", "ada"); again == "" || again == tenant {
		t.Errorf("new workspace %q", again)
	}
}
