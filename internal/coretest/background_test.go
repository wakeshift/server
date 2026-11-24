package coretest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// shellRE is the Shell call a Cursor result gives, and the command's url.
var shellRE = cmdRE{re: regexp.MustCompile(`(\{"block_until_ms":0,"command":"f=\$HOME/\.wakeshift/links/.*?","notify_on_output":\{.*?\}\}) \.`)}

// loopRE is the command an Antigravity result gives for run_command (the
// session's wait in a loop that ends when messages arrive), and its url.
var loopRE = cmdRE{re: regexp.MustCompile(loopCmd(`c=; p=\$PPID; `) + ` \.`)}

// waitCmdRE is the one-shot wait a Grok Bot result gives: its url.
var waitCmdRE = cmdRE{re: regexp.MustCompile(`run this command now and let it run: ` + loopCmd(`c=; p=; s=\$\(date \+%s\); `) + ` \.`), urlOnly: true}

// runCommand runs a command in sh until it ends, and returns its lines.
func runCommand(t *testing.T, cmd string) []string {
	t.Helper()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "sh", "-c", cmd).Output()
	if err != nil {
		t.Errorf("sh: %v %s", err, out)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// sessionConversation is a session's conversation, as the core keeps it.
func sessionConversation(t *testing.T, c *coretest.Core, tenant, handle string) string {
	t.Helper()
	var conv string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(conversation, '') FROM sessions WHERE label = $1`, strings.TrimPrefix(handle, "wsc_")).Scan(&conv)
	}); err != nil {
		t.Fatal(err)
	}
	return conv
}

// Cursor, with nothing installed: the result gives the Shell call (the
// session's wait in a loop, notify_on_output on event lines) and asks for
// CURSOR_CONVERSATION_ID; while the command runs, a publish arrives on it
// and results say it's running, with the same command; the conversation's
// ID, passed back, is learned (once); a cloud agent's (bc-…) is told it
// can't be woken, and gets its messages in its next results.
func TestCursorBackground(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "cy", Client: "Cursor"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	m := shellRE.FindStringSubmatch(sub.Addition.Text)
	// It didn't say which program it is (Cursor's calls are also Grok
	// Bot's): asked to, with Cursor's instructions for a tool that can't.
	if m == nil || !strings.HasPrefix(sub.Addition.Text, "If you're Grok Bot or a Cursor cloud agent, pass harness") ||
		!strings.Contains(m[2], "/v1/stream/wait?") || strings.Contains(m[1], "format=lines") {
		t.Fatalf("result: %s", sub.Addition.Text)
	}
	var shell struct {
		Command        string
		NotifyOnOutput struct{ Pattern string } `json:"notify_on_output"`
	}
	if err := json.Unmarshal([]byte(m[1]), &shell); err != nil {
		t.Fatalf("shell call %s: %v", m[1], err)
	}
	pattern := regexp.MustCompile(shell.NotifyOnOutput.Pattern)

	lines, _ := runMonitor(t, shell.Command)
	waitFor(t, "the command connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	emitNote(app, []string{"ci/acme"}, note{Text: "deploy finished"})
	f := lines.expect("deploy finished")
	if !pattern.MatchString(`{"type":"event","id":"` + f.Event.ID + `"`) {
		t.Fatalf("the pattern %s", pattern)
	}
	tenant, _ := topicSubRow(t, c, sub.ID)
	waitFor(t, "confirmed by the command's next round", func() bool {
		return count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE event_id = $1 AND state = 'acked'`, f.Event.ID) == 1
	})
	if r, _ := annotate(app, call); r.Text != "" {
		t.Fatalf("while it runs: %s", r.Text)
	}
	// Its conversation's ID, passed back: the session learns it, and stops
	// asking. The command it's told to keep running is the same.
	call.Conversation = "4f0c9a52-6f0e-4bde-8a43-1d0a7e39d2a1"
	again, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil || !strings.Contains(again.Addition.Text, "already running") || again.Session != sub.Session {
		t.Fatalf("again: %+v %v", again, err)
	}
	// The same command, but for its one-time link claim (#1152).
	sameCmd := func(a, b string) bool {
		code := regexp.MustCompile(`wso_[A-Za-z0-9_-]+`)
		return code.ReplaceAllString(a, "CODE") == code.ReplaceAllString(b, "CODE")
	}
	if a := shellRE.FindStringSubmatch(again.Addition.Text); a == nil || !sameCmd(a[1], m[1]) {
		t.Fatalf("not the same command: %s", again.Addition.Text)
	}
	if got := sessionConversation(t, c, tenant, sub.Session); got != call.Conversation {
		t.Fatalf("learned %q", got)
	}
	// It learns it once: another ID later doesn't move it.
	other := call
	other.Conversation = "9b1e8f0e-0000-4000-8000-000000000000"
	subscribe(app, other, "ci/acme", subOpts{})
	if got := sessionConversation(t, c, tenant, sub.Session); got != call.Conversation {
		t.Fatalf("moved to %q", got)
	}

	// Something that isn't an ID (the variable's name) is ignored.
	odd, _ := subscribe(app, caller{User: "cx", Client: "Cursor", Conversation: "$CURSOR_CONVERSATION_ID", Harness: "cursor"}, "ci/acme", subOpts{})
	if !strings.Contains(odd.Addition.Text, "printenv CURSOR_CONVERSATION_ID") || strings.Contains(odd.Addition.Text, "To give you the right instructions") {
		t.Fatalf("odd: %s", odd.Addition.Text)
	}

	// A cloud agent: no command, one neutral line, and its messages wait
	// for a listener (none comes): no result hands them over.
	cloud := caller{User: "cz", Client: "Cursor", Conversation: "bc-6f1d2c3b-4a5e-4f60-9b7a-8c9d0e1f2a3b"}
	cs, err := subscribe(app, cloud, "ci/cloud", subOpts{})
	if err != nil || !strings.Contains(cs.Addition.Text, "Cursor cloud agent") || strings.Contains(cs.Addition.Text, "curl") || strings.Contains(cs.Addition.Text, "wakeshift-beacon") {
		t.Fatalf("cloud: %+v %v", cs.Addition, err)
	}
	cloud.Session = cs.Session
	emitNote(app, []string{"ci/cloud"}, note{Text: "for the cloud agent"})
	heldNotShown(t, c, app, cloud, cs.ID, "for the cloud agent")
	// Without a user, the conversation's own workspace keeps its session
	// when it passes its ID after subscribing.
	anon := caller{Client: "Cursor"}
	as, _ := subscribe(app, anon, "ci/acme", subOpts{})
	anon.Session, anon.Conversation = as.Session, "0d7f1c2e-1111-4222-8333-444455556666"
	if again, _ := subscribe(app, anon, "ci/acme", subOpts{}); again.Session != as.Session || strings.Contains(again.Addition.Text, "printenv") {
		t.Fatalf("anonymous: %s then %+v", as.Session, again)
	}
}

