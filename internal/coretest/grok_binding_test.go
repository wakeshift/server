package coretest_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/botkeys"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Stopping wake-ups is the agent's to do (there's no page for it): the
// user asks it to stop watching, and its unsubscribe ends this app's
// destination, audited. Deleting the routine in Grok ends every app's,
// since Cursor then refuses Wakeshift's POSTs. And one routine has one
// binding: a re-setup carries its key to every app's destination on it.

// liveHook: the workspace has its Cursor destination, live.
func liveHook(t *testing.T, c *coretest.Core, tenant string) bool {
	t.Helper()
	st, _, ns := hookState(t, c, tenant)
	return st == "live" && !ns
}

// ciPosts is how many POSTs of events from app name the fake Cursor got.
func ciPosts(f *fakeCursor, name string) int {
	n := 0
	for _, r := range f.of(agenthooks.TypeEvents) {
		if r.Body.App == name {
			n++
		}
	}
	return n
}

// Asked to stop watching, the agent unsubscribes: this app's destination
// ends (audited, reason unwatched), and its next publish starts no run;
// another app that joined the bot's webhook keeps waking it. Deleting the
// routine then stops every app's: Cursor refuses the POSTs, and the
// destination goes dead.
func TestGrokUnwatchStopsWakeups(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	a := noteApp(t, c, "ci")
	asub, acall, _ := grokSetUp(t, c, f, a, bot, "gina", "ci/grok", "stopHook1")
	atenant, _ := topicSubRow(t, c, asub.ID)
	b := noteApp(t, c, "cd")
	bsub, _ := subscribe(b, bot.call("gina"), "cd/grok", subOpts{})
	if out := bot.keyStep(t, bsub.Addition.Text); strings.TrimSpace(out) != harnesses.GrokJoinedNote("Cd") {
		t.Fatalf("join: %s", out)
	}
	btenant, _ := topicSubRow(t, c, bsub.ID)

	// The user asks the agent to stop watching Ci.
	if _, err := unsubscribe(a, acall, "ci/grok"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Ci's destination to end", func() bool { st, _, _ := hookState(t, c, atenant); return st == "" })
	if n := count(t, c, atenant, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.unregistered' AND detail->>'reason' = 'unwatched'`); n != 1 {
		t.Fatalf("%d unwatch entries", n)
	}
	before := ciPosts(f, "Ci")
	emitNote(a, []string{"ci/grok"}, note{Text: "after the unwatch"})
	time.Sleep(time.Second)
	if n := ciPosts(f, "Ci"); n != before {
		t.Fatal("a publish after the unwatch started a run")
	}
	// Cd still wakes the bot.
	if !liveHook(t, c, btenant) {
		t.Fatal("Cd's destination went with Ci's")
	}
	emitNote(b, []string{"cd/grok"}, note{Text: "still waking"})
	waitFor(t, "Cd's message", func() bool { _, s := deliveryOf(t, c, btenant, "still waking"); return s == "acked" })

	// The routine is deleted in Grok: Cursor refuses, Cd's destination dies.
	f.remove("stopHook1")
	time.Sleep(400 * time.Millisecond)
	emitNote(b, []string{"cd/grok"}, note{Text: "to a deleted routine"})
	waitFor(t, "Cd's destination to die", func() bool { st, _, _ := hookState(t, c, btenant); return st == "dead" })
	if _, r, _ := hookState(t, c, btenant); r != "not_found" {
		t.Fatalf("reason %s", r)
	}
}

// The setup page says wake-ups are on only while they are: once the
// routine is deleted and the destination dies, it doesn't.
func TestGrokPageOnlyWhileLive(t *testing.T) {
	c := startGrok(t, nil)
	f := newFakeCursor(t, c)
	app := noteApp(t, c, "ci")
	bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	sub, _, token := grokSetUp(t, c, f, app, bot, "gina", "ci/grok", "liveHook1")
	tenant, _ := topicSubRow(t, c, sub.ID)
	f.remove("liveHook1")
	time.Sleep(400 * time.Millisecond)
	emitNote(app, []string{"ci/grok"}, note{Text: "to a deleted routine"})
	waitFor(t, "the destination to die", func() bool { st, _, _ := hookState(t, c, tenant); return st == "dead" })
	if code, out := describe(t, c, token); code == 200 && out["state"] == "on" {
		t.Fatal("the page says wake-ups are on for a dead webhook")
	}
}

// A join racing a re-setup copies nothing stale: one that read the bot's
// destination, or the routine's binding, before the re-setup's bind took
// the routine joins nothing, and the bot gets the setup path.
func TestGrokJoinRaces(t *testing.T) {
	// A join that read the bot's webhook just before a re-setup rewrote it
	// (here: the index already holds the re-setup's new removal hash while
	// the destination the join read still has the old) joins nothing: it
	// would copy the old webhook key and an older binding. The bot gets
	// the setup path instead.
	t.Run("a join racing a re-setup", func(t *testing.T) {
		c := startGrok(t, nil)
		f := newFakeCursor(t, c)
		bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
		a := noteApp(t, c, "ci")
		grokSetUp(t, c, f, a, bot, "gina", "ci/grok", "rmHookRace1")
		if _, err := c.DB.Pool.Exec(ctx, `UPDATE bot_webhooks SET removal_hash = sha256('a newer setup')`); err != nil {
			t.Fatal(err)
		}
		b := noteApp(t, c, "cd")
		sub, err := subscribe(b, bot.call("gina"), "cd/grok", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if out := bot.keyStep(t, sub.Addition.Text); !strings.Contains(out, "Now set up the webhook") {
			t.Fatalf("a join on a superseded read: %s", out)
		}
		btenant, _ := topicSubRow(t, c, sub.ID)
		if st, _, _ := hookState(t, c, btenant); st != "" {
			t.Fatal("joined with an older binding")
		}
	})

	// A join that read the routine's binding just before a re-setup took
	// the routine joins nothing (here: the routine already holds a newer
	// binding than the destination the join read).
	t.Run("a join racing a re-setup of the routine", func(t *testing.T) {
		c := startGrok(t, nil)
		f := newFakeCursor(t, c)
		bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
		grokSetUp(t, c, f, noteApp(t, c, "ci"), bot, "gina", "ci/grok", "rmHookRace2")
		if _, err := c.DB.Pool.Exec(ctx, `UPDATE bot_routines SET removal_hash = sha256('a newer setup')`); err != nil {
			t.Fatal(err)
		}
		sub, err := subscribe(noteApp(t, c, "cd"), bot.call("gina"), "cd/grok", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if out := bot.keyStep(t, sub.Addition.Text); !strings.Contains(out, "Now set up the webhook") {
			t.Fatalf("a join on a superseded routine: %s", out)
		}
	})
}

// routineState is the index's view of the bot's routine: how many
// destinations, distinct removal hashes and keys, and how many hold the
// routine's own binding.
func routineState(t *testing.T, c *coretest.Core) (dests, hashes, keys, current int) {
	t.Helper()
	err := c.DB.Pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT w.removal_hash), count(DISTINCT w.fingerprint),
		count(*) FILTER (WHERE r.removal_hash = w.removal_hash AND r.fingerprint = w.fingerprint)
		FROM bot_webhooks w LEFT JOIN bot_routines r ON r.routine = w.routine`).Scan(&dests, &hashes, &keys, &current)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// One routine, one binding: a re-setup after the bot's key is lost,
// first reported by an app whose session never registered the old key,
// carries the new key and webhook to every destination on the routine,
// in every app.
func TestGrokRoutineOneBinding(t *testing.T) {
	t.Run("section 8 step 5", func(t *testing.T) {
		c := startGrok(t, nil)
		f := newFakeCursor(t, c)
		vm := newGrokVM(t)
		bot := &grokBot{c: c, vm: vm, id: grokBotID}
		a := noteApp(t, c, "ci")
		asub, _, _ := grokSetUp(t, c, f, a, bot, "gina", "ci/grok", "routineHook1")
		atenant, _ := topicSubRow(t, c, asub.ID)
		url, key := f.webhook("routineHook1", bot)
		// Step 4: a second app joins.
		b := noteApp(t, c, "cd")
		bsub, _ := subscribe(b, bot.call("gina"), "cd/grok", subOpts{})
		if out := bot.keyStep(t, bsub.Addition.Text); strings.TrimSpace(out) != harnesses.GrokJoinedNote("Cd") {
			t.Fatalf("join: %s", out)
		}
		btenant, _ := topicSubRow(t, c, bsub.ID)
		// Step 5: the key file goes (a VM reset); a third app's key step
		// makes a new key (its init) and proves it there, and the user sets
		// the webhook up again through that app's link.
		bot.loseKey(t)
		e := noteApp(t, c, "ef")
		ecall := bot.call("gina")
		esub, err := subscribe(e, ecall, "ef/grok", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		ecall.Session = esub.Session
		etenant, _ := topicSubRow(t, c, esub.ID)
		token := setupToken(t, bot.keyStep(t, esub.Addition.Text))
		if code, out := save(t, c, token, url, key); code != 200 {
			t.Fatalf("save: %d %v", code, out)
		}
		waitOn(t, c, token)
		waitFor(t, "one binding on the routine", func() bool {
			d, h, k, cur := routineState(t, c)
			return d == 3 && h == 1 && k == 1 && cur == 3
		})
		for _, tn := range []string{atenant, btenant, etenant} {
			if !liveHook(t, c, tn) {
				t.Fatalf("workspace %s isn't live", tn)
			}
		}
		// The first app's messages still wake the bot, through the routine.
		emitNote(a, []string{"ci/grok"}, note{Text: "after the re-setup"})
		waitFor(t, "the first app's message", func() bool { _, s := deliveryOf(t, c, atenant, "after the re-setup"); return s == "acked" })
		// The bind's own app's binding is the routine's, in every app.
		var be, ba []byte
		c.DB.Pool.QueryRow(ctx, `SELECT removal_hash FROM bot_webhooks WHERE tenant_id = $1`, etenant).Scan(&be)
		c.DB.Pool.QueryRow(ctx, `SELECT removal_hash FROM bot_webhooks WHERE tenant_id = $1`, atenant).Scan(&ba)
		if !bytes.Equal(be, ba) {
			t.Fatal("the first app isn't on the re-setup's binding")
		}
	})

	// Two re-setups of the routine at once, from two apps: they take turns
	// (one check per bot), the last bind is the routine's binding
	// everywhere, and both pages say wake-ups are on.
	t.Run("two re-setups at once", func(t *testing.T) {
		c := startGrok(t, nil)
		f := newFakeCursor(t, c)
		bot := &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
		a := noteApp(t, c, "ci")
		asub, _, _ := grokSetUp(t, c, f, a, bot, "gina", "ci/grok", "routineHook2")
		atenant, _ := topicSubRow(t, c, asub.ID)
		url, key := f.webhook("routineHook2", bot)
		bot.loseKey(t)
		// App B: a new key (its key step's init), a setup link.
		b := noteApp(t, c, "cd")
		bcall := bot.call("gina")
		bsub, _ := subscribe(b, bcall, "cd/grok", subOpts{})
		bcall.Session = bsub.Session
		btenant, _ := topicSubRow(t, c, bsub.ID)
		tb := setupToken(t, bot.keyStep(t, bsub.Addition.Text))
		// App D: proves the new key, finds nothing live on it, gets a link.
		d := noteApp(t, c, "de")
		dsub, _ := subscribe(d, bot.call("gina"), "de/grok", subOpts{})
		dtenant, _ := topicSubRow(t, c, dsub.ID)
		td := setupToken(t, bot.keyStep(t, dsub.Addition.Text))
		// Both pasted at once: one gets the check, the other waits its turn
		// (one check at a time per bot), and goes once the first is done.
		done := make(chan [2]any, 2)
		for _, tok := range []string{tb, td} {
			go func() { code, _ := save(t, c, tok, url, key); done <- [2]any{tok, code} }()
		}
		codes, waiting := map[int]int{}, ""
		for range 2 {
			r := <-done
			codes[r[1].(int)]++
			if r[1].(int) == 409 {
				waiting = r[0].(string)
			}
		}
		if codes[200] != 1 || codes[409] != 1 {
			t.Fatalf("two saves at once: %v", codes)
		}
		first := tb
		if waiting == tb {
			first = td
		}
		waitOn(t, c, first)
		if code, out := save(t, c, waiting, url, key); code != 200 {
			t.Fatalf("the second save, after the first: %d %v", code, out)
		}
		waitOn(t, c, waiting)
		f.ran()
		waitFor(t, "one binding on the routine", func() bool {
			n, h, k, cur := routineState(t, c)
			return n == 3 && h == 1 && k == 1 && cur == 3
		})
		// Both pages say wake-ups are on: they are, whichever bind won.
		for _, tok := range []string{tb, td} {
			if code, out := describe(t, c, tok); code != 200 || out["state"] != "on" {
				t.Fatalf("a setup's page: %d %v", code, out)
			}
		}
		for _, tn := range []string{atenant, btenant, dtenant} {
			if !liveHook(t, c, tn) {
				t.Fatalf("workspace %s isn't live", tn)
			}
		}
	})
}

// stepFive runs section 8's steps 1 to 5: app A sets up (oldBinding is
// its bind's), app B joins, the key file goes, app E's first proof reports
// it and the user sets up again there. It returns the workspaces, the old
// binding and the routine's URL.
type stepFive struct {
	f                         *fakeCursor
	bot                       *grokBot
	e                         *wakeshift.Client
	ecall                     caller
	atenant, btenant, etenant string
	url                       string
	oldBinding                []byte
}

func runStepFive(t *testing.T, c *coretest.Core, hookID string) stepFive {
	t.Helper()
	var r stepFive
	r.f = newFakeCursor(t, c)
	r.bot = &grokBot{c: c, vm: newGrokVM(t), id: grokBotID}
	a := noteApp(t, c, "ci")
	asub, _, _ := grokSetUp(t, c, r.f, a, r.bot, "gina", "ci/grok", hookID)
	r.atenant, _ = topicSubRow(t, c, asub.ID)
	if err := c.DB.Pool.QueryRow(ctx, `SELECT removal_hash FROM bot_webhooks WHERE tenant_id = $1`, r.atenant).Scan(&r.oldBinding); err != nil {
		t.Fatal(err)
	}
	url, key := r.f.webhook(hookID, r.bot)
	r.url = url
	bsub, _ := subscribe(noteApp(t, c, "cd"), r.bot.call("gina"), "cd/grok", subOpts{})
	if out := r.bot.keyStep(t, bsub.Addition.Text); strings.TrimSpace(out) != harnesses.GrokJoinedNote("Cd") {
		t.Fatalf("join: %s", out)
	}
	r.btenant, _ = topicSubRow(t, c, bsub.ID)
	r.bot.loseKey(t)
	r.e = noteApp(t, c, "ef")
	r.ecall = r.bot.call("gina")
	esub, err := subscribe(r.e, r.ecall, "ef/grok", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	r.ecall.Session = esub.Session
	r.etenant, _ = topicSubRow(t, c, esub.ID)
	token := setupToken(t, r.bot.keyStep(t, esub.Addition.Text))
	if code, out := save(t, c, token, url, key); code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	waitOn(t, c, token)
	return r
}

// oneBinding waits for every destination on the routine to hold its one
// binding.
func oneBinding(t *testing.T, c *coretest.Core, dests int) {
	t.Helper()
	waitFor(t, "one binding on the routine", func() bool {
		n, h, k, cur := routineState(t, c)
		return n == dests && h == 1 && k == 1 && cur == dests
	})
}

// A carry-over interrupted after the bind committed (a core that stopped
// before reaching the first app) is finished by the expiry pass, once.
func TestGrokCarryOverSweep(t *testing.T) {
	c := startGrok(t, nil)
	var old struct{ hash, fp, pub []byte }
	r := runStepFive(t, c, "sweepHook1")
	oneBinding(t, c, 3)
	// Put app A back as the bind found it: its old key and binding.
	old.hash = r.oldBinding
	k := newTestKey()
	old.pub, old.fp = k.pub, botkeys.Fingerprint(k.pub)
	if err := c.DB.InTenant(ctx, r.atenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE agent_webhooks SET removal_hash = $1, key_fingerprint = $2`, old.hash, old.fp); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE bot_keys SET public_key = $1, fingerprint = $2`, []byte(old.pub), old.fp); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE bot_webhooks SET removal_hash = $1, fingerprint = $2 WHERE tenant_id = current_tenant()`, old.hash, old.fp)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, h, _, cur := routineState(t, c); h != 2 || cur != 2 {
		t.Fatalf("not interrupted: %d hashes, %d current", h, cur)
	}
	audits := func() int {
		return count(t, c, r.atenant, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.registered'`)
	}
	before := audits()

	if err := c.Core.ExpireNow(ctx); err != nil {
		t.Fatal(err)
	}
	if n, h, k, cur := routineState(t, c); n != 3 || h != 1 || k != 1 || cur != 3 {
		t.Fatalf("after the pass: %d destinations, %d hashes, %d keys, %d current", n, h, k, cur)
	}
	if audits() != before+1 || count(t, c, r.atenant, `SELECT count(*) FROM bot_keys WHERE fingerprint = $1`, old.fp) != 0 {
		t.Fatal("the carry-over wasn't audited in its workspace, or the old key stayed")
	}
	// Idempotent: another pass changes nothing.
	if err := c.Core.ExpireNow(ctx); err != nil {
		t.Fatal(err)
	}
	if audits() != before+1 {
		t.Fatal("a second pass carried over again")
	}
}

// Expiry passes running all along, on two cores, while the routine is set
// up again twice: the last bind is the routine's everywhere, each
// carry-over is recorded once per workspace.
func TestGrokCarryOverSweepRacesResetup(t *testing.T) {
	c := startGrok(t, nil)
	c2 := startGrok(t, func(o *core.Options) { o.DB = c.DB })
	stop := make(chan struct{})
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			core := c.Core
			if i%2 == 1 {
				core = c2.Core
			}
			core.ExpireNow(ctx)
		}
	}()
	r := runStepFive(t, c, "sweepHook2")
	oneBinding(t, c, 3)
	// The webhook's key rotates; the routine dies; set up again through E.
	newKey := r.f.rotate("sweepHook2")
	emitNote(r.e, []string{"ef/grok"}, note{Text: "after the rotation"})
	waitFor(t, "E's destination to die", func() bool { st, _, _ := hookState(t, c, r.etenant); return st == "dead" })
	dead, _ := subscribe(r.e, r.ecall, "ef/grok", subOpts{})
	token3 := setupToken(t, dead.Addition.Text)
	if code, out := save(t, c, token3, r.url, newKey); code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	waitOn(t, c, token3)
	oneBinding(t, c, 3)
	close(stop)
	<-swept
	var cur, last []byte
	c.DB.Pool.QueryRow(ctx, `SELECT removal_hash FROM bot_webhooks WHERE tenant_id = $1`, r.etenant).Scan(&last)
	if err := c.DB.Pool.QueryRow(ctx, `SELECT removal_hash FROM bot_routines`).Scan(&cur); err != nil || !bytes.Equal(cur, last) {
		t.Fatalf("the routine's binding isn't the last bind's (%v)", err)
	}
	// A: its setup, then one carry-over per re-setup. B: its join, then
	// one per re-setup.
	for _, tn := range []string{r.atenant, r.btenant} {
		if n := count(t, c, tn, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.registered'`); n != 3 {
			t.Fatalf("workspace %s: %d registrations recorded, want 3", tn, n)
		}
	}
}

