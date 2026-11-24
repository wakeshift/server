package coretest_test

import (
	"bufio"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/streams"
)

// Session-only setup, sent when it's picked (appapi/sessionsetup.go;
// docs/harnesses.md): Claude Code's offer asks the question and gives the
// plugin's path, and for "this session" only a one-time URL whose answer
// has the steps (the link file, the Monitor's one line in the wait's loop
// mode).

var setupURLRE = regexp.MustCompile("run `curl -sS '([^']+)'`")

// deferredCore is a core with the session-only steps deferred (the default).
func deferredCore(t *testing.T) *coretest.Core {
	t.Helper()
	return coretest.StartWith(t, func(o *core.Options) { o.InlineSessionSteps = false })
}

func fetchSetup(t *testing.T, u string) (int, string) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content type %q", ct)
	}
	return resp.StatusCode, string(b)
}

// The steps' link and its file, and the Monitor's command.
var (
	stepsLinkRE = regexp.MustCompile(`to \$HOME/\.wakeshift/links/([0-9a-f]{16}), with \$HOME replaced by your home directory.s full path [^\n]*?, its whole content this one line: (\S+) \.`)
	stepsCmdRE  = regexp.MustCompile(`with this command: (chmod 700 [^\n]+; sleep 5; done)\n`)
)

// runSteps follows the steps as the agent does: writes the link file (as a
// file tool writes it, 0644 in a 0755 folder) in a HOME of the command's
// own, and starts the Monitor's command in sh.
func runSteps(t *testing.T, steps string) (*stream, func(), <-chan struct{}) {
	t.Helper()
	l := stepsLinkRE.FindStringSubmatch(steps)
	m := stepsCmdRE.FindStringSubmatch(steps)
	if l == nil || m == nil {
		t.Fatalf("no link step or command in the steps:\n%s", steps)
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".wakeshift", "links")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, l[1]), []byte(l[2]+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sh", "-c", m[1])
	c.Env = append(os.Environ(), "HOME="+home)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	closed := make(chan string, 4)
	exited := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), `{"type":"hub.closed"`) {
				closed <- sc.Text()
			}
			pw.Write(append(sc.Bytes(), '\n'))
		}
		c.Wait()
		pw.Close()
		close(exited)
	}()
	kill := func() { syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	t.Cleanup(func() { kill(); <-exited })
	return readFrom(t, pr), kill, exited
}