// Antigravity, with nothing installed: the result gives the command for
// run_command (the session's wait in a loop that ends when messages
// arrive); it ends with a publish, confirming it; watching again while it
// runs gives the same command; a link that expired says to call the tool
// again, and the result after that gives a fresh command.
func TestAntigravityBackground(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const conv = "2b9d08bb-7acc-4459-9956-69064f8d5869"
	call := caller{Client: "antigravity-client", Meta: map[string]any{"antigravity.google/conversation_id": conv}}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	m := loopRE.FindStringSubmatch(sub.Addition.Text)
	// Without a user, the session key is what finds the conversation again
	// (its ID only names it), so it's given even though agy names the
	// conversation.
	if m == nil || !strings.Contains(sub.Addition.Text, "run_command") || !strings.Contains(sub.Addition.Text, `session_key="`+sub.Session+`"`) || strings.Contains(sub.Addition.Text, "curl -sN") {
		t.Fatalf("result: %s", sub.Addition.Text)
	}
	done := make(chan []string)
	go func() { done <- runCommand(t, m[1]) }()
	waitFor(t, "waiting", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	// The agent calls the tool again with its session key, as the result
	// says.
	call.Session = sub.Session
	if again, _ := subscribe(app, call, "ci/acme", subOpts{}); !strings.Contains(again.Addition.Text, "already running") ||
		loopRE.FindStringSubmatch(again.Addition.Text) == nil || loopRE.FindStringSubmatch(again.Addition.Text)[1] != m[1] {
		t.Fatalf("watching again while it runs: %s", again.Addition.Text)
	}
	emitNote(app, []string{"ci/acme"}, note{Text: "build failed"})
	var got []string
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the command didn't end")
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], `{"type":"event"`) || !strings.Contains(got[0], "build failed") {
		t.Fatalf("printed: %q", got)
	}
	tenant, _ := topicSubRow(t, c, sub.ID)
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE state = 'acked'`); n != 1 {
		t.Fatalf("confirmed before it ended: %d", n)
	}
	// Its link expires (long after the last wait: the computer slept): the
	// next run renews it, once, and gets what arrived; the same command
	// again says its link was renewed, and the next result has the new one.
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = now() - interval '2 minutes'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_links SET expires_at = now() - interval '1 minute'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	go func() { done <- runCommand(t, m[1]) }()
	time.Sleep(500 * time.Millisecond)
	emitNote(app, []string{"ci/acme"}, note{Text: "after the sleep"})
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the command didn't end after the sleep")
	}
	if len(got) != 1 || !strings.Contains(got[0], "after the sleep") {
		t.Fatalf("after the sleep: %q", got)
	}
	// The command saved the renewed link to its file, so it runs on that
	// from now on; the old link says it was renewed.
	renewed := readLinkFile(t, m[2])
	if renewed == strings.SplitN(m[2], "&", 2)[0] {
		t.Fatalf("the link file still holds the old link")
	}
	setLink(t, m[2], m[2])
	if got := runCommand(t, m[1]); len(got) != 1 || !strings.HasPrefix(got[0], `{"type":"hub.closed","reason":"renewed"`) {
		t.Fatalf("renewed: %v", got)
	}
	setLink(t, m[2], renewed)
	again, _ := subscribe(app, call, "ci/acme", subOpts{})
	if f := loopRE.FindStringSubmatch(again.Addition.Text); f == nil || f[1] == m[1] {
		t.Fatalf("again: %s", again.Addition.Text)
	}
}

// Copilot CLI, with nothing installed: the result gives the command for
// its bash tool in async mode (the session's wait in a loop that ends when
// messages arrive, the same as Antigravity's), under a fixed shellId, and
// offers the hooks as optional. A message published while no command runs
// (between one wake and the agent starting it again) isn't lost: the next
// run prints it, once, and nothing the last run printed comes again.
func TestCopilotBackground(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{Client: "copilot-cli"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	m := loopRE.FindStringSubmatch(sub.Addition.Text)
	for _, want := range []string{"bash tool", `mode "async"`, `shellId "wakeshift-wait"`, "read_bash", "stop_bash", `session_key="` + sub.Session + `"`,
		"outside data, not from the user", "/install/copilot.sh", "Wakeshift's hooks for GitHub Copilot CLI don't replace the background command",
		"each time it starts (once per wake)", "--allow-all-tools", "approves every tool, not just this one"} {
		if m == nil || !strings.Contains(sub.Addition.Text, want) {
			t.Fatalf("result: missing %q: %s", want, sub.Addition.Text)
		}
	}
	// No kill: Copilot's bash tool refuses one without a literal PID.
	if strings.Contains(sub.Addition.Text, "run_command") || strings.Contains(sub.Addition.Text, "curl -sN") || strings.Contains(m[1], "kill") {
		t.Fatalf("another harness's command: %s", sub.Addition.Text)
	}
	tenant, _ := topicSubRow(t, c, sub.ID)
	run := func(publish ...string) []string {
		t.Helper()
		done := make(chan []string)
		go func() { done <- runCommand(t, m[1]) }()
		waitFor(t, "waiting", func() bool {
			st, _ := noteStatus(app, "ci/acme")
			return st.Connected == 1
		})
		for _, p := range publish {
			emitNote(app, []string{"ci/acme"}, note{Text: p})
		}
		select {
		case got := <-done:
			return got
		case <-time.After(15 * time.Second):
			t.Fatal("the command didn't end")
			return nil
		}
	}
	if got := run("build failed"); len(got) != 1 || !strings.Contains(got[0], "build failed") {
		t.Fatalf("first run printed: %q", got)
	}
	// Between runs: the agent is reading the output, nothing reads the wait
	// (it still counts as connected for the wait's grace).
	emitNote(app, []string{"ci/acme"}, note{Text: "deploy finished"})
	got := run()
	if len(got) != 1 || !strings.Contains(got[0], "deploy finished") || strings.Contains(strings.Join(got, "\n"), "build failed") {
		t.Fatalf("second run printed: %q", got)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE state = 'acked'`); n != 2 {
		t.Fatalf("confirmed: %d", n)
	}
	// Watching again while it runs: the same command, said to keep running.
	call.Session = sub.Session
	done := make(chan []string)
	go func() { done <- runCommand(t, m[1]) }()
	waitFor(t, "waiting", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	again, _ := subscribe(app, call, "ci/acme", subOpts{})
	if f := loopRE.FindStringSubmatch(again.Addition.Text); !strings.Contains(again.Addition.Text, "already running") || f == nil || f[1] != m[1] || !strings.Contains(again.Addition.Text, `mode "async"`) {
		t.Fatalf("watching again while it runs: %s", again.Addition.Text)
	}
	emitNote(app, []string{"ci/acme"}, note{Text: "done"})
	<-done
}