// An expiry pass that reaches a destination while the bind's own
// carry-over holds it waits, then finds it done: it records nothing twice.
func TestGrokCarryOverSweepWaitsForCarryOver(t *testing.T) {
	c := startGrok(t, nil)
	r := runStepFive(t, c, "sweepHook3")
	oneBinding(t, c, 3)
	// A's current (carried-over) binding, kept to replay the carry-over.
	var hash, fp, pub []byte
	if err := c.DB.InTenant(ctx, r.atenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT removal_hash, key_fingerprint FROM agent_webhooks`).Scan(&hash, &fp); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT public_key FROM bot_keys`).Scan(&pub)
	}); err != nil {
		t.Fatal(err)
	}
	set := func(tx pgx.Tx, h, f, p []byte) error {
		if _, err := tx.Exec(ctx, `UPDATE agent_webhooks SET removal_hash = $1, key_fingerprint = $2`, h, f); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE bot_keys SET public_key = $1, fingerprint = $2`, p, f); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE bot_webhooks SET removal_hash = $1, fingerprint = $2 WHERE tenant_id = current_tenant()`, h, f)
		return err
	}
	// Back to before the carry-over.
	k := newTestKey()
	if err := c.DB.InTenant(ctx, r.atenant, func(tx pgx.Tx) error {
		return set(tx, r.oldBinding, botkeys.Fingerprint(k.pub), []byte(k.pub))
	}); err != nil {
		t.Fatal(err)
	}
	audits := func() int {
		return count(t, c, r.atenant, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.registered'`)
	}
	before := audits()
	// The carry-over, in progress: A's index row locked while it writes.
	done := make(chan error, 1)
	if err := c.DB.InTenant(ctx, r.atenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM bot_webhooks WHERE tenant_id = current_tenant() FOR UPDATE`); err != nil {
			return err
		}
		go func() { done <- c.Core.ExpireNow(ctx) }()
		time.Sleep(500 * time.Millisecond) // the pass reaches A and waits
		return set(tx, hash, fp, pub)
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if audits() != before {
		t.Fatal("the pass carried over what the carry-over had just done")
	}
	if n, h, k, cur := routineState(t, c); n != 3 || h != 1 || k != 1 || cur != 3 {
		t.Fatalf("after: %d destinations, %d hashes, %d keys, %d current", n, h, k, cur)
	}
}
