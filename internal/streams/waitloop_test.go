package streams_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// A loop-mode round (waitloop.go) as the one-line command runs it.
type round struct {
	code   int
	events []map[string]any
	closed map[string]any // the hub.closed line, if any
	other  []string       // any other line: there must be none
}

func loopRound(t *testing.T, u string) round {
	t.Helper()
	resp, err := waitClient.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := round{code: resp.StatusCode}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		switch m["type"] {
		case "event":
			r.events = append(r.events, m)
		case "hub.closed":
			r.closed = m
		default:
			r.other = append(r.other, sc.Text())
		}
	}
	if len(r.other) > 0 {
		t.Fatalf("a loop's round printed more than events: %v", r.other)
	}
	return r
}

// loopSession registers a watching session (claude: Claude Code's, so its
// rounds default to LoopTimeout and end once its hooks connect) and mints
// its sliding link, as a Monitor's.
func (f *fixture) loopSession(label string, claude bool) (link, id string) {
	f.t.Helper()
	f.s.LoopPause = -1 // refusals at once (TestWaitLoopRefusalsPause has the pause)
	f.tx(func(tx pgx.Tx) error {
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "codex", Label: label, CreatedBy: "test"})
		if err != nil {
			return err
		}
		id = s.ID
		if err := sessions.SetWatching(ctx, tx, id, true); err != nil {
			return err
		}
		if claude {
			if _, err := tx.Exec(ctx, `UPDATE sessions SET harness = 'claude-code' WHERE id = $1`, id); err != nil {
				return err
			}
		}
		link, err = f.mintFor(tx, id, true, streams.SessionLinkTTL)
		return err
	})
	return link, id
}

func loopURL(base, link, loop, query string) string {
	u := waitURL(base, link, "loop="+loop)
	if query != "" {
		u += "&" + query
	}
	return u
}

// The next round of the same loop confirms what the last printed; the
// body holds only the events (no cursor line, no hub.next).
func TestWaitLoopNextRoundConfirms(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.loopSession("loop-confirms", false)
	f.toSession(id, "one")
	f.toSession(id, "two")

	r := loopRound(t, loopURL(base, link, "111", "timeout=300ms"))
	if r.code != 200 || len(r.events) != 2 || r.closed != nil || r.events[0]["trust"] != "outside data, not from the user" {
		t.Fatalf("first round: %+v", r)
	}
	one, two := f.deliveryOf(r.events[0]["id"].(string)), f.deliveryOf(r.events[1]["id"].(string))
	if f.state(one) != "in_flight" || f.state(two) != "in_flight" {
		t.Fatalf("returned is confirmed already: %s %s", f.state(one), f.state(two))
	}
	start := time.Now()
	r = loopRound(t, loopURL(base, link, "111", "timeout=300ms"))
	if r.code != 200 || len(r.events) != 0 || r.closed != nil || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("second round: %+v", r)
	}
	if f.state(one) != "acked" || f.state(two) != "acked" {
		t.Fatalf("not confirmed by the next round: %s %s", f.state(one), f.state(two))
	}
	// And the next event arrives once, on a round that's waiting.
	go func() { time.Sleep(200 * time.Millisecond); f.toSession(id, "three") }()
	r = loopRound(t, loopURL(base, link, "111", "timeout=5s"))
	if len(r.events) != 1 || r.events[0]["subject"] != "three" || r.events[0]["redelivered"] != nil {
		t.Fatalf("third round: %+v", r)
	}
}

