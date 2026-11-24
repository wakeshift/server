package coretest_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/coretest"
)

// A routine's run has its own CURSOR_CONVERSATION_ID, not the bot's (as
// observed in Grok Bot). The routine Wakeshift has the bot make
// names the bot itself, so a strict run of it still answers the setup
// challenge (with the key in the bot's folder, signed over the bot's ID)
// and acts on a wake; it still refuses a body for another bot.
func TestGrokRoutineRunsHaveTheirOwnConversationID(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	app := noteApp(t, c, "ci")
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	// The challenge, through the webhook, in a run whose
	// CURSOR_CONVERSATION_ID isn't the bot's: the setup binds.
	sub, call, _ := grokSetUp(t, c, f, app, bot, "gina", "ci/grok", "runIDHook1")
	tenant, _ := topicSubRow(t, c, sub.ID)
	if bot.routine == "" || strings.Contains(bot.routine, "isn't the output of `printenv CURSOR_CONVERSATION_ID`") {
		t.Fatalf("the routine checks the bot against CURSOR_CONVERSATION_ID: %s", bot.routine)
	}
	// An idle wake, in such a run: it acts.
	emitNote(app, []string{"ci/grok"}, note{Text: "wake the bot"})
	waitFor(t, "the delivery", func() bool { _, s := deliveryOf(t, c, tenant, "wake the bot"); return s == "acked" })
	f.mu.Lock()
	wakes := append([]string(nil), f.wakes...)
	f.mu.Unlock()
	if len(wakes) != 1 || !strings.HasPrefix(wakes[0], "woken") {
		t.Fatalf("the wake's run: %v", wakes)
	}
	// The woken run goes on with the app's tools: it passes the bot's ID
	// as its conversation, so its call lands in the bot's session, not a
	// new one of the run's.
	f.mu.Lock()
	conv := f.wakeConv[0]
	f.mu.Unlock()
	run := caller{User: "gina", Client: "Cursor", UserAgent: "Cursor/1.0.0", Harness: "grok-bot", Conversation: conv}
	again, err := subscribe(app, run, "ci/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if again.Session != call.Session {
		t.Fatalf("the run's call landed in session %s, not the bot's %s", again.Session, call.Session)
	}
	// A body naming another bot is still refused.
	f.wake(fakeHook{bot: bot}, agenthooks.CursorBody{Type: agenthooks.TypeEvents, Bot: "9a8b7c6d-5e4f-4321-8abc-0123456789ab"})
	f.mu.Lock()
	last := f.wakes[len(f.wakes)-1]
	f.mu.Unlock()
	if last != "another bot" {
		t.Fatalf("a body for another bot: %s", last)
	}
}

// A routine's run has no file-writing tool (as observed in Grok Bot): it
// answers the setup challenge with the routine's command
// alone, and the webhook is bound.
func TestGrokRoutineRunHasNoFileTool(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	grokSetUp(t, c, f, noteApp(t, c, "ci"), bot, "gina", "ci/grok", "noFileHook1")
	if runs := f.ran(); len(runs) != 1 || !strings.Contains(runs[0], "Wake-ups are on") {
		t.Fatalf("the routine's run: %v", runs)
	}
}

// Nothing from a webhook's body reaches a command: the routine's command
// is fixed text and the connector fetches the check from Wakeshift. A body
// that carries anything at all (shell syntax, quotes, a forged challenge)
// changes nothing: with no check waiting, the run signs nothing.
func TestGrokRoutineIgnoresTheBody(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	grokSetUp(t, c, f, noteApp(t, c, "ci"), bot, "gina", "ci/grok", "hostileHook1")
	if strings.Contains(bot.routine, "CHALLENGE") || strings.Contains(bot.routine, "<<") || strings.Contains(bot.routine, "openssl") {
		t.Fatalf("the routine still takes text into its command: %s", bot.routine)
	}
	canary := filepath.Join(bot.vm.home, "pwned")
	for _, ch := range []string{"wakeshift:v1:setup:'; touch " + canary + "; echo '", "$(touch " + canary + ")", "`touch " + canary + "`",
		"\ntouch " + canary, "wakeshift:v1:setup:" + strings.Repeat("A", 43) + ":" + strings.Repeat("B", 22)} {
		before := len(f.ran())
		f.routine(t, fakeHook{bot: bot}, agenthooks.CursorBody{Type: agenthooks.TypeChallenge, Bot: bot.id, Challenge: ch, App: ch, Notice: ch})
		runs := f.ran()
		if len(runs) != before+1 || !strings.Contains(runs[len(runs)-1], "no check is waiting for this bot") {
			t.Fatalf("body %q: %v", ch, runs[before:])
		}
		if _, err := os.Stat(canary); err == nil {
			t.Fatalf("body %q ran a command", ch)
		}
	}
}

