package coretest_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
)

// redeemWithLink is redeemBeaconHeld, also returning the link the redeem
// handed out ("" if refused).
func redeemWithLink(t *testing.T, c *coretest.Core, s noteSub, call, conv, held string) (int, string) {
	t.Helper()
	tok, _ := s.Addition.Meta[beacon.MetaKey].(string)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("no beacon in %v", s.Addition.Meta)
	}
	var claims beacon.Claims
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(b, &claims)
	in := map[string]any{"ticket": claims.Ticket, "thread": conv, "call": call}
	if held != "" {
		in["held"] = held
	}
	body, _ := json.Marshal(in)
	resp, err := http.Post(c.URL+"/v1/connector/redeem", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		URL   string `json:"url"`
		Error struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode == http.StatusConflict && out.Error.Reason != "link_not_held" {
		t.Errorf("a refused redeem's reason: %q", out.Error.Reason)
	}
	return resp.StatusCode, out.URL
}

// A ticket for a sibling session of the same user and agent (a new session,
// with no conversation yet), redeemed naming a conversation that another
// session holds through its hooks' link, must show that link, as a redeem
// of that session's own ticket must (#1152): before, the redeem merged the
// sibling into the held session and minted a link for it, so anyone who
// had the app mint a beacon naming the conversation took its stream over.
// Hooks that show the link still connect it.
func TestSiblingTicketNeedsTheHeldLink(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const conv = "0a1f1c2e-5b53-4d0e-9a71-0c3e2f6d8b10"
	sub := func(call string) noteSub {
		t.Helper()
		s, err := subscribe(app, caller{User: "bo", Client: "claude-code", Tool: "watch_builds", Meta: map[string]any{"claudecode/toolUseId": call}}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// The conversation's hooks redeem its first beacon, binding its session.
	s1 := sub("toolu_A1")
	st, held := redeemWithLink(t, c, s1, "toolu_A1", conv, "")
	if st != 200 || held == "" {
		t.Fatalf("the conversation's own redeem: %d", st)
	}
	tenant := workspaceOf(t, c, "ci", "bo")
	connectorLinks := func() (n int) {
		t.Helper()
		c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM stream_links WHERE revoked_at IS NULL AND coalesce(connector, '') <> ''`).Scan(&n)
		})
		return n
	}
	// Another conversation's call (no key: a new session) has the app mint
	// a beacon; its ticket names the first conversation, without its link.
	s2 := sub("toolu_B1")
	if s2.Session == s1.Session {
		t.Fatal("one session")
	}
	before := connectorLinks()
	if st, link := redeemWithLink(t, c, s2, "toolu_B1", conv, ""); st != http.StatusConflict || link != "" {
		t.Fatalf("a sibling's ticket without the held link: %d, link handed out: %v", st, link != "")
	}
	if n := connectorLinks(); n != before {
		t.Fatalf("connector links: %d → %d", before, n)
	}
	var merged bool
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE merged_into IS NOT NULL)`).Scan(&merged)
	})
	if merged {
		t.Fatal("a session was merged")
	}

	// The conversation's hooks, which hold its link, redeem another call's
	// beacon (a parallel first call's): connected, one session.
	s3 := sub("toolu_C1")
	if st, link := redeemWithLink(t, c, s3, "toolu_C1", conv, held); st != 200 || link == "" {
		t.Fatalf("the hooks' redeem with their link: %d", st)
	}
	var watching int
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE watching AND conversation = $1`, conv).Scan(&watching)
	})
	if watching != 1 {
		t.Fatalf("%d sessions watch the conversation", watching)
	}
}

// Another user of the same app (Mallory) redeems a beacon for her own
// session naming Ada's conversation, with supersedes = Ada's revoked link
// (from a log, say). The takeover check counts a revoked link as not live,
// so her redeem connects her own session; but Ada's session, in Ada's
// workspace, must not be ended by it: only a conversation's own workspace
// (no user) of the same app is superseded from another workspace.
func TestRevokedSupersedesLeavesAnotherUsersSession(t *testing.T) {
	c := coretest.Start(t)
	chat := newRelayChat(t, c)
	const conv = "27188d7e-37d9-4a60-b001-465a6950b24c"
	ada := chat.agent(t, "ada", "Cursor")
	hook := localHook(t, c, "cursor")
	res := ada.call("watch", map[string]any{"conversation_id": conv})
	runLocalHook(hook, "", cursorPost(t, conv, "watch", res))
	held := hook.Links.Load(conv)
	if held == "" {
		t.Fatalf("not connected: %s", lastText(res))
	}
	adaTenant := userTenant(t, c, "relay", "ada")
	// The link is revoked (rotated, say); the session still watches.
	if err := c.DB.InTenant(ctx, adaTenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_links SET revoked_at = now() WHERE coalesce(connector,'') <> ''`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	state := func() (watching bool, subs int) {
		t.Helper()
		c.DB.InTenant(ctx, adaTenant, func(tx pgx.Tx) error {
			tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE watching AND conversation = $1)`, conv).Scan(&watching)
			return tx.QueryRow(ctx, `SELECT count(*) FROM topic_subscriptions WHERE ended_at IS NULL`).Scan(&subs)
		})
		return
	}
	w0, s0 := state()
	if !w0 || s0 == 0 {
		t.Fatalf("before: watching %v, %d subscriptions", w0, s0)
	}

	mallory := chat.agent(t, "mallory", "Cursor")
	mres := mallory.call("watch", map[string]any{"conversation_id": conv})
	cl, err := beacon.Verify(beacon.Trailer(lastText(mres)), conv, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]string{"ticket": cl.Ticket, "thread": conv, "supersedes": held})
	resp, err := http.Post(cl.Redeem, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if w, s := state(); !w || s != s0 {
		t.Fatalf("Mallory's redeem (%d) ended Ada's session: watching %v, %d of %d subscriptions", resp.StatusCode, w, s, s0)
	}
}

// The race the sibling check must not lose: a first redeem reads that no
// session has the conversation, and before it binds, another session is
// bound to it and connected (its own redeem's link). The redeem decides on
// the session that has the conversation once it holds the agent's lock,
// never on what it read before: it's refused (link_not_held), and nothing
// is merged or linked.
func TestSiblingRedeemRacingABindIsRefused(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const conv = "5b2e8c1d-0f3a-4c7e-9d21-6a8b4e2f1c39"
	sub := func(call string) noteSub {
		t.Helper()
		s, err := subscribe(app, caller{User: "bo", Client: "claude-code", Tool: "watch_builds", Meta: map[string]any{"claudecode/toolUseId": call}}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first, second := sub("toolu_R1"), sub("toolu_R2")
	if first.Session == second.Session {
		t.Fatal("one session")
	}
	tenant := workspaceOf(t, c, "ci", "bo")
	var firstID, agent, grant string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id::text, agent_id FROM sessions ORDER BY created_at LIMIT 1`).Scan(&firstID, &agent); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT id::text FROM grants WHERE app_id = 'ci' AND revoked_at IS NULL ORDER BY created_at DESC LIMIT 1`).Scan(&grant)
	}); err != nil {
		t.Fatal(err)
	}

	// Hold the agent's sessions lock (as a bind does), so the redeem waits
	// for it after reading that nothing has the conversation.
	tx, err := c.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('sessions:' || current_tenant()::text || ':' || $1))`, agent); err != nil {
		t.Fatal(err)
	}
	type result struct {
		code int
		link string
	}
	done := make(chan result, 1)
	go func() {
		code, link := redeemWithLink(t, c, second, "toolu_R2", conv, "")
		done <- result{code, link}
	}()
	waitFor(t, "the redeem to wait for the lock", func() bool {
		var n int
		c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND wait_event = 'advisory'`).Scan(&n)
		return n > 0
	})
	// Meanwhile the first session is bound to the conversation and
	// connected.
	if _, err := tx.Exec(ctx, `UPDATE sessions SET conversation = $2 WHERE id = $1`, firstID, conv); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO stream_links (tenant_id, agent_id, target, ack, created_by, grant_id, expires_at, connector)
		VALUES (current_tenant(), $1, $2, true, 'app:ci', $3, now() + interval '25 minutes', 'wakeshift/dev claude-code')`, agent, firstID, grant); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.code != http.StatusConflict || r.link != "" {
		t.Fatalf("the racing redeem: %d, link handed out: %v", r.code, r.link != "")
	}
	var merged bool
	var links int
	c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sessions WHERE merged_into IS NOT NULL)`).Scan(&merged)
		return tx.QueryRow(ctx, `SELECT count(*) FROM stream_links WHERE revoked_at IS NULL AND coalesce(connector, '') <> ''`).Scan(&links)
	})
	if merged || links != 1 {
		t.Fatalf("merged %v, %d connector links", merged, links)
	}
}

// Redeems that take a session's redeem lock and the agent's sessions lock
// in every combination, at once: sibling tickets without the held link
// (the holder's redeem lock, then the agent's), sibling tickets with it
// (the holder's, then supersede's agent lock), and the holder's own
// tickets with it (redeemProven's, then supersede). Each answers (200,
// or a refusal); none fails on a deadlock (500).
func TestConcurrentRedeemsTakeLocksInOneOrder(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const conv = "8d3f1a2b-7c4e-4f60-a1b2-3c4d5e6f7a8b"
	sub := func(call, session string) noteSub {
		t.Helper()
		s, err := subscribe(app, caller{User: "bo", Client: "claude-code", Tool: "watch_builds", Session: session,
			Meta: map[string]any{"claudecode/toolUseId": call}}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := sub("toolu_L0", "")
	st, held := redeemWithLink(t, c, first, "toolu_L0", conv, "")
	if st != 200 {
		t.Fatalf("the conversation's own redeem: %d", st)
	}
	type redeem struct {
		s          noteSub
		call, held string
	}
	var all []redeem
	for i := range 4 {
		all = append(all,
			redeem{sub(fmt.Sprintf("toolu_S%d", i), ""), fmt.Sprintf("toolu_S%d", i), ""},
			redeem{sub(fmt.Sprintf("toolu_H%d", i), ""), fmt.Sprintf("toolu_H%d", i), held})
		own := sub(fmt.Sprintf("toolu_O%d", i), first.Session)
		if _, ok := own.Addition.Meta[beacon.MetaKey].(string); ok {
			all = append(all, redeem{own, fmt.Sprintf("toolu_O%d", i), held})
		}
	}
	codes := make([]int, len(all))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, r := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes[i], _ = redeemWithLink(t, c, r.s, r.call, conv, r.held)
		}()
	}
	close(start)
	wg.Wait()
	for i, code := range codes {
		if code >= 500 {
			t.Errorf("redeem %d (held %v): %d", i, all[i].held != "", code)
		}
	}
}