// Anything but the same loop's next round, in time, from the holder that
// printed it, hands the batch back: it comes again (redelivered), never
// lost. Duplicates rather than misses.
func TestWaitLoopHandsBackWhatItCantConfirm(t *testing.T) {
	cases := []struct {
		name    string
		between func(f *fixture, base, link, id string)
		next    string // the next round's loop
	}{
		// The agent started the Monitor again (a new shell, a new $$).
		{"another loop", func(*fixture, string, string, string) {}, "222"},
		// Something else took the stream in between (Wakeshift's hooks
		// connecting, which hand back what it held).
		{"taken in between", func(f *fixture, _, _, id string) {
			f.tx(func(tx pgx.Tx) error { return streams.TakeOver(ctx, tx, "codex", id) })
		}, "111"},
		// Its answer wasn't written in full (the write failed).
		{"not written in full", func(f *fixture, _, _, id string) {
			f.tx(func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE stream_leases SET loop_flushed = NULL WHERE target = $1`, id)
				return err
			})
		}, "111"},
		// The lease ran out before the next round.
		{"past its lease", func(f *fixture, _, _, id string) {
			f.tx(func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE deliveries SET lease_until = now() - interval '1 second' WHERE target = $1 AND state = 'in_flight'`, id)
				return err
			})
		}, "111"},
		// The same id, but not as the loop's next round (it came after the
		// grace: a new Monitor that got the same $$, or a PID used again).
		{"same id, later", func(f *fixture, _, _, id string) {
			f.tx(func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = now() - interval '2 minutes' WHERE target = $1`, id)
				return err
			})
		}, "111"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			base := f.waitServer()
			link, id := f.loopSession("loop-back", false)
			f.toSession(id, "one")
			r := loopRound(t, loopURL(base, link, "111", "timeout=300ms"))
			if len(r.events) != 1 {
				t.Fatalf("first round: %+v", r)
			}
			d := f.deliveryOf(r.events[0]["id"].(string))
			c.between(f, base, link, id)
			if f.state(d) == "acked" {
				t.Fatalf("confirmed before the next round")
			}
			r = loopRound(t, loopURL(base, link, c.next, "timeout=2s"))
			if r.code != 200 || len(r.events) != 1 || r.events[0]["subject"] != "one" || r.events[0]["redelivered"] != true {
				t.Fatalf("handed back: %+v (state %s)", r, f.state(d))
			}
			// Its own next round confirms it this time.
			loopRound(t, loopURL(base, link, c.next, "timeout=100ms"))
			if f.state(d) != "acked" {
				t.Fatalf("not confirmed the second time: %s", f.state(d))
			}
		})
	}
}

// Two loops on one link: the newest takes the stream, the older one's
// round ends with nothing, and its next round, while the newer one is
// live, answers 410 saying so, so the two never take turns.
func TestWaitLoopTwoLoops(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.loopSession("loop-two", false)

	older := make(chan round, 1)
	go func() { older <- loopRound(t, loopURL(base, link, "111", "timeout=10s")) }()
	time.Sleep(300 * time.Millisecond)
	newer := make(chan round, 1)
	go func() { newer <- loopRound(t, loopURL(base, link, "222", "timeout=3s")) }()
	r := <-older
	if r.code != 200 || len(r.events) != 0 || r.closed != nil {
		t.Fatalf("the older round, replaced: %+v", r)
	}
	r = loopRound(t, loopURL(base, link, "111", "timeout=10s"))
	if r.code != http.StatusGone || r.closed == nil || r.closed["reason"] != "replaced" {
		t.Fatalf("the older loop's next round: %+v", r)
	}
	f.toSession(id, "one")
	r = <-newer
	if len(r.events) != 1 {
		t.Fatalf("the newer loop: %+v", r)
	}
}

// A round that can't go on answers with a status, so `while curl
// --fail-with-body` ends, and its body is the hub.closed line saying why.
func TestWaitLoopClosedIsAStatus(t *testing.T) {
	f := setup(t)
	base := f.waitServer()

	// A token that isn't a link: 401.
	if r := loopRound(t, base+"/v1/stream/wait?link=nope&loop=1"); r.code != 401 || r.closed == nil {
		t.Fatalf("not a link: %+v", r)
	}
	// No token at all (the link file missing or empty, as when an agent
	// wrote it to a literal "$HOME/…" path): 401, saying to write the file.
	if r := loopRound(t, base+"/v1/stream/wait?loop=1&link="); r.code != 401 || r.closed == nil || r.closed["reason"] != "link_file" {
		t.Fatalf("no link: %+v", r)
	}
	// A bad loop id: 400.
	link, id := f.loopSession("loop-closed", false)
	if r := loopRound(t, loopURL(base, link, "a.b", "")); r.code != 400 {
		t.Fatalf("bad loop: %+v", r)
	}
	// Revoked: 410, at the start.
	f.tx(func(tx pgx.Tx) error {
		_, err := streams.RevokeSessionLinks(ctx, tx, "codex", id)
		return err
	})
	if r := loopRound(t, loopURL(base, link, "1", "timeout=1s")); r.code != http.StatusGone || r.closed == nil {
		t.Fatalf("revoked: %+v", r)
	}

	// Revoked while a round waits: that round ends with nothing more (its
	// 200 is out), and the next one says why with the status.
	link, id = f.loopSession("loop-closed-2", false)
	done := make(chan round, 1)
	go func() { done <- loopRound(t, loopURL(base, link, "1", "timeout=10s")) }()
	time.Sleep(300 * time.Millisecond)
	f.tx(func(tx pgx.Tx) error {
		_, err := streams.RevokeSessionLinks(ctx, tx, "codex", id)
		return err
	})
	if r := <-done; r.code != 200 || r.closed != nil || len(r.events) != 0 {
		t.Fatalf("revoked while waiting: %+v", r)
	}
	if r := loopRound(t, loopURL(base, link, "1", "timeout=1s")); r.code != http.StatusGone || r.closed == nil {
		t.Fatalf("the round after: %+v", r)
	}
}

// Claude Code's session: its rounds end once Wakeshift's hooks connect it,
// without until=hooks, with 410 and the note that they're connected.
func TestWaitLoopClaudeCodeEndsWhenHooksConnect(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.loopSession("loop-claude", true)
	f.tx(func(tx pgx.Tx) error { f.hooksRedeem(tx, id, true); return nil })
	r := loopRound(t, loopURL(base, link, "1", "timeout=1s"))
	if r.code != http.StatusGone || r.closed == nil || r.closed["reason"] != "hooks" || !strings.HasPrefix(r.closed["note"].(string), "Wakeshift: connected") {
		t.Fatalf("hooks connected: %+v", r)
	}
	// Another harness's session isn't stopped by hooks it doesn't wait for.
	link, id = f.loopSession("loop-other", false)
	f.tx(func(tx pgx.Tx) error { f.hooksRedeem(tx, id, true); return nil })
	if r := loopRound(t, loopURL(base, link, "1", "timeout=200ms")); r.code != 200 || r.closed != nil {
		t.Fatalf("not Claude Code's: %+v", r)
	}
}

// The server follows its own rotation: a round on a link that expired
// while nothing used it renews it and waits on the successor, and the same
// link from the file keeps working after that, served as the newest of its
// chain. Nothing is lost on the way.
func TestWaitLoopFollowsRenewals(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.loopSession("loop-renew", false)
	f.expire(link, time.Hour)
	f.toSession(id, "one")

	r := loopRound(t, loopURL(base, link, "1", "timeout=300ms"))
	if r.code != 200 || len(r.events) != 1 {
		t.Fatalf("on an expired link: %+v", r)
	}
	if !f.renewed(link) {
		t.Fatal("not renewed")
	}
	d := f.deliveryOf(r.events[0]["id"].(string))
	// The file still holds the old link: the next round follows the chain,
	// and confirms the batch.
	r = loopRound(t, loopURL(base, link, "1", "timeout=300ms"))
	if r.code != 200 || len(r.events) != 0 {
		t.Fatalf("following the chain: %+v", r)
	}
	if f.state(d) != "acked" {
		t.Fatalf("not confirmed across the renewal: %s", f.state(d))
	}
	// Twice renewed: still followed.
	var succ string
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT successor_id::text FROM stream_links WHERE id = $1`, linkID(t, link)).Scan(&succ)
	})
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_links SET expires_at = now() - interval '1 hour' WHERE id = $1`, succ)
		return err
	})
	f.toSession(id, "two")
	if r = loopRound(t, loopURL(base, link, "1", "timeout=300ms")); r.code != 200 || len(r.events) != 1 || r.events[0]["subject"] != "two" {
		t.Fatalf("after a second renewal: %+v", r)
	}

	// Revoking the session ends the chain: 410.
	f.tx(func(tx pgx.Tx) error {
		_, err := streams.RevokeSessionLinks(ctx, tx, "codex", id)
		return err
	})
	if r = loopRound(t, loopURL(base, link, "1", "timeout=300ms")); r.code != http.StatusGone || r.closed == nil {
		t.Fatalf("after revoking: %+v", r)
	}
}