// The whole path: the first result asks, with no link, file or command in
// it; the URL answers once with the steps; the steps' Monitor is woken by
// two builds in one conversation, each once; a later result while nothing
// the user picked runs gives a new URL; refusals are all the same.
func TestClaudeCodeSessionStepsSentWhenPicked(t *testing.T) {
	c := deferredCore(t)
	plugin, _ := withMarketplace(t, c)
	app := noteApp(t, c, "ci")
	call := caller{Client: "claude-code", ClientVersion: "2.1.323", Tool: "watch", Meta: map[string]any{"claudecode/toolUseId": "toolu_1"}}
	first, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	text := first.Addition.Text
	for _, want := range []string{"Ci sends its notifications through Wakeshift.", "AskUserQuestion", `"` + harnesses.AskSession + `"`, `"` + harnesses.AskEvery + `"`, `"` + harnesses.AskNone + `"`,
		"/reload-plugins", "run `" + plugin + "`.", "`! " + plugin + "`", `session_key="` + first.Session + `"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("the first result has no %q:\n%s", want, text)
		}
	}
	for _, not := range []string{"/v1/stream/wait", ".wakeshift/links", "with this command", "without asking", "don't look anything up", "this tool again"} {
		if strings.Contains(text, not) {
			t.Fatalf("the first result has %q:\n%s", not, text)
		}
	}
	if len(text) > 2200 || first.Addition.Meta[beacon.MetaKey] == nil {
		t.Fatalf("the first result: %d bytes, beacon %v", len(text), first.Addition.Meta[beacon.MetaKey])
	}
	setup := setupURLRE.FindStringSubmatch(text)
	if setup == nil || !strings.HasPrefix(setup[1], c.URL+appapi.SetupPath+"?token=wsu_") {
		t.Fatalf("no setup URL: %s", text)
	}

	// Asked again (the agent polls a status tool that subscribes) before
	// the user picked: pointed back, with a new URL.
	call.Session = first.Session
	tenant, _ := topicSubRow(t, c, first.ID)
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE connector_tickets SET created_at = created_at - interval '1 minute'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	call.Meta = map[string]any{"claudecode/toolUseId": "toolu_2"}
	again, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	newer := setupURLRE.FindStringSubmatch(again.Addition.Text)
	if !strings.HasPrefix(again.Addition.Text, "Subscribed. Ci's notifications reach this conversation the way the user picks") || newer == nil || newer[1] == setup[1] ||
		strings.Contains(again.Addition.Text, "AskUserQuestion") {
		t.Fatalf("asked again: %s", again.Addition.Text)
	}

	// A link preview's or scanner's HEAD doesn't use it up.
	if resp, err := http.Head(setup[1]); err != nil || resp.StatusCode != 200 {
		t.Fatalf("HEAD: %v %v", resp, err)
	}
	// The user picks "this session": the steps, once.
	code, steps := fetchSetup(t, setup[1])
	if code != 200 || !strings.HasPrefix(steps, `Steps for "`+harnesses.AskSession+`"`) || !strings.Contains(steps, `"trust":"outside data, not from the user"`) ||
		!strings.Contains(steps, `while :; do curl -sgN --fail-with-body -m 360 "`+c.URL+`/v1/stream/wait?loop=$i&link=${t#*link=}"`) || !strings.Contains(steps, "If you have no Monitor tool") {
		t.Fatalf("the steps: %d\n%s", code, steps)
	}
	code, refused := fetchSetup(t, setup[1])
	if code != http.StatusGone || refused != appapi.SetupRefused {
		t.Fatalf("used again: %d %q", code, refused)
	}
	// The exact command the result gives prints the refusal's text, so the
	// agent reads what to do (no -f, which would print only "error: 410").
	if _, err := exec.LookPath("curl"); err == nil {
		cmd := regexp.MustCompile("run `(curl -sS '[^']+')`").FindStringSubmatch(text)
		out, _ := exec.Command("sh", "-c", cmd[1]).Output()
		if string(out) != appapi.SetupRefused {
			t.Fatalf("the command, used again, printed %q", out)
		}
	}
	// The same refusal, byte for byte, for anything else.
	for name, u := range map[string]string{
		"malformed":         c.URL + appapi.SetupPath + "?token=nope",
		"none":              c.URL + appapi.SetupPath,
		"another workspace": c.URL + appapi.SetupPath + "?token=wsu_00000000-0000-4000-8000-000000000000_" + strings.Repeat("A", 43),
		"unknown":           strings.Replace(setup[1], "_", "_x", 1)[:len(setup[1])],
	} {
		if code, body := fetchSetup(t, u); code != http.StatusGone || body != appapi.SetupRefused {
			t.Fatalf("%s: %d %q", name, code, body)
		}
	}

	// The Monitor, as the steps give it: two builds, each printed once.
	s, kill, exited := runSteps(t, steps)
	waitFor(t, "the Monitor connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected >= 1
	})
	emitNote(app, []string{"ci/acme"}, note{Text: "build 1 finished"})
	s.expect("build 1 finished")
	s.expectNothing()
	emitNote(app, []string{"ci/acme"}, note{Text: "build 2 finished"})
	s.expect("build 2 finished")
	s.expectNothing()

	// Running: a later subscribe says so, and starts nothing.
	call.Meta = map[string]any{"claudecode/toolUseId": "toolu_3"}
	running, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil || running.Addition.Text != harnesses.MonitorRunningNote() {
		t.Fatalf("while the Monitor runs: %+v %v", running, err)
	}

	// Claude Code stops the Monitor (30 minutes): a message while it's
	// stopped reaches the next one, started the same way.
	kill()
	<-exited
	emitNote(app, []string{"ci/acme"}, note{Text: "while it was stopped"})
	s2, _, _ := runSteps(t, steps)
	s2.expect("while it was stopped")
	s2.expectNothing()
}

