package coretest_test

import (
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
)

// Issue #1106: the pi conversation that installed Wakeshift's extension
// subscribed before the extension was there, so its watch named no session
// ID (no beacon came with it: nothing to late-redeem), and after /reload it
// called a tool that subscribes to nothing. The extension now passes the
// session's key (from that watch's result), its ID and harness "pi" on the
// app's tools; the session learns its ID from that call, as a subscribe
// teaches it, so the result's beacon line names the session and the
// extension connects it from there. In an app's user's workspace (Webhooks:
// the webhook is the user), as the app's tools run.
func TestPiInstallingSessionConnectsFromAnotherTool(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const sid = "019a5c3e-7d2f-7c11-9a41-5e0f3b2d8c6a"
	pi := caller{User: "hook-1", Client: "pi", ClientVersion: "1.5.0", UserAgent: "undici", Meta: map[string]any{"progressToken": 1}, Tool: "watch"}

	// Before the extension: the offer, with no beacon.
	first, err := subscribe(app, pi, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if beacon.InText(first.Addition.Text) != "" || first.Session == "" {
		t.Fatalf("first: %+v", first)
	}

	// After /reload, a tool that subscribes to nothing, with the key alone
	// (what the agent passes at most): no session ID, so no beacon.
	read := pi
	read.Tool, read.Session = "recent_posts", first.Session
	if got, err := annotate(app, read); err != nil || beacon.InText(got.Text) != "" {
		t.Fatalf("key alone: %+v %v", got, err)
	}
	// Its ID alone names no session of this workspace: nothing.
	byID := pi
	byID.Tool, byID.Conversation, byID.Harness = "recent_posts", sid, "pi"
	if got, err := annotate(app, byID); err != nil || beacon.InText(got.Text) != "" {
		t.Fatalf("ID alone: %+v %v", got, err)
	}

	// What the extension passes: the key, the ID and the harness. Before
	// the fix the session never learned the ID from such a call, so this
	// result had no beacon either, and nothing connected.
	both := read
	both.Conversation, both.Harness = sid, "pi"
	got, err := annotate(app, both)
	if err != nil {
		t.Fatal(err)
	}
	tok := beacon.Trailer(got.Text)
	if tok == "" {
		t.Fatalf("no beacon ending the result: %q", got.Text)
	}
	claims, err := beacon.Verify(tok, sid, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil || claims.Tool != "recent_posts" {
		t.Fatalf("beacon for the session: %+v %v", claims, err)
	}
	if _, err := beacon.Verify(tok, "019a5c3e-0000-7000-8000-00000000beef", []string{c.URL}, coreKeys(t, c), time.Now()); err == nil {
		t.Fatal("the beacon verified for another session")
	}
	// Redeemed for another conversation: refused, and used up.
	if code, _ := redeemTicket(t, claims.Redeem, claims.Ticket, "019a5c3e-0000-7000-8000-00000000beef"); code == 200 {
		t.Fatal("redeemed for another conversation")
	}
	if code, _ := redeemTicket(t, claims.Redeem, claims.Ticket, sid); code == 200 {
		t.Fatal("a ticket offered for the wrong conversation redeemed after")
	}
	// A fresh one (another tool's result) redeems once, for the session.
	both.Tool = "webhook_info"
	got, err = annotate(app, both)
	if err != nil {
		t.Fatal(err)
	}
	claims, err = beacon.Verify(beacon.Trailer(got.Text), sid, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatalf("second beacon: %q %v", got.Text, err)
	}
	if code, out := redeemTicket(t, claims.Redeem, claims.Ticket, sid); code != 200 || out["url"] == "" {
		t.Fatalf("redeem: %d %v", code, out)
	}
	if code, _ := redeemTicket(t, claims.Redeem, claims.Ticket, sid); code == 200 {
		t.Fatal("a ticket redeemed twice")
	}
}

// The session learns an ID from a call that passes its key only while it
// has none and no other session of the agent has that ID: another
// conversation's key with this ID, or this key with another conversation's
// ID, never moves an ID or a beacon to the wrong session.
func TestSessionLearnsItsIDOnlyOnce(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const a, b = "019a5c3e-0000-7000-8000-00000000000a", "019a5c3e-0000-7000-8000-00000000000b"
	pi := caller{User: "hook-1", Client: "pi", UserAgent: "undici", Tool: "watch"}
	one, err := subscribe(app, pi, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	two, err := subscribe(app, pi, "ci/acme/web", subOpts{})
	if err != nil || two.Session == one.Session {
		t.Fatalf("second conversation: %+v %v", two, err)
	}
	call := func(session, id string) string {
		t.Helper()
		x := pi
		x.Tool, x.Session, x.Conversation, x.Harness = "recent_posts", session, id, "pi"
		got, err := annotate(app, x)
		if err != nil {
			t.Fatal(err)
		}
		return beacon.Trailer(got.Text)
	}
	if tok := call(one.Session, a); tok == "" {
		t.Fatal("no beacon for the first conversation")
	} else if _, err := beacon.Verify(tok, a, []string{c.URL}, coreKeys(t, c), time.Now()); err != nil {
		t.Fatal(err)
	}
	// The second conversation's key with the first's ID: that ID is the
	// first session's, found by it (the call's key is the agent's own, as
	// before); the second session doesn't take it.
	if tok := call(two.Session, a); tok != "" {
		if _, err := beacon.Verify(tok, a, []string{c.URL}, coreKeys(t, c), time.Now()); err != nil {
			t.Fatalf("beacon for a: %v", err)
		}
	}
	if tok := call(two.Session, b); tok == "" {
		t.Fatal("the second conversation didn't learn its own ID")
	} else if _, err := beacon.Verify(tok, b, []string{c.URL}, coreKeys(t, c), time.Now()); err != nil {
		t.Fatalf("beacon for b: %v", err)
	}
	// The first session keeps its ID: its key with another ID gets a
	// beacon for its own, never for the other.
	if tok := call(one.Session, "019a5c3e-0000-7000-8000-00000000000c"); tok != "" {
		if _, err := beacon.Verify(tok, a, []string{c.URL}, coreKeys(t, c), time.Now()); err != nil {
			t.Fatalf("the first session's beacon moved: %v", err)
		}
	}
}

// opencode's MCP server learns neither the session nor the call, and
// opencode keeps no result's _meta, so its installing conversation connects
// from the next result of the app's tools that names the session by its
// key, which the plugin now passes (nothing in the core changed for it):
// the result of a tool that subscribes to nothing carries a beacon then,
// and none without the key.
func TestOpencodeInstallingSessionGetsABeaconWithItsKey(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	oc := caller{User: "hook-1", Client: "opencode", ClientVersion: "1.19.5", UserAgent: "opencode/1.19.5", Tool: "watch"}
	first, err := subscribe(app, oc, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	read := oc
	read.Tool = "recent_posts"
	if got, err := annotate(app, read); err != nil || len(got.Meta) != 0 {
		t.Fatalf("no key: %+v %v", got, err)
	}
	read.Session = first.Session
	got, err := annotate(app, read)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := got.Meta[beacon.MetaKey].(string)
	if tok == "" {
		t.Fatalf("no beacon with the key: %+v", got)
	}
	claims, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "ses_opencode_1", Unbound: true}, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := lateRedeem(t, claims.Redeem, claims.Ticket, "ses_opencode_1", "", true); code != 200 {
		t.Fatalf("an unexpired ticket, late or not, redeems: %d", code)
	}
	if code, _ := lateRedeem(t, claims.Redeem, claims.Ticket, "ses_opencode_1", "", true); code == 200 {
		t.Fatal("redeemed twice")
	}
}
