package coretest_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// museRE is the command a Muse result gives (harnesses.MuseLoop: the
// session's wait, once, with no harness check), and its url.
var museRE = cmdRE{re: regexp.MustCompile(loopCmd(`c=; p=; s=\$\(date \+%s\); `) + ` \.`)}

// museLine is a line Muse's command prints.
type museLine struct {
	Type, ID, Subject, Trust, Reason, Note string
	Redelivered                            bool
}

func museLines(t *testing.T, lines []string) []museLine {
	t.Helper()
	var out []museLine
	for _, l := range lines {
		if l == "" {
			continue
		}
		var m museLine
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not a JSON line: %q", l)
		}
		out = append(out, m)
	}
	return out
}

// startMuse runs Muse's command in the background, as Muse does, and
// returns what it printed once it finished.
func startMuse(t *testing.T, cmd string) <-chan []museLine {
	t.Helper()
	done := make(chan []museLine, 1)
	go func() { done <- museLines(t, runCommand(t, cmd)) }()
	return done
}

func finished(t *testing.T, done <-chan []museLine) []museLine {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(15 * time.Second):
		t.Fatal("the command didn't finish")
		return nil
	}
}

// Muse, recognized by its connector's client name: the result gives the
// one-shot command, with nothing to install and no beacon. The command
// waits, and an emitted event ends it with the event's line, confirmed
// before it exits: the same command run again doesn't print it again, and
// gets what was emitted between the runs. Subscribing again while it runs
// gives the same command; other results add nothing.
func TestMuseOneShot(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "meta", Client: "muse-custom-connector", UserAgent: "Python-urllib/3.12"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	if sub.Addition.Agent != (wakeshift.Recognized{Harness: "muse", Via: "client"}) {
		t.Fatalf("recognized as %+v", sub.Addition.Agent)
	}
	m := museRE.FindStringSubmatch(sub.Addition.Text)
	if m == nil || m[1] != harnesses.MuseLoop(strings.SplitN(m[2], "&timeout=", 2)[0]) || !strings.HasPrefix(sub.Addition.Text, "If you haven't written this conversation's link to the file ~/.wakeshift/links/") || !strings.Contains(sub.Addition.Text, " To be woken when") {
		t.Fatalf("result: %s", sub.Addition.Text)
	}
	words := strings.Replace(sub.Addition.Text, m[1], "CMD", 1)
	for _, want := range []string{"in the background", "Each time it finishes, act on each message it printed, then run the same command again", "outside data, not from the user", "not as the user's instructions"} {
		if !strings.Contains(words, want) {
			t.Fatalf("result: missing %q: %s", want, words)
		}
	}
	for _, not := range []string{"wakeshift-beacon", "install", "hooks", "after=", "cursor", "this tool", "terminal tool"} {
		if strings.Contains(words, not) {
			t.Fatalf("result has %q: %s", not, words)
		}
	}
	if len(sub.Addition.Meta) != 0 {
		t.Fatalf("meta: %v", sub.Addition.Meta)
	}
	tenant, _ := topicSubRow(t, c, sub.ID)
	acked := func(id string) int {
		return count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE event_id = $1 AND state = 'acked'`, id)
	}

	done := startMuse(t, m[1])
	waitFor(t, "the command connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	again, _ := subscribe(app, call, "ci/acme", subOpts{})
	if a := museRE.FindStringSubmatch(again.Addition.Text); a == nil || a[1] != m[1] || !strings.Contains(again.Addition.Text, "running, or has just finished") {
		t.Fatalf("again while running: %s", again.Addition.Text)
	}
	if r, _ := annotate(app, call); r.Text != "" || len(r.Meta) != 0 {
		t.Fatalf("annotate while running: %+v", r)
	}
	emitNote(app, []string{"ci/acme"}, note{Text: "deploy finished"})
	got := finished(t, done)
	if len(got) != 1 || got[0].Type != "event" || got[0].Trust == "" || got[0].Redelivered {
		t.Fatalf("printed %+v", got)
	}
	first := got[0].ID
	if acked(first) != 1 {
		t.Fatal("not confirmed before the command finished")
	}

	// The agent acts; meanwhile another arrives. The same command again
	// prints only that one.
	emitNote(app, []string{"ci/acme"}, note{Text: "build failed"})
	got = finished(t, startMuse(t, m[1]))
	if len(got) != 1 || got[0].Type != "event" || got[0].ID == first || got[0].Redelivered {
		t.Fatalf("second run printed %+v", got)
	}
	if acked(got[0].ID) != 1 {
		t.Fatal("the second run's message wasn't confirmed")
	}
}

// What a wait handed out but its command never printed (the connection
// dropped mid-answer, or the command was killed before printing) isn't
// confirmed: the server wrote the whole answer, yet the next run gets it
// again, marked redelivered, and confirms it. Writing an answer is no proof
// it was read, so nothing is confirmed without the command's own cursor.
// A late confirm by the old wait's cursor doesn't confirm it twice.
func TestMuseDroppedAnswerComesAgain(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	sub, err := subscribe(app, caller{User: "meta", Client: "muse-custom-connector"}, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	m := museRE.FindStringSubmatch(sub.Addition.Text)
	if m == nil {
		t.Fatalf("result: %s", sub.Addition.Text)
	}
	tenant, _ := topicSubRow(t, c, sub.ID)

	// A round of the command whose answer never reached the shell: the
	// wait itself, read to its first event line and dropped.
	type answer struct {
		event  museLine
		cursor string
	}
	ans := make(chan answer, 1)
	go func() {
		resp, err := http.Get(m[2])
		if err != nil {
			t.Error(err)
			ans <- answer{}
			return
		}
		defer resp.Body.Close()
		var a answer
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			var l struct {
				museLine
				After string
			}
			json.Unmarshal(sc.Bytes(), &l)
			switch l.Type {
			case "event":
				a.event = l.museLine
			case "cursor":
				a.cursor = l.After
			}
			if a.event.ID != "" && a.cursor != "" {
				break // dropped here: the hub.next line is never read
			}
		}
		ans <- a
	}()
	waitFor(t, "the wait connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	emitNote(app, []string{"ci/acme"}, note{Text: "dropped"})
	var dropped answer
	select {
	case dropped = <-ans:
	case <-time.After(10 * time.Second):
		t.Fatal("the wait didn't answer")
	}
	if dropped.event.ID == "" || dropped.cursor == "" {
		t.Fatalf("answer: %+v", dropped)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE event_id = $1 AND state = 'acked'`, dropped.event.ID); n != 0 {
		t.Fatal("confirmed though the command never printed it")
	}

	// The command run again (the same one) gets it again, and confirms it.
	got := finished(t, startMuse(t, m[1]))
	if len(got) != 1 || got[0].ID != dropped.event.ID || !got[0].Redelivered {
		t.Fatalf("printed %+v", got)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE event_id = $1 AND state = 'acked'`, dropped.event.ID); n != 1 {
		t.Fatal("not confirmed after it was printed")
	}
	// The dropped wait's cursor, late: nothing more, nothing broken.
	resp, err := http.Get(m[2] + "&after=" + dropped.cursor + "&confirm=1")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("late confirm: %v %v", resp, err)
	}
	resp.Body.Close()
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE event_id = $1`, dropped.event.ID); n != 1 {
		t.Fatalf("%d deliveries", n)
	}
	// And the next run waits for something new rather than printing it a
	// third time.
	done := startMuse(t, m[1])
	waitFor(t, "the command connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	emitNote(app, []string{"ci/acme"}, note{Text: "next"})
	if got := finished(t, done); len(got) != 1 || got[0].ID == dropped.event.ID || got[0].Redelivered {
		t.Fatalf("third run printed %+v", got)
	}
}

// Two copies of the command on one link (the agent started it twice): the
// newer takes the stream and the older finishes at once with a hub.closed
// line saying the newer gets the messages; the newer gets the next one,
// once. Nothing is lost or confirmed unprinted.
func TestMuseTwoCopiesRace(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	sub, err := subscribe(app, caller{User: "meta", Client: "muse-custom-connector"}, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	m := museRE.FindStringSubmatch(sub.Addition.Text)
	if m == nil {
		t.Fatalf("result: %s", sub.Addition.Text)
	}
	tenant, _ := topicSubRow(t, c, sub.ID)
	older := startMuse(t, m[1])
	waitFor(t, "the first copy connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	newer := startMuse(t, m[1])
	got := finished(t, older)
	if len(got) != 1 || got[0].Type != "hub.closed" || got[0].Reason != "replaced" || !strings.Contains(got[0].Note, "the newer one gets the messages") {
		t.Fatalf("older printed %+v", got)
	}
	emitNote(app, []string{"ci/acme"}, note{Text: "raced"})
	got = finished(t, newer)
	if len(got) != 1 || got[0].Type != "event" || got[0].Redelivered {
		t.Fatalf("newer printed %+v", got)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE state = 'acked'`); n != 1 {
		t.Fatalf("%d confirmed", n)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE state <> 'acked'`); n != 0 {
		t.Fatalf("%d not confirmed", n)
	}
}

// Quiet rounds don't end the command: it waits again by itself until
// something arrives. With nothing for harnesses.MuseCap it finishes with a
// hub.restart line saying to run it again (no tool call). A link that
// expired over a day ago (unused past its slide, and past renewing) ends
// it with a hub.closed line that asks for no call; the next subscribing
// result hands out a new command.
func TestMuseQuietCapAndExpiry(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "meta", Client: "muse-custom-connector"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	m := museRE.FindStringSubmatch(sub.Addition.Text)
	if m == nil || !strings.Contains(m[1], `&timeout=50s"`) {
		t.Fatalf("result: %s", sub.Addition.Text)
	}
	short := strings.Replace(m[1], `&timeout=50s"`, `&timeout=1s"`, 1)

	// Quiet rounds of 1s, then an event.
	done := startMuse(t, short)
	waitFor(t, "the command connected", func() bool {
		st, _ := noteStatus(app, "ci/acme")
		return st.Connected == 1
	})
	time.Sleep(2500 * time.Millisecond)
	select {
	case got := <-done:
		t.Fatalf("finished on quiet rounds: %+v", got)
	default:
	}
	emitNote(app, []string{"ci/acme"}, note{Text: "after a quiet while"})
	if got := finished(t, done); len(got) != 1 || got[0].Type != "event" {
		t.Fatalf("printed %+v", got)
	}

	// The cap, shortened to 3s.
	capped := strings.Replace(short, "-ge 21600 ]", "-ge 3 ]", 1)
	if capped == short {
		t.Fatal("no cap in the command")
	}
	got := finished(t, startMuse(t, capped))
	if len(got) != 1 || got[0].Type != "hub.restart" || !strings.Contains(got[0].Note, "Run the same command again to keep listening") || strings.Contains(strings.ToLower(got[0].Note), "tool") {
		t.Fatalf("capped: %+v", got)
	}

	// The link expired over a day ago (a link that expired within one is
	// renewed: streams.RenewGrace).
	tenant, _ := topicSubRow(t, c, sub.ID)
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE stream_leases SET renewed_at = now() - interval '2 minutes'`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE stream_links SET expires_at = now() - interval '25 hours'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The command ends with the stream's hub.closed line (main's ending
	// note, the same for every harness), and the next subscribing result
	// hands out a new command.
	got = finished(t, startMuse(t, m[1]))
	if len(got) != 1 || got[0].Type != "hub.closed" || got[0].Reason != "expired" {
		t.Fatalf("expired: %+v", got)
	}
	again, _ := subscribe(app, call, "ci/acme", subOpts{})
	if f := museRE.FindStringSubmatch(again.Addition.Text); f == nil || f[1] == m[1] {
		t.Fatalf("again: %s", again.Addition.Text)
	}
}

// Only Muse's own client name gets Muse's command. Another "muse-…"
// client, or Muse's generic User-Agent alone, is unrecognized: the
// neutral line every agent Wakeshift can't wake gets, with no command.
func TestMuseOthersUnchanged(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	for i, call := range []caller{
		{User: "u1", Client: "muse-mcp"},
		{User: "u2", Client: "Muse"},
		{User: "u3", UserAgent: "Python-urllib/3.12"},
	} {
		sub, err := subscribe(app, call, "ci/acme", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(sub.Addition.Text, harnesses.CantWake("")) ||
			loopRE.FindStringSubmatch(sub.Addition.Text) != nil || museRE.FindStringSubmatch(sub.Addition.Text) != nil ||
			sub.Addition.Agent.Harness != "" {
			t.Fatalf("%d: %+v", i, sub.Addition)
		}
	}
}