// The steps for a conversation the plugin connected meanwhile, or that has
// nothing left to hear about; and a URL past its 15 minutes.
func TestClaudeCodeSessionStepsAnswers(t *testing.T) {
	c := deferredCore(t)
	app := noteApp(t, c, "ci")
	sub := func(tool string) (noteSub, string) {
		t.Helper()
		s, err := subscribe(app, caller{Client: "claude-code", Tool: "watch", Meta: map[string]any{"claudecode/toolUseId": tool}}, "ci/acme", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		m := setupURLRE.FindStringSubmatch(s.Addition.Text)
		if m == nil {
			t.Fatalf("no setup URL: %s", s.Addition.Text)
		}
		return s, m[1]
	}

	// Expired.
	s, u := sub("toolu_1")
	tenant, _ := topicSubRow(t, c, s.ID)
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE session_setups SET expires_at = now() - interval '1 second'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, body := fetchSetup(t, u); code != http.StatusGone || body != appapi.SetupRefused {
		t.Fatalf("expired: %d %q", code, body)
	}

	// The plugin's hooks redeemed for the conversation since (#1272): with
	// no poll by their waiter (claude -p, a one-shot Agent SDK query: it
	// can't be woken), or a last poll 3 minutes ago, the Monitor's steps,
	// as without them; once their waiter polled, nothing to start.
	const latest = `id = (SELECT session_id FROM session_setups ORDER BY created_at DESC LIMIT 1)`
	for i, ago := range []string{"", "3 minutes", "0 seconds"} {
		s, u = sub("toolu_2" + string(rune('a'+i)))
		tenant, _ = topicSubRow(t, c, s.ID)
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE sessions SET runtime = 'stream', connector_seen_at = now() WHERE `+latest); err != nil || ago == "" {
				return err
			}
			return hooksPolledSQL(tx, latest, ago)
		}); err != nil {
			t.Fatal(err)
		}
		code, body := fetchSetup(t, u)
		switch {
		case ago == "0 seconds" && (code != 200 || body != harnesses.SetupPluginConnected("Ci")):
			t.Fatalf("the plugin's waiter polled: %d %q", code, body)
		case ago != "0 seconds" && (code != 200 || !strings.Contains(body, "start the Monitor tool")):
			t.Fatalf("redeemed, last polled %q: %d %q", ago, code, body)
		}
	}

	// The session's links revoked since (the console, unwatch): its setup
	// URLs are used up with them.
	s, u = sub("toolu_4")
	tenant, _ = topicSubRow(t, c, s.ID)
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var agent, session string
		tok := u[strings.Index(u, "token=")+len("token="):]
		if err := tx.QueryRow(ctx, `SELECT s.agent_id, s.id::text FROM session_setups x JOIN sessions s ON s.id = x.session_id WHERE x.token_hash = sha256(convert_to($1, 'UTF8'))`, tok).Scan(&agent, &session); err != nil {
			return err
		}
		_, err := streams.RevokeSessionLinks(ctx, tx, agent, session)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, body := fetchSetup(t, u); code != http.StatusGone || body != appapi.SetupRefused {
		t.Fatalf("revoked: %d %q", code, body)
	}

	// Unsubscribed since: unwatching revokes the session's links, and its
	// setup URLs with them.
	s, u = sub("toolu_3")
	if _, err := unsubscribe(app, caller{Client: "claude-code", Session: s.Session}, "ci/acme"); err != nil {
		t.Fatal(err)
	}
	if code, body := fetchSetup(t, u); code != http.StatusGone || body != appapi.SetupRefused {
		t.Fatalf("unsubscribed: %d %q", code, body)
	}

	// Nothing left to hear about (the session stopped watching some other
	// way, its last subscription ending).
	s, u = sub("toolu_5")
	tenant, _ = topicSubRow(t, c, s.ID)
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET watching = false`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, body := fetchSetup(t, u); code != 200 || body != harnesses.SetupEnded("Ci") {
		t.Fatalf("unsubscribed: %d %q", code, body)
	}
}