// Loop mode is only for a conversation's own (sliding) link: a connector's
// link, which the hooks keep, is refused (400), and isn't renewed or
// followed.
func TestWaitLoopOnlyOnAnAgentsOwnLink(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	f.s.LoopPause = -1
	link, _ := f.graceSession("loop-connector", false, streams.SessionLinkTTL)
	f.expire(link, time.Hour)
	if r := loopRound(t, loopURL(base, link, "1", "timeout=1s")); r.code != 400 || r.closed == nil {
		t.Fatalf("a connector's link: %+v", r)
	}
	if f.renewed(link) {
		t.Fatal("renewed through loop mode")
	}
}

// A Claude Code loop ends itself (410, hub.restart) once it has run for
// LoopTimeout, before Claude Code stops its Monitor mid-round, confirming
// its last batch first; and a round never waits past that point.
func TestWaitLoopRestartsBeforeTheMonitorsLimit(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.loopSession("loop-restart", true)
	f.toSession(id, "one")
	r := loopRound(t, loopURL(base, link, "1", ""))
	if len(r.events) != 1 {
		t.Fatalf("first round: %+v", r)
	}
	d := f.deliveryOf(r.events[0]["id"].(string))
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_leases SET loop_since = now() - interval '26 minutes' WHERE target = $1`, id)
		return err
	})
	resp, err := waitClient.Get(loopURL(base, link, "1", ""))
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	json.NewDecoder(resp.Body).Decode(&line)
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone || line["type"] != "hub.restart" || !strings.Contains(line["note"].(string), "without asking the user") {
		t.Fatalf("after 26 minutes: %d %v", resp.StatusCode, line)
	}
	if f.state(d) != "acked" {
		t.Fatalf("its last batch, not confirmed: %s", f.state(d))
	}
	// The new Monitor (a new id) starts its own 25 minutes.
	start := time.Now()
	if r := loopRound(t, loopURL(base, link, "2", "timeout=300ms")); r.code != 200 || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("the new Monitor: %+v", r)
	}
}

// A refusal comes after a pause, so a loop that goes round on one anyway
// can't spin.
func TestWaitLoopRefusalsPause(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	f.s.LoopPause = 500 * time.Millisecond
	start := time.Now()
	if r := loopRound(t, base+"/v1/stream/wait?link=nope&loop=1"); r.code != 401 || time.Since(start) < 500*time.Millisecond {
		t.Fatalf("%+v after %v", r, time.Since(start))
	}
}

// Without loop=, the wait is as it was: the cursor line and hub.next.
func TestWaitWithoutLoopUnchanged(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.loopSession("no-loop", true)
	f.toSession(id, "one")
	w := wait(t, waitURL(base, link, "timeout=300ms"))
	if w.code != 200 || len(w.events) != 1 || len(w.cursors) != 1 || w.last["type"] != "hub.next" {
		t.Fatalf("got %+v", w)
	}
}

// loopLine is the one-line command (docs/harnesses.md, "The wait's loop
// mode"), for a link file named name, with query added (a test's short
// timeout).
func loopLine(name, u, query string) string {
	prefix, _, _ := strings.Cut(u, "?")
	return `chmod 700 "$HOME/.wakeshift/links"; t=$(cat "$HOME/.wakeshift/links/` + name + `"); n=0; i=$$-$(date +%s); ` +
		`while :; do curl -sgN --fail-with-body -m 30 "` + prefix + `?loop=$i` + query + `&link=${t#*link=}"; c=$?; ` +
		`[ $c = 0 ] && continue; [ $c = 22 ] && break; n=$((n+1)); i=$$-$(date +%s)-$n; sleep 5; done`
}

// startLoop runs the one-line command in sh, in a HOME of its own with
// the link file written as an agent's file tool writes it (0644, in a
// folder it made 0755), and returns what it prints, line by line, a
// channel closed when the command exits, and what stops it.
func startLoop(t *testing.T, u, query string) (<-chan map[string]any, <-chan struct{}, func()) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".wakeshift", "links")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0123456789abcdef"), []byte(u+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sh", "-c", loopLine("0123456789abcdef", u, query))
	c.Env = append(os.Environ(), "HOME="+home)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	kill := func() { syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	printed := make(chan map[string]any, 100)
	exited := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				printed <- m
			}
		}
		c.Wait()
		close(exited)
	}()
	t.Cleanup(func() { kill(); <-exited })
	deadline := time.Now().Add(5 * time.Second)
	for {
		fi, err := os.Stat(dir)
		if err == nil && fi.Mode().Perm() == 0o700 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the links folder isn't made private: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return printed, exited, kill
}

func nextLine(t *testing.T, printed <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case m := <-printed:
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("nothing printed")
		return nil
	}
}

func quiet(t *testing.T, printed <-chan map[string]any, d time.Duration) {
	t.Helper()
	select {
	case m := <-printed:
		t.Fatalf("printed again: %v", m)
	case <-time.After(d):
	}
}

// The one line, run by sh as a Monitor runs it: two builds in one
// conversation, each event printed once (no duplicate of the first when
// the second comes, none missed); a Monitor stopped and started again (a
// new $$) gets an unconfirmed batch again rather than losing it; and the
// loop ends by itself, printing why, once the link is revoked.
func TestWaitLoopOneLine(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	f := setup(t)
	base := f.waitServer()
	f.s.PublicURL = base
	link, id := f.loopSession("one-line", true)
	u := onServer(base, streams.WaitURL(link))
	printed, exited, kill := startLoop(t, u, "&timeout=2s")

	for _, build := range []string{"build 1 finished", "build 2 finished"} {
		time.Sleep(500 * time.Millisecond)
		f.toSession(id, build)
		if m := nextLine(t, printed); m["type"] != "event" || m["subject"] != build || m["redelivered"] != nil {
			t.Fatalf("%s: printed %v", build, m)
		}
		quiet(t, printed, 3*time.Second) // past a quiet round: nothing again
	}

	// Stopped while a round waits (as Claude Code stops a Monitor after
	// 30 minutes), with a message on its way: the new Monitor (a new $$)
	// gets it.
	kill()
	<-exited
	f.toSession(id, "while it was stopped")
	printed, exited, _ = startLoop(t, u, "&timeout=2s")
	if m := nextLine(t, printed); m["subject"] != "while it was stopped" {
		t.Fatalf("after a restart: %v", m)
	}
	quiet(t, printed, 3*time.Second)

	// Revoked: the loop prints the hub.closed line and ends.
	f.tx(func(tx pgx.Tx) error {
		_, err := streams.RevokeSessionLinks(ctx, tx, "codex", id)
		return err
	})
	if m := nextLine(t, printed); m["type"] != "hub.closed" {
		t.Fatalf("after revoking: %v", m)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop didn't end")
	}
}
