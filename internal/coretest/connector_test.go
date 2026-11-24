package coretest_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/grants"
)

// coreKeys fetches the core's JWKS, as the connector's hook does.
func coreKeys(t *testing.T, c *coretest.Core) func(kid string) ed25519.PublicKey {
	t.Helper()
	var jwks struct {
		Keys []struct{ Kid, X string }
	}
	getJSON(t, c.URL+"/oauth/jwks", &jwks)
	return func(kid string) ed25519.PublicKey {
		for _, k := range jwks.Keys {
			if k.Kid == kid {
				b, _ := base64.RawURLEncoding.DecodeString(k.X)
				return b
			}
		}
		return nil
	}
}

// redeemTicket posts a ticket for a thread, as the connector's hook does.
func redeemTicket(t *testing.T, url, ticket, thread string) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"ticket": ticket, "thread": thread})
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// A Codex thread's watch gets a signed beacon; its ticket is redeemed once,
// only for that thread, for the thread's own stream link, which then holds
// what was queued for it; unwatch closes it.
func TestConnectorBeacon(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")

	const thread = "01a14bd7-bbbc-7b70-a723-0abb9f38118e"
	sess, err := agentkit.ConversationSession(ctx, codex, thread)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Connector == nil || sess.Connector.Running || !strings.Contains(sess.Connector.Note, "/install/codex.sh | sh") || !strings.Contains(sess.Connector.Note, "Only if they say yes") || strings.Contains(sess.Connector.Note, "Connector Terms") {
		t.Fatalf("before any connector: %+v", sess.Connector)
	}
	lineSub(t, c, app, codex, sess.Handle)

	keys := coreKeys(t, c)
	watched, err := threadWatch(t, c, app, codex, thread)
	if err != nil || watched.Beacon == "" {
		t.Fatalf("watch: %+v %v", watched, err)
	}
	if _, err := beacon.Verify(watched.Beacon, thread, []string{"https://other.example"}, keys, time.Now()); err != beacon.ErrUntrusted {
		t.Fatalf("an issuer not pinned: %v", err)
	}
	if _, err := beacon.Verify(watched.Beacon, "another-thread", []string{c.URL}, keys, time.Now()); err != beacon.ErrInvalid {
		t.Fatalf("for another thread: %v", err)
	}
	if _, err := beacon.Verify(watched.Beacon, thread, []string{c.URL}, keys, time.Now().Add(2*time.Minute)); err != beacon.ErrInvalid {
		t.Fatalf("expired: %v", err)
	}
	claims, err := beacon.Verify(watched.Beacon, thread, []string{c.URL}, keys, time.Now())
	if err != nil || claims.App == "" || claims.Redeem != c.URL+"/v1/connector/redeem" {
		t.Fatalf("verify: %+v %v", claims, err)
	}
	// A signature from another key isn't Wakeshift's.
	parts := strings.Split(watched.Beacon, ".")
	if _, err := beacon.Verify(parts[0]+"."+parts[1]+"."+base64.RawURLEncoding.EncodeToString(make([]byte, 64)), thread, []string{c.URL}, keys, time.Now()); err != beacon.ErrUntrusted {
		t.Fatalf("a forged signature: %v", err)
	}

	// The wrong thread uses the ticket up.
	if code, out := redeemTicket(t, claims.Redeem, claims.Ticket, "another-thread"); code != 403 {
		t.Fatalf("wrong thread: %d %v", code, out)
	}
	if code, _ := redeemTicket(t, claims.Redeem, claims.Ticket, thread); code != 404 {
		t.Fatalf("a ticket used up on the wrong thread worked: %d", code)
	}
	// Another tenant's ID in the ticket finds nothing.
	watched, _ = threadWatch(t, c, app, codex, thread)
	claims, _ = beacon.Verify(watched.Beacon, thread, []string{c.URL}, keys, time.Now())
	other := c.SignIn("bob@example.com").Workspace(t)
	if code, _ := redeemTicket(t, claims.Redeem, strings.Replace(claims.Ticket, codex.Workspace, other, 1), thread); code != 404 {
		t.Fatalf("a ticket under another tenant: %d", code)
	}

	// Queued while nothing delivers to the thread (after its last watch,
	// whose result would carry it).
	postLine(t, c, app, codex, "while you were away")
	waitFor(t, "queued", func() bool {
		w, _ := agentkit.InboxOf(ctx, codex, sess.Handle)
		return w.Pending > 0
	})
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
	if code != 200 || out["session"] != sess.Handle {
		t.Fatalf("redeem: %d %v", code, out)
	}
	if code, _ := redeemTicket(t, claims.Redeem, claims.Ticket, thread); code != 404 {
		t.Fatalf("redeemed twice: %d", code)
	}
	ws, _, err := websocket.Dial(ctx, out["url"].(string), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	read := func() (string, error) {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, b, err := ws.Read(rctx)
		var m struct {
			Type  string
			Event struct{ Subject string }
		}
		json.Unmarshal(b, &m)
		return m.Type + ":" + m.Event.Subject, err
	}
	if got, err := read(); err != nil || got != "event:while you were away" {
		t.Fatalf("the queued message on the thread's link: %q %v", got, err)
	}
	postLine(t, c, app, codex, "now")
	if got, err := read(); err != nil || got != "event:now" {
		t.Fatalf("a new message: %q %v", got, err)
	}
	waitFor(t, "connected", func() bool {
		s, _ := agentkit.ConversationSession(ctx, codex, thread)
		return s.Connector != nil && s.Connector.Running && s.Watching
	})

	// Unwatch closes the stream (4001) and stops it watching.
	if err := agentkit.Unwatch(ctx, codex, sess.Handle); err != nil {
		t.Fatal(err)
	}
	if _, err := read(); websocket.CloseStatus(err) != 4001 {
		t.Fatalf("after unwatch: %v", err)
	}
	if s, _ := agentkit.ConversationSession(ctx, codex, thread); s.Watching || s.Connector == nil || s.Connector.Running ||
		!strings.Contains(s.Connector.Note, "starts again by itself") {
		t.Fatalf("after unwatch: %+v %+v", s, s.Connector)
	}
}

// A ticket stops working when it expires, or when the agent's grant goes.
func TestConnectorTicketLimits(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")
	const thread = "thread-1"
	agentkit.ConversationSession(ctx, codex, thread)
	if _, err := threadWatch(t, c, app, codex, thread); err != nil {
		t.Fatal(err)
	}
	keys := coreKeys(t, c)
	ticket := func() beacon.Claims {
		w, err := threadWatch(t, c, app, codex, thread)
		if err != nil {
			t.Fatal(err)
		}
		cl, err := beacon.Verify(w.Beacon, thread, []string{c.URL}, keys, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return cl
	}
	expired := ticket()
	c.DB.InTenant(ctx, codex.Workspace, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE connector_tickets SET expires_at = now() - interval '1 second'`)
		return err
	})
	if code, _ := redeemTicket(t, expired.Redeem, expired.Ticket, thread); code != 404 {
		t.Fatalf("expired: %d", code)
	}
	revoked := ticket()
	c.DB.InTenant(ctx, codex.Workspace, func(tx pgx.Tx) error {
		g, err := grants.Live(ctx, tx, "agentchat", "codex")
		if err != nil {
			return err
		}
		_, err = grants.Revoke(ctx, tx, g.ID, "user:test")
		return err
	})
	if code, out := redeemTicket(t, revoked.Redeem, revoked.Ticket, thread); code != 403 || out["error"].(map[string]any)["code"] != "no_grant" {
		t.Fatalf("after the grant was revoked: %d %v", code, out)
	}
}

// Unwatch sticks: a ticket handed out before it doesn't start the connector
// again, and no new beacons are given out until the thread watches again.
func TestUnwatchOutlivesTickets(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")
	const thread = "thread-1"
	sess, _ := agentkit.ConversationSession(ctx, codex, thread)
	watched, err := threadWatch(t, c, app, codex, thread)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := beacon.Verify(watched.Beacon, thread, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := agentkit.Unwatch(ctx, codex, sess.Handle); err != nil {
		t.Fatal(err)
	}
	if code, _ := redeemTicket(t, claims.Redeem, claims.Ticket, thread); code != 404 {
		t.Fatalf("a ticket from before unwatch: %d", code)
	}
	if s, _ := agentkit.ConversationSession(ctx, codex, thread); s.Watching {
		t.Fatal("watching again after unwatch")
	}
	if w, err := threadResult(t, c, app, codex, thread); err != nil || w.Beacon != "" {
		t.Fatalf("a beacon after unwatch: %+v %v", w, err)
	}
}

// A redemption racing the grant's revocation waits for it, so the link it
// would mint can't outlive the grant.
func TestConnectorRedeemWaitsForRevocation(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")
	const thread = "thread-1"
	agentkit.ConversationSession(ctx, codex, thread)
	watched, err := threadWatch(t, c, app, codex, thread)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := beacon.Verify(watched.Beacon, thread, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	revoking, commit, revoked := make(chan struct{}), make(chan struct{}), make(chan error)
	go func() {
		revoked <- c.DB.InTenant(ctx, codex.Workspace, func(tx pgx.Tx) error {
			g, err := grants.Live(ctx, tx, "agentchat", "codex")
			if err == nil {
				_, err = grants.Revoke(ctx, tx, g.ID, "user:test")
			}
			close(revoking)
			<-commit
			return err
		})
	}()
	<-revoking
	type result struct {
		code int
		out  map[string]any
	}
	redeemed := make(chan result, 1)
	go func() {
		code, out := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
		redeemed <- result{code, out}
	}()
	time.Sleep(300 * time.Millisecond) // the redemption reaches the grant
	close(commit)
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	if r := <-redeemed; r.code != 403 {
		t.Fatalf("redeemed while the grant was being revoked: %d %v", r.code, r.out)
	}
}

// A redeem without its thread (or ticket) is a 400 naming the missing
// field, answered before the ticket is looked up: the ticket still
// redeems for its thread afterwards.
func TestConnectorRedeemMissingField(t *testing.T) {
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")
	const thread = "01a14bd7-bbbc-7b70-a723-0abb9f38118f"
	watched, err := threadWatch(t, c, app, codex, thread)
	if err != nil || watched.Beacon == "" {
		t.Fatalf("watch: %+v %v", watched, err)
	}
	claims, err := beacon.Verify(watched.Beacon, thread, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ ticket, thread, missing string }{
		{claims.Ticket, "", "thread"},
		{claims.Ticket, "  ", "thread"},
		{"", thread, "ticket"},
	} {
		code, out := redeemTicket(t, claims.Redeem, f.ticket, f.thread)
		e, _ := out["error"].(map[string]any)
		if msg, _ := e["message"].(string); code != http.StatusBadRequest || e["code"] != "bad_request" || !strings.Contains(msg, f.missing+" is missing") {
			t.Errorf("without %s: %d %v", f.missing, code, out)
		}
	}
	if code, out := redeemTicket(t, claims.Redeem, claims.Ticket, thread); code != http.StatusOK {
		t.Fatalf("the ticket after the refused redeems: %d %v", code, out)
	}
}
