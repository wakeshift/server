package coretest_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// topicSubRow is the workspace and subscription an event subscription's
// ID names (as only the core knows).
func topicSubRow(t *testing.T, c *coretest.Core, id string) (tenant, sub string) {
	t.Helper()
	tenant, sub, _ = noteRow(t, c, id)
	return tenant, sub
}

// count counts rows inside a workspace.
func count(t *testing.T, c *coretest.Core, tenant, query string, args ...any) int {
	t.Helper()
	var n int
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error { return tx.QueryRow(ctx, query, args...).Scan(&n) }); err != nil {
		t.Fatal(err)
	}
	return n
}

// handleRE is a session handle in a user's workspace (or one a harness
// names by conversation): random, never a number.
var handleRE = regexp.MustCompile(`^wsc_[a-z2-7]{26}$`)

// The status calls say who is subscribed and connected, and how, without
// delivering, minting or ending anything.
func TestEventStatusSaysWhoIsConnected(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	if st, err := noteStatus(app, "ci/acme"); err != nil || st.Subscriptions != 0 || st.Connected != 0 || st.Via["stream"] != 0 {
		t.Fatalf("nobody: %+v %v", st, err)
	}
	if _, err := noteStatus(app, "Not/A/Topic"); !wakeshift.IsCode(err, "bad_arguments") {
		t.Fatalf("bad arguments: %v", err)
	}

	// Claude Code, its Monitor not started; an inbox conversation with a
	// message waiting.
	claudeCall := caller{User: "bob", Client: "claude-code"}
	claude, _ := subscribe(app, claudeCall, "ci/acme", subOpts{})
	inbox, _ := subscribe(app, caller{User: "cy", Client: "some-client"}, "ci/acme", subOpts{})
	emitNote(app, []string{inbox.Topic}, note{Text: "waiting"})
	claudeTenant, _ := topicSubRow(t, c, claude.ID)
	links := count(t, c, claudeTenant, `SELECT count(*) FROM stream_links`)
	st, err := noteStatus(app, "ci/acme")
	if err != nil || st.Subscriptions != 2 || st.Connected != 0 {
		t.Fatalf("not connected: %+v %v", st, err)
	}
	if s, err := subStatus(app, claude, "bob"); err != nil || !s.Live || s.Connected || s.Via != "" {
		t.Fatalf("claude: %+v %v", s, err)
	}
	if n := count(t, c, claudeTenant, `SELECT count(*) FROM stream_links`); n != links {
		t.Fatalf("status minted links: %d, then %d", links, n)
	}

	// Its Monitor starts: connected by stream.
	mon := monitorRE.FindStringSubmatch(claude.Addition.Text)
	readLines(t, mon[1])
	waitFor(t, "connected", func() bool {
		s, _ := subStatus(app, claude, "bob")
		return s.Connected && s.Via == "poll"
	})
	if st, _ := noteStatus(app, "ci/acme"); st.Subscriptions != 2 || st.Connected != 1 || st.Via["poll"] != 1 || st.Via["hooks"] != 0 {
		t.Fatalf("one connected: %+v", st)
	}

	// A Codex thread whose connector redeemed its beacon: by hooks.
	const thread = "019a14bd-bbbc-7b70-a723-0abb9f38118e"
	codexSub, _ := subscribe(app, caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}},
		"ci/acme", subOpts{})
	claims, err := beacon.Verify(codexSub.Addition.Meta[beacon.MetaKey].(string), thread, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, redeemed := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
	dialStream(t, redeemed["url"].(string), true)
	waitFor(t, "codex connected", func() bool {
		s, _ := subStatus(app, codexSub, "ada")
		return s.Via == "hooks"
	})
	if st, _ := noteStatus(app, "ci/acme"); st.Subscriptions != 3 || st.Connected != 2 || st.Via["hooks"] != 1 {
		t.Fatalf("two connected: %+v", st)
	}

	// Nothing was drained: the conversation's message still waits for it.
	if tenant, sub := topicSubRow(t, c, inbox.ID); count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE subscription_id = $1 AND state = 'pending'`, sub) != 1 {
		t.Fatal("status drained the message")
	}
	// Unsubscribed, it's not live and not counted; another topic is its own.
	unsubscribe(app, caller{User: "cy", Client: "some-client", Session: inbox.Session}, "ci/acme")
	if s, _ := subStatus(app, inbox, "cy"); s.Live || s.Connected {
		t.Fatalf("after unsubscribe: %+v", s)
	}
	if st, _ := noteStatus(app, "ci/acme"); st.Subscriptions != 2 {
		t.Fatalf("after unsubscribe: %+v", st)
	}
	if st, _ := noteStatus(app, "ci/other"); st.Subscriptions != 0 {
		t.Fatalf("another topic: %+v", st)
	}
	// Another app sees nothing of these.
	if st, _ := noteStatus(noteApp(t, c, "other"), "ci/acme"); st.Subscriptions != 0 {
		t.Fatalf("another app: %+v", st)
	}
}

// age moves a session's newest stream links and tickets into the past.
func age(t *testing.T, c *coretest.Core, tenant string, by time.Duration) {
	t.Helper()
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		secs := by.Seconds()
		if _, err := tx.Exec(ctx, `UPDATE stream_links SET created_at = created_at - make_interval(secs => $1), handed_at = handed_at - make_interval(secs => $1)`, secs); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE connector_tickets SET created_at = created_at - make_interval(secs => $1), expires_at = expires_at - make_interval(secs => $1)`, secs)
		return err
	})
}