// sessionHarness is what a session recorded its agent said it runs in.
func sessionHarness(t *testing.T, c *coretest.Core, tenant, handle string) string {
	t.Helper()
	var h string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(harness, '') FROM sessions WHERE label = $1`, strings.TrimPrefix(handle, "wsc_")).Scan(&h)
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

// Agents say which program they run in (harness) and their conversation's
// ID (conversation_id). The claim only refines what the call was
// recognized as within its family (Cursor's calls are also Grok Bot's and
// its cloud agents'), or names the harness of a call nothing recognized;
// one that contradicts the client name is ignored. Neither grants
// anything: a claimed harness or a borrowed conversation ID reaches no
// other conversation's messages.
func TestAgentsSayWhoTheyAre(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const grokConv = "bc-0f3e2d1c-5b6a-4789-8abc-def012345678"

	// Grok Bot: a Cursor client that says so waits in the foreground on the
	// one-shot wait, and the session keeps both (a later call that doesn't
	// say gets the same instructions).
	grok := caller{User: "gina", Client: "Cursor", UserAgent: "Cursor/1.0.0", Harness: "Grok Bot", Conversation: grokConv}
	sub, err := subscribe(app, grok, "ci/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if sub.Addition.Agent != (wakeshift.Recognized{Harness: "grok-bot", Via: "client", Claimed: "grok-bot"}) {
		t.Fatalf("agent: %+v", sub.Addition.Agent)
	}
	m := waitCmdRE.FindStringSubmatch(sub.Addition.Text)
	if m == nil || !strings.Contains(sub.Addition.Text, "Grok Bot can't be woken by a command in the background") || !strings.Contains(sub.Addition.Text, "you're blocked on that command") ||
		strings.Contains(sub.Addition.Text, "Shell tool") || strings.Contains(sub.Addition.Text, "To give you the right instructions") ||
		strings.Contains(sub.Addition.Text, "printenv") || strings.Contains(sub.Addition.Text, "wakeshift-beacon") {
		t.Fatalf("grok: %s", sub.Addition.Text)
	}
	tenant, _ := topicSubRow(t, c, sub.ID)
	if h, conv := sessionHarness(t, c, tenant, sub.Session), sessionConversation(t, c, tenant, sub.Session); h != "grok-bot" || conv != grokConv {
		t.Fatalf("recorded %q, %q", h, conv)
	}
	done := make(chan []map[string]any)
	go func() { done <- waitLines(t, m[1]) }()
	waitFor(t, "waiting", func() bool {
		st, _ := noteStatus(app, "ci/grok")
		return st.Connected == 1
	})
	emitNote(app, []string{"ci/grok"}, note{Text: "for grok"})
	var next string
	select {
	case got := <-done:
		if len(got) != 3 || got[0]["subject"] != "for grok" || got[1]["type"] != "cursor" || got[2]["type"] != "hub.next" {
			t.Fatalf("wait: %v", got)
		}
		next = got[2]["url"].(string) + "&after=" + got[1]["after"].(string)
	case <-time.After(5 * time.Second):
		t.Fatal("the wait didn't answer")
	}
	later, _ := subscribe(app, caller{User: "gina", Client: "Cursor", Session: sub.Session}, "ci/grok", subOpts{})
	if later.Addition.Agent.Harness != "grok-bot" || later.Addition.Agent.Claimed != "" || !strings.Contains(later.Addition.Text, "in the foreground") {
		t.Fatalf("later: %+v", later)
	}
	// A later claim that contradicts recognition doesn't touch the record.
	subscribe(app, caller{User: "gina", Client: "Cursor", Session: sub.Session, Harness: "claude-code"}, "ci/grok", subOpts{})
	if h := sessionHarness(t, c, tenant, sub.Session); h != "grok-bot" {
		t.Fatalf("an ignored claim moved the record to %q", h)
	}

	// A claim that contradicts the client name is ignored: a Cursor client
	// claiming Claude Code gets Cursor's command (and is asked), not a
	// Monitor; a Claude Code client claiming Grok Bot gets its Monitor.
	notClaude, _ := subscribe(app, caller{User: "nc", Client: "Cursor", Harness: "claude-code"}, "ci/x", subOpts{})
	if notClaude.Addition.Agent != (wakeshift.Recognized{Harness: "cursor", Via: "client", Claimed: "claude-code"}) || shellRE.FindStringSubmatch(notClaude.Addition.Text) == nil ||
		strings.Contains(notClaude.Addition.Text, "Monitor tool") || !strings.Contains(notClaude.Addition.Text, "If you're Grok Bot or a Cursor cloud agent") {
		t.Fatalf("Cursor claiming Claude Code: %+v", notClaude)
	}
	tn, _ := topicSubRow(t, c, notClaude.ID)
	if h := sessionHarness(t, c, tn, notClaude.Session); h != "" {
		t.Fatalf("an ignored claim was recorded: %q", h)
	}
	notGrok, _ := subscribe(app, caller{User: "ng", Client: "claude-code", Harness: "grok-bot"}, "ci/x", subOpts{})
	if notGrok.Addition.Agent.Harness != "claude-code" || !strings.Contains(notGrok.Addition.Text, "Monitor tool") || strings.Contains(notGrok.Addition.Text, "foreground") {
		t.Fatalf("Claude Code claiming Grok Bot: %+v", notGrok)
	}

	// A Cursor cloud agent, claimed: no command, its messages are held.
	cloud, _ := subscribe(app, caller{User: "cc", Client: "Cursor", Harness: "cursor-cloud"}, "ci/x", subOpts{})
	if cloud.Addition.Agent.Harness != "cursor-cloud" || !strings.Contains(cloud.Addition.Text, "Cursor cloud agent") || strings.Contains(cloud.Addition.Text, "curl") {
		t.Fatalf("cloud: %+v", cloud)
	}
	// A bc- ID with "cursor" claimed is a cloud agent too.
	cloud2, _ := subscribe(app, caller{User: "cc2", Client: "Cursor", Harness: "cursor", Conversation: "bc-11111111-2222-4333-8444-555555555555"}, "ci/x", subOpts{})
	if cloud2.Addition.Agent.Harness != "cursor-cloud" || strings.Contains(cloud2.Addition.Text, "curl") {
		t.Fatalf("bc-, cursor: %+v", cloud2)
	}

	// Nothing recognized: the claim names the harness (as stating that
	// client name would), and the conversation is named as before, so its
	// calls with and without the claim are one conversation.
	anon, _ := subscribe(app, caller{UserAgent: "node"}, "ci/anon", subOpts{})
	if !strings.HasPrefix(anon.Addition.Text, "Wakeshift can't wake this agent, so nothing will notify you here") {
		t.Fatalf("anon: %s", anon.Addition.Text)
	}
	decided, err := subscribe(app, caller{UserAgent: "node", Session: anon.Session, Harness: "claude-code"}, "ci/anon", subOpts{})
	if err != nil || decided.Session != anon.Session || decided.Addition.Agent != (wakeshift.Recognized{Harness: "claude-code", Via: "claim", Claimed: "claude-code"}) ||
		!strings.Contains(decided.Addition.Text, "Monitor tool") {
		t.Fatalf("decided: %+v %v", decided, err)
	}

	// Spoofing gains nothing. Another user's conversation claiming Grok Bot
	// with gina's conversation ID gets a session of its own: its wait sees
	// nothing of gina's, and gina's session is unchanged.
	spoof, err := subscribe(app, caller{User: "mallory", Client: "Cursor", Harness: "grok-bot", Conversation: grokConv}, "ci/mallory", subOpts{})
	if err != nil || spoof.Session == sub.Session {
		t.Fatalf("spoof: %+v %v", spoof, err)
	}
	sm := waitCmdRE.FindStringSubmatch(spoof.Addition.Text)
	if sm == nil || sm[1] == m[1] {
		t.Fatalf("spoof's command: %s", spoof.Addition.Text)
	}
	emitNote(app, []string{"ci/grok"}, note{Text: "only for gina"})
	if got := waitLines(t, sm[1]+"&timeout=300ms"); len(got) != 1 || got[0]["type"] != "hub.next" {
		t.Fatalf("the spoof's wait got: %v", got)
	}
	// Passing gina's session key from another workspace names nothing
	// there: a new conversation, not gina's.
	stolen, _ := subscribe(app, caller{User: "mallory", Client: "Cursor", Harness: "grok-bot", Session: sub.Session}, "ci/mallory", subOpts{})
	if stolen.Session == sub.Session || !strings.Contains(stolen.Addition.Text, "isn't known") {
		t.Fatalf("stolen key: %+v", stolen)
	}
	if h, conv := sessionHarness(t, c, tenant, sub.Session), sessionConversation(t, c, tenant, sub.Session); h != "grok-bot" || conv != grokConv {
		t.Fatalf("gina's session changed: %q, %q", h, conv)
	}
	// Gina's message waits for her own next wait.
	if got := waitLines(t, next); len(got) != 3 || got[0]["subject"] != "only for gina" {
		t.Fatalf("gina's: %v", got)
	}
}

// waitLines runs a wait and returns its JSON lines.
func waitLines(t *testing.T, url string) []map[string]any {
	resp, err := http.Get(url)
	if err != nil {
		t.Error(err)
		return nil
	}
	defer resp.Body.Close()
	var out []map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}
