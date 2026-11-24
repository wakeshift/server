package coretest_test

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

// cursorShellRE is the Shell call a Cursor result gives (group 1), as the
// text has it.
var cursorShellRE = regexp.MustCompile(`(\{"block_until_ms":0,"command":"f=\$HOME/\.wakeshift/links/.*?","notify_on_output":\{.*?\}\}) \.`)

// secretRE is what no text a Cursor agent is served may hold (#1152): a
// stream link or its token, a session key, and the session's handle.
var secretRE = regexp.MustCompile(`/v1/stream(?:/wait)?\?link=[A-Za-z0-9._~%-]|\bwsc_[A-Za-z0-9_-]`)

// shellCommand is the command of the Shell call in a Cursor result.
func shellCommand(t *testing.T, text string) string {
	t.Helper()
	m := cursorShellRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no Shell call: %s", text)
	}
	var shell struct{ Command string }
	if err := json.Unmarshal([]byte(m[1]), &shell); err != nil {
		t.Fatal(err)
	}
	return shell.Command
}

// A Cursor conversation where the app's user is signed in (#1152): no
// text it's given holds its link or a session key. Its command claims the
// link with a one-time code the first time it runs, writes the link file
// itself (private from the start), and teaches the session its
// conversation's ID, which finds the conversation from then on. The code
// works once, not while a command listens, and not after the session's
// links are revoked; it's no setup URL.
func TestCursorCommandClaimsItsLink(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	// The agent's first call, before it ran printenv: no conversation ID.
	call := caller{User: "cl", Client: "Cursor", Harness: "cursor"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	text := sub.Addition.Text
	if secretRE.MatchString(text) || strings.Contains(text, sub.Session) || coretest.LinkStepRE.MatchString(text) || strings.Contains(text, harnesses.SessionArg) {
		t.Fatalf("the result holds a secret, or asks for the key or a file: %s", text)
	}
	cmd := shellCommand(t, text)
	claim := coretest.ClaimRE.FindStringSubmatch(cmd)
	if claim == nil || !strings.Contains(text, "printenv CURSOR_CONVERSATION_ID") {
		t.Fatalf("no claim, or no ask for the ID: %s", text)
	}

	// The command, run as Cursor's Shell runs it, in a HOME of its own.
	home := t.TempDir()
	const conv = "7a1e2b3c-4d5e-4f60-8a9b-0c1d2e3f4a5b"
	proc := exec.Command("sh", "-c", cmd)
	proc.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "CURSOR_CONVERSATION_ID=" + conv}
	proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := proc.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { syscall.Kill(-proc.Process.Pid, syscall.SIGKILL); proc.Wait() }) }
	t.Cleanup(stop)
	lines := readFrom(t, out)
	waitFor(t, "the command connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	files, _ := filepath.Glob(filepath.Join(home, ".wakeshift", "links", "*"))
	if len(files) != 1 {
		t.Fatalf("link files: %v", files)
	}
	for p, want := range map[string]os.FileMode{files[0]: 0o600, filepath.Dir(files[0]): 0o700} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", p, fi.Mode(), err)
		}
	}
	emitNote(app, []string{"ci/acme"}, note{Text: "build passed"})
	lines.expect("build passed")

	// The claim taught the session its conversation: a call that passes
	// only the ID finds it, and is told to keep passing the ID (its value),
	// never a key.
	tenant, _ := topicSubRow(t, c, sub.ID)
	if got := sessionConversation(t, c, tenant, sub.Session); got != conv {
		t.Fatalf("the session learned %q", got)
	}
	byID := caller{User: "cl", Client: "Cursor", Conversation: conv}
	again, err := subscribe(app, byID, "ci/acme", subOpts{})
	if err != nil || again.Session != sub.Session {
		t.Fatalf("by its ID: %+v %v", again, err)
	}
	if secretRE.MatchString(again.Addition.Text) || strings.Contains(again.Addition.Text, sub.Session) ||
		!strings.Contains(again.Addition.Text, `Pass conversation_id="`+conv+`" to this app's tools from now on`) || !strings.Contains(again.Addition.Text, "already running") {
		t.Fatalf("again: %s", again.Addition.Text)
	}

	// The code works once; a later result's code, while the command
	// listens, hands nothing over (it came from a screen or a transcript).
	if link, status := coretest.ClaimLink(claim[2], claim[1], ""); link != "" || status != http.StatusGone {
		t.Fatalf("used twice: %d", status)
	}
	later := coretest.ClaimRE.FindStringSubmatch(shellCommand(t, again.Addition.Text))
	if later == nil || later[1] == claim[1] {
		t.Fatalf("no new code: %s", again.Addition.Text)
	}
	if link, status := coretest.ClaimLink(later[2], later[1], ""); link != "" || status != http.StatusGone {
		t.Fatalf("claimed while the command listens: %d", status)
	}
	// It was used up by that refusal, too.
	stop()
	if link, status := coretest.ClaimLink(later[2], later[1], ""); link != "" || status != http.StatusGone {
		t.Fatalf("a refused code worked later: %d", status)
	}

	// A code is no setup URL, and a code from another result (made up, or
	// cut short) names nothing.
	if resp, err := http.Get(c.URL + "/v1/stream/setup?token=" + later[1]); err != nil || resp.StatusCode != http.StatusGone {
		t.Fatalf("a code as a setup URL: %v %v", resp, err)
	}
	for _, bad := range []string{"", later[1][:len(later[1])-1], "wso_" + tenant + "_" + strings.Repeat("A", 43), strings.Replace(later[1], "wso_", "wsu_", 1)} {
		if link, status := coretest.ClaimLink(later[2], bad, ""); link != "" || status != http.StatusGone {
			t.Fatalf("%q: %d", bad, status)
		}
	}

	// Once it stops watching (its links revoked), its codes hand nothing over.
	third, _ := subscribe(app, byID, "ci/other", subOpts{})
	code := coretest.ClaimRE.FindStringSubmatch(shellCommand(t, third.Addition.Text))
	if code == nil {
		t.Fatalf("third: %s", third.Addition.Text)
	}
	for _, topic := range []string{"ci/acme", "ci/other"} {
		if _, err := unsubscribe(app, byID, topic); err != nil {
			t.Fatal(err)
		}
	}
	if link, status := coretest.ClaimLink(code[2], code[1], ""); link != "" || status != http.StatusGone {
		t.Fatalf("claimed after the links were revoked: %d", status)
	}
}

// Without a user, a conversation's session key is still what finds it
// (a conversation ID anyone could report never does), so its results still
// ask for the key; but no Cursor text holds its link: the command claims
// it. A key's holder who calls the tool gets a code, which hands nothing
// over while the conversation's command listens.
func TestCursorWithoutUserClaims(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{Client: "Cursor", Harness: "cursor"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	text := sub.Addition.Text
	if coretest.LeaksLink(text) || coretest.LinkStepRE.MatchString(text) || !strings.Contains(text, `Pass session_key="`+sub.Session+`"`) {
		t.Fatalf("result: %s", text)
	}
	cmd := shellCommand(t, text)
	runMonitor(t, cmd) // the command claims its link itself
	waitFor(t, "the command connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	call.Session = sub.Session
	thief, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	code := coretest.ClaimRE.FindStringSubmatch(shellCommand(t, thief.Addition.Text))
	if code == nil {
		t.Fatalf("again: %s", thief.Addition.Text)
	}
	if link, status := coretest.ClaimLink(code[2], code[1], ""); link != "" || status != http.StatusGone {
		t.Fatalf("a key's holder claimed the link from under the command: %d", status)
	}
}