// A setup whose proof never came leaves the user a way forward without
// leaving Grok: after the 10 minutes the same link takes another paste;
// and if the conversation stopped watching meanwhile (which revokes the
// link with its stream link), watching again gives a new link, not the
// dead one.
func TestGrokSetupRetry(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	app := noteApp(t, c, "ci")
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	call := bot.call("gina")
	sub, err := subscribe(app, call, "ci/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	tenant, _ := topicSubRow(t, c, sub.ID)
	token := setupToken(t, bot.keyStep(t, sub.Addition.Text))
	url, key := f.webhook("retryHook1", bot)

	// The proof never comes (a run that couldn't answer); its 10 minutes pass.
	f.set("retryHook1", func(h *fakeHook) { h.silent = true })
	if code, _ := save(t, c, token, url, key); code != 200 {
		t.Fatal(code)
	}
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_webhook_setups SET proof_due = now() - interval '1 second'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, out := describe(t, c, token); code != 200 || out["state"] != "open" || out["error"] != "unconfirmed" {
		t.Fatalf("after the timeout: %d %v", code, out)
	}

	// The conversation stops watching, then watches again: a new link.
	if _, err := unsubscribe(app, call, "ci/grok"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the old link to stop working", func() bool { code, _ := describe(t, c, token); return code == 401 })
	again, err := subscribe(app, call, "ci/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	token2 := setupToken(t, again.Addition.Text)
	if token2 == token {
		t.Fatal("watching again gave the dead link")
	}
	if code, out := describe(t, c, token2); code != 200 || out["state"] != "open" {
		t.Fatalf("the new link: %d %v", code, out)
	}
	// The user pastes again; this time the run answers.
	f.set("retryHook1", func(h *fakeHook) { h.silent = false })
	if code, out := save(t, c, token2, url, key); code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	waitOn(t, c, token2)
}

// The install command, exactly as the key step gives it, against the
// core's own installer path: it installs the connector the core serves,
// checks it on later runs without downloading again, replaces a binary
// that isn't the core's, and installs nothing from a download whose
// checksum doesn't match.
func TestGrokInstallCommand(t *testing.T) {
	c := startGrok(t, nil)
	app := noteApp(t, c, "ci")
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	sub, err := subscribe(app, bot.call("gina"), "ci/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	m := installCmdRE.FindStringSubmatch(sub.Addition.Text)
	if m == nil {
		t.Fatalf("no install command: %s", sub.Addition.Text)
	}
	install := m[1]
	if !strings.Contains(install, c.URL+"/download/wakeshift/") {
		t.Fatalf("the install command doesn't fetch from the core's installer path: %s", install)
	}
	want, err := os.ReadFile(filepath.Join(coretest.ConnectorDir(t), "wakeshift-"+runtime.GOOS+"-"+runtime.GOARCH))
	if err != nil {
		t.Fatal(err)
	}
	installed := func() {
		t.Helper()
		if out := bot.vm.run(t, bot.id, install); strings.TrimSpace(out) != "Wakeshift's connector is installed." {
			t.Fatalf("install: %s", out)
		}
		got, err := os.ReadFile(bot.vm.bin())
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("the installed connector isn't the core's: %v", err)
		}
		if fi, _ := os.Stat(bot.vm.bin()); fi.Mode().Perm() != 0o755 {
			t.Fatalf("the connector's mode: %v", fi.Mode())
		}
		if fi, _ := os.Stat(filepath.Join(bot.vm.home, ".wakeshift")); fi.Mode().Perm() != 0o700 {
			t.Fatalf("~/.wakeshift's mode: %v", fi.Mode())
		}
		entries, _ := os.ReadDir(filepath.Dir(bot.vm.bin()))
		if len(entries) != 1 {
			t.Fatalf("left behind in the bin folder: %v", entries)
		}
	}
	installed()
	// Again: checked, not downloaded (the file is the same one).
	before, _ := os.Stat(bot.vm.bin())
	installed()
	if after, _ := os.Stat(bot.vm.bin()); !os.SameFile(before, after) {
		t.Fatal("a second run downloaded the connector again")
	}
	// A binary that isn't the core's is replaced.
	if err := os.WriteFile(bot.vm.bin(), []byte("#!/bin/sh\necho not the connector\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	installed()
	// A download whose checksum doesn't match installs nothing.
	if err := os.Remove(bot.vm.bin()); err != nil {
		t.Fatal(err)
	}
	sumRE := regexp.MustCompile(`want=[0-9a-f]{64}`)
	if !sumRE.MatchString(install) {
		t.Fatalf("no checksum in the install command: %s", install)
	}
	bad := sumRE.ReplaceAllString(install, "want="+strings.Repeat("0", 64))
	if out := bot.vm.run(t, bot.id, bad); strings.TrimSpace(out) != "The download failed or didn't match its checksum; nothing was installed." {
		t.Fatalf("a mismatched download: %s", out)
	}
	if entries, _ := os.ReadDir(filepath.Dir(bot.vm.bin())); len(entries) != 0 {
		t.Fatalf("a mismatched download left %v", entries)
	}
	// A download that fails installs nothing either.
	gone := strings.Replace(install, "/download/wakeshift/", "/download/nothing-here/", 1)
	if out := bot.vm.run(t, bot.id, gone); !strings.HasSuffix(strings.TrimSpace(out), "The download failed or didn't match its checksum; nothing was installed.") {
		t.Fatalf("a failed download: %s", out)
	}
	if entries, _ := os.ReadDir(filepath.Dir(bot.vm.bin())); len(entries) != 0 {
		t.Fatalf("a failed download left %v", entries)
	}
}

// A VM reset (the connector and the key gone): the routine's run says the
// command isn't found and changes nothing; the user's re-setup in a chat
// (install, init, prove, then the page) binds the webhook again, and the
// first app's messages wake the bot again.
func TestGrokVMReset(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	a := noteApp(t, c, "ci")
	asub, _, _ := grokSetUp(t, c, f, a, bot, "gina", "ci/grok", "resetHook1")
	atenant, _ := topicSubRow(t, c, asub.ID)
	url, key := f.webhook("resetHook1", bot)
	bot.vm.reset(t)

	before := len(f.ran())
	f.routine(t, fakeHook{bot: bot}, agenthooks.CursorBody{Type: agenthooks.TypeChallenge, Bot: bot.id})
	runs := f.ran()
	if len(runs) != before+1 || !(strings.Contains(runs[before], "not found") || strings.Contains(runs[before], "No such file")) {
		t.Fatalf("the routine's run after a reset: %v", runs[before:])
	}
	if _, err := os.Stat(bot.vm.bin()); err == nil {
		t.Fatal("the routine's run installed the connector")
	}
	if _, err := os.Stat(bot.vm.dir(bot.id)); err == nil {
		t.Fatal("the routine's run made a key folder")
	}

	b := noteApp(t, c, "cd")
	bcall := bot.call("gina")
	bsub, err := subscribe(b, bcall, "cd/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	token := setupToken(t, bot.keyStep(t, bsub.Addition.Text))
	if code, out := save(t, c, token, url, key); code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	waitOn(t, c, token)
	if runs := f.ran(); !strings.Contains(runs[len(runs)-1], "Wake-ups are on") {
		t.Fatalf("the re-setup's run: %v", runs[before:])
	}
	emitNote(a, []string{"ci/grok"}, note{Text: "after the reset"})
	waitFor(t, "the first app's message", func() bool { _, s := deliveryOf(t, c, atenant, "after the reset"); return s == "acked" })
}