// Annotating a result says nothing, hands nothing over and mints no link:
// Claude Code without a Monitor isn't given its command again. It carries
// a beacon for hooks the user may have installed (Claude Code bound to its
// call, Codex without its connector, Cursor without its hooks), but not
// while the last one is a moment old and unused; once that's a few minutes
// old, a fresh one comes.
func TestQuietAnnotate(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")

	claudeCall := caller{User: "bob", Client: "claude-code"}
	claude, _ := subscribe(app, claudeCall, "ci/acme", subOpts{})
	claudeCall.Session = claude.Session
	tenant, _ := topicSubRow(t, c, claude.ID)
	links := count(t, c, tenant, `SELECT count(*) FROM stream_links`)
	for range 3 {
		if r, err := annotate(app, claudeCall); err != nil || r.Text != "" || len(r.Meta) != 0 {
			t.Fatalf("right after subscribing: %+v %v", r, err)
		}
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM stream_links`); n != links {
		t.Fatalf("links: %d, then %d", links, n)
	}
	// What arrives waits for a listener: no result hands it over, or gives
	// the Monitor's command again (only subscribing sets a listener up).
	emitNote(app, []string{claude.Topic}, note{Text: "while you were away"})
	heldNotShown(t, c, app, claudeCall, claude.ID, "while you were away")
	age(t, c, tenant, 4*time.Minute)
	for range 2 {
		if r, _ := annotate(app, claudeCall); r.Text != "" {
			t.Fatalf("after a few minutes: %q", r.Text)
		}
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM stream_links`); n != links {
		t.Fatalf("annotating minted links: %d, then %d", links, n)
	}
	// Bound to its tool call, a later result carries a beacon (for hooks
	// installed since), and still says nothing.
	withCall := claudeCall
	withCall.Meta = map[string]any{"claudecode/toolUseId": "toolu_q"}
	if r, _ := annotate(app, withCall); r.Text != "" || r.Meta[beacon.MetaKey] == nil {
		t.Fatalf("with a call: %+v", r)
	}

	// Codex without its connector: no new ticket for each result.
	const thread = "019a14bd-bbbc-7b70-a723-0abb9f38118f"
	codexCall := caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
	codexSub, _ := subscribe(app, codexCall, "ci/acme", subOpts{})
	codexTenant, _ := topicSubRow(t, c, codexSub.ID)
	if r, err := annotate(app, codexCall); err != nil || len(r.Meta) != 0 {
		t.Fatalf("codex right after subscribing: %+v %v", r, err)
	}
	if n := count(t, c, codexTenant, `SELECT count(*) FROM connector_tickets`); n != 1 {
		t.Fatalf("codex tickets: %d", n)
	}
	age(t, c, codexTenant, 4*time.Minute)
	if r, _ := annotate(app, codexCall); r.Meta[beacon.MetaKey] == nil {
		t.Fatalf("codex after a few minutes: %+v", r)
	}

	// Cursor without its hooks: the same, with a text beacon.
	cursorCall := caller{User: "cy", Client: "Cursor", Conversation: "4e1d9b0a-1111-4222-8333-444455556666"}
	cursorSub, _ := subscribe(app, cursorCall, "ci/acme", subOpts{})
	cursorCall.Session = cursorSub.Session
	if r, _ := annotate(app, cursorCall); beacon.InText(r.Text) != "" {
		t.Fatalf("cursor right after subscribing: %q", r.Text)
	}
	cursorTenant, _ := topicSubRow(t, c, cursorSub.ID)
	age(t, c, cursorTenant, 4*time.Minute)
	if r, _ := annotate(app, cursorCall); beacon.InText(r.Text) == "" {
		t.Fatalf("cursor after a few minutes: %q", r.Text)
	}

	// Long-expired tickets and links are cleaned up; recent ones stay.
	age(t, c, codexTenant, 2*24*time.Hour)
	annotate(app, codexCall) // a new ticket
	var tickets, cleaned int64
	c.DB.InTenant(ctx, codexTenant, func(tx pgx.Tx) (err error) {
		tickets, _, err = appapi.CleanUp(ctx, tx, 100)
		return err
	})
	if tickets != 2 || count(t, c, codexTenant, `SELECT count(*) FROM connector_tickets`) != 1 {
		t.Fatalf("cleaned %d tickets", tickets)
	}
	all := count(t, c, tenant, `SELECT count(*) FROM stream_links`)
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_links SET expires_at = now() - interval '2 days' WHERE id = (SELECT id FROM stream_links ORDER BY created_at LIMIT 1)`)
		return err
	})
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		_, cleaned, err = appapi.CleanUp(ctx, tx, 100)
		return err
	})
	if cleaned != 1 || count(t, c, tenant, `SELECT count(*) FROM stream_links`) != all-1 {
		t.Fatalf("cleaned %d links of %d", cleaned, all)
	}
}

// Agents are named after the harness, not the client's raw name, in topics
// as in sign-in.
func TestTopicAgentsFollowTheHarness(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	for client, want := range map[string]string{"copilot-cli": "copilot", "antigravity-client": "antigravity", "gemini-cli-mcp-client": "gemini", "Claude Code": "claude-code"} {
		sub, err := subscribe(app, caller{User: "u-" + want[:3], Client: client}, "ci/acme", subOpts{})
		if err != nil || !handleRE.MatchString(sub.Session) || sub.Addition.Agent.Harness != want {
			t.Errorf("%s: %q %q %v", client, sub.Session, sub.Addition.Text, err)
		}
	}
	if g := agentkit.HarnessAgent("copilot-cli"); g != "copilot" {
		t.Errorf("agentkit.HarnessAgent: %q", g)
	}
}

// Gemini CLI is retired: subscribing tells its agent plainly that it can't
// be woken and that Antigravity CLI (agy) replaced it, with no install
// note and no beacon; what's sent waits for the session, and no result
// hands it over.
func TestGeminiCLIIsRetired(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "ada", Client: "gemini-cli-mcp-client"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil || !handleRE.MatchString(sub.Session) {
		t.Fatalf("%q %v", sub.Session, err)
	}
	text := sub.Addition.Text
	if !strings.Contains(text, "can't wake Gemini CLI conversations") || !strings.Contains(text, "Antigravity CLI (agy)") ||
		strings.Contains(text, "curl") || strings.Contains(text, "install") || beacon.InText(text) != "" || len(sub.Addition.Meta) != 0 {
		t.Fatalf("subscribe: %+v", sub.Addition)
	}
	tenant, _ := topicSubRow(t, c, sub.ID)
	if n := count(t, c, tenant, `SELECT count(*) FROM connector_tickets`); n != 0 {
		t.Fatalf("tickets: %d", n)
	}
	call.Session = sub.Session
	emitNote(app, []string{sub.Topic}, note{Text: "build done"})
	heldNotShown(t, c, app, call, sub.ID, "build done")
}

// A Copilot conversation that subscribed before Wakeshift's hooks were
// installed has a session that doesn't know its conversation. After the
// install (and copilot --continue), its calls name the conversation in
// _meta: passing its handle back adopts the session for the conversation,
// rather than starting a second one that publishes also match.
func TestCopilotKeepsItsSessionOnceHooksNameIt(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	before := caller{User: "ada", Client: "copilot-cli"}
	sub, err := subscribe(app, before, "ci/acme", subOpts{})
	h1 := sub.Session
	if err != nil || !handleRE.MatchString(h1) {
		t.Fatalf("%q %v", sub.Session, err)
	}
	named := func(conversation, session string) caller {
		return caller{User: "ada", Client: "copilot-cli", Session: session,
			Meta: map[string]any{"wakeshift/conversation": conversation, "progressToken": 1}}
	}
	again, err := subscribe(app, named("1b47c0de", h1), "ci/acme", subOpts{})
	if err != nil || again.Session != h1 {
		t.Fatalf("after the hooks: %q %v", again.Session, err)
	}
	// From now on the conversation alone names it, handle or not.
	if s, err := subscribe(app, named("1b47c0de", ""), "ci/acme", subOpts{}); err != nil || s.Session != h1 {
		t.Fatalf("by conversation: %q %v", s.Session, err)
	}
	if p, err := emitNote(app, []string{"ci/acme"}, note{Text: "x"}); err != nil || p.Matched != 1 {
		t.Fatalf("publish matched %d %v", p.Matched, err)
	}
	// A session that already has its conversation isn't taken by another
	// conversation passing its handle: that one gets its own.
	other, err := subscribe(app, named("9f00aa11", h1), "ci/acme", subOpts{})
	if err != nil || !handleRE.MatchString(other.Session) || other.Session == h1 {
		t.Fatalf("another conversation: %q %v", other.Session, err)
	}
	seen := map[string]bool{h1: true, other.Session: true}
	// Nor is another agent's session, or another user's.
	goose, _ := subscribe(app, caller{User: "ada", Client: "goose-cli"}, "ci/acme", subOpts{})
	seen[goose.Session] = true
	if s, err := subscribe(app, named("77aa", goose.Session), "ci/acme", subOpts{}); err != nil || seen[s.Session] {
		t.Fatalf("another agent's: %q %v", s.Session, err)
	} else {
		seen[s.Session] = true
	}
	bob, _ := subscribe(app, caller{User: "bob", Client: "copilot-cli"}, "ci/acme", subOpts{})
	seen[bob.Session] = true
	for _, h := range []string{bob.Session, "copilot#1", "wsc_" + strings.Repeat("a", 26)} {
		if s, err := subscribe(app, caller{User: "ada", Client: "copilot-cli", Session: h,
			Meta: map[string]any{"wakeshift/conversation": "88bb-" + h}}, "ci/acme", subOpts{}); err != nil || seen[s.Session] {
			t.Fatalf("an unknown handle %q: %q %v", h, s.Session, err)
		} else {
			seen[s.Session] = true
		}
	}
}
