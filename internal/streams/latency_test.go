package streams_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/notify"
	"github.com/wakeshift/server/internal/streams"
)

// How soon a pending wait answers once a delivery for it is ready. The
// safety recheck is an hour in these tests, so only the notify hub can
// wake a wait in time.
const wakeWithin = 200 * time.Millisecond

// timedWait is one wait's answer, and when its first event line (or, with
// none, its last line) was read.
type timedWait struct {
	at      time.Time
	events  []string // subjects
	ids     []string // event IDs
	cursors []string
	err     error
}

func waitTimed(u string) timedWait {
	resp, err := http.Get(u)
	if err != nil {
		return timedWait{err: err}
	}
	defer resp.Body.Close()
	var w timedWait
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var m struct{ Type, Subject, After, ID string }
		if len(sc.Bytes()) == 0 || json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Type {
		case "event":
			if w.at.IsZero() {
				w.at = time.Now()
			}
			w.events = append(w.events, m.Subject)
			w.ids = append(w.ids, m.ID)
		case "cursor":
			w.cursors = append(w.cursors, m.After)
		default:
			if w.at.IsZero() {
				w.at = time.Now()
			}
		}
	}
	w.err = sc.Err()
	return w
}

// instance is another server (its own notify hub) on f's database, as a
// second instance behind the front end is.
func (f *fixture) instance() (*streams.Server, string) {
	f.t.Helper()
	runCtx, stop := context.WithCancel(ctx)
	f.t.Cleanup(stop)
	hub := notify.New(f.d.Pool, deliveries.NotifyChannel, streams.TakeoverChannel, streams.RevokedChannel)
	go hub.Run(runCtx)
	s := &streams.Server{DB: f.d, Key: []byte(strings.Repeat("k", 32)), Notify: hub, Lease: time.Minute, Poll: time.Hour}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/stream/wait", s.ServeWait)
	srv := httptest.NewServer(mux)
	f.t.Cleanup(srv.Close)
	s.PublicURL = srv.URL
	time.Sleep(100 * time.Millisecond) // let the listener start
	return s, srv.URL
}

// A pending wait answers within moments of a delivery for its session
// becoming ready: woken by the notify hub, whichever instance it waits on
// (the delivery's notification comes through Postgres), not by a recheck.
func TestPendingWaitWakesAtOnce(t *testing.T) {
	for _, where := range []string{"same instance", "other instance"} {
		t.Run(where, func(t *testing.T) {
			f := setup(t)
			f.s.Poll = time.Hour
			base := f.waitServer()
			f.s.PublicURL = base
			if where == "other instance" {
				_, base = f.instance()
			}
			link, id := f.sessionLinkFor("cc", streams.SessionLinkTTL)
			u := waitURL(base, link, "timeout=5s")
			var took []time.Duration
			for i := range 5 {
				done := make(chan timedWait, 1)
				go func() { done <- waitTimed(u) }()
				time.Sleep(300 * time.Millisecond) // the wait is pending
				published := time.Now()
				f.toSession(id, fmt.Sprint("m", i))
				w := <-done
				if w.err != nil || !slices.Equal(w.events, []string{fmt.Sprint("m", i)}) || len(w.cursors) != 1 {
					t.Fatalf("round %d: %v %v %v", i, w.events, w.cursors, w.err)
				}
				took = append(took, w.at.Sub(published))
				u = waitURL(base, link, "timeout=5s&after="+w.cursors[0])
			}
			slices.Sort(took)
			if took[len(took)/2] > wakeWithin || took[len(took)-1] > time.Second {
				t.Fatalf("waits answered %v after the publish (want a median under %v)", took, wakeWithin)
			}
		})
	}
}

// What arrives between one wait answering and the next starting (the
// loop's re-arm gap) isn't missed: the next wait returns it at once, and
// confirms the batch before it.
func TestWaitReArmGapMissesNothing(t *testing.T) {
	f := setup(t)
	f.s.Poll = time.Hour
	base := f.waitServer()
	f.s.PublicURL = base
	link, id := f.sessionLinkFor("cc", streams.SessionLinkTTL)
	f.toSession(id, "first")
	w := waitTimed(waitURL(base, link, "timeout=5s"))
	if !slices.Equal(w.events, []string{"first"}) || len(w.cursors) != 1 {
		t.Fatalf("got %v %v %v", w.events, w.cursors, w.err)
	}
	// No wait is pending: these arrive in the gap.
	f.toSession(id, "in the gap")
	f.toSession(id, "also in the gap")
	start := time.Now()
	next := waitTimed(waitURL(base, link, "timeout=5s&after="+w.cursors[0]))
	if !slices.Equal(next.events, []string{"in the gap", "also in the gap"}) || next.at.Sub(start) > wakeWithin {
		t.Fatalf("got %v after %v", next.events, next.at.Sub(start))
	}
	if s := f.state(f.deliveryOf(w.ids[0])); s != "acked" {
		t.Fatalf("the next wait didn't confirm the batch before it: %s", s)
	}
}

// Claude Code's Monitor loop and Cursor's command (the exact commands the
// watch result gives) print each message within moments of its publish,
// curl and all, and miss none published in quick succession (some land
// while a round is between waits).
func TestLoopsPrintAtOnce(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	for _, kind := range []string{"monitor", "cursor"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			f.s.Poll = time.Hour
			base := f.waitServer()
			f.s.PublicURL = base
			link, id := f.sessionLinkFor(kind, streams.SessionLinkTTL)
			u := waitURL(base, link, "")
			cmd := harnesses.MonitorLoop(u)
			if kind == "cursor" {
				cmd = harnesses.CursorLoop(u, harnesses.Claim{})
			}
			// The link file the agent writes, in a HOME of the command's own.
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, ".wakeshift", "links"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".wakeshift", "links", harnesses.LinkKey(u)), []byte(u+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			c := exec.Command("sh", "-c", cmd)
			c.Env = append(os.Environ(), "HOME="+home)
			c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			out, err := c.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { syscall.Kill(-c.Process.Pid, syscall.SIGKILL); c.Wait() })
			printed := make(chan string, 100)
			go func() {
				sc := bufio.NewScanner(out)
				for sc.Scan() {
					var m struct{ Type, Subject string }
					if json.Unmarshal(sc.Bytes(), &m) == nil && m.Type == "event" {
						printed <- m.Subject
					}
				}
			}()
			line := func() string {
				select {
				case s := <-printed:
					return s
				case <-time.After(10 * time.Second):
					t.Fatal("nothing printed")
					return ""
				}
			}
			var took []time.Duration
			for i := range 5 {
				time.Sleep(300 * time.Millisecond) // a round is pending
				published := time.Now()
				f.toSession(id, fmt.Sprint("idle", i))
				if s := line(); s != fmt.Sprint("idle", i) {
					t.Fatalf("printed %q", s)
				}
				took = append(took, time.Since(published))
			}
			slices.Sort(took)
			if took[len(took)/2] > wakeWithin || took[len(took)-1] > time.Second {
				t.Fatalf("printed %v after the publish (want a median under %v)", took, wakeWithin)
			}
			// A burst, some of it between rounds.
			for i := range 10 {
				f.toSession(id, fmt.Sprint("burst", i))
				time.Sleep(time.Duration(i%3) * 10 * time.Millisecond)
			}
			seen := map[string]bool{}
			for range 10 {
				seen[line()] = true
			}
			for i := range 10 {
				if !seen[fmt.Sprint("burst", i)] {
					t.Fatalf("burst%d missed: %v", i, seen)
				}
			}
		})
	}
}
