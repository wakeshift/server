package coretest_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/streams"
)

// OpenClaw, as its plugin (plugins/openclaw) sees the core. Its own runtime
// calls MCP servers as openclaw-bundle-mcp with no _meta: the beacon is a
// line of text (the plugin's tool-result filter takes it out), bound to
// nothing, and the redeem tells the core the conversation. When OpenClaw
// runs the turn in Codex's app-server, Codex's client makes the call but
// marks the thread as OpenClaw's: it's OpenClaw's all the same, with the
// beacon in _meta. The plugin then reads the conversation's stream as SSE
// and acknowledges with POST /v1/stream/ack.
func TestOpenClaw(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	keys := coreKeys(t, c)

	call := caller{User: "oc", Client: "openclaw-bundle-mcp", UserAgent: "undici"}
	sub, err := subscribe(app, call, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	text := sub.Addition.Text
	if got := sub.Addition.Agent; got.Harness != "openclaw" || got.Via != "client" {
		t.Fatalf("agent %+v", got)
	}
	for _, want := range []string{"Ask the user now whether to install", "Only if they say yes", "`curl -fsSL " + c.URL + "/install/openclaw.sh | WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1 sh`", "In the same question, tell them", "exec tool", `"Wakeshift: connected"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if len(sub.Addition.Meta) != 0 {
		t.Fatalf("_meta for OpenClaw's own runtime, which drops it: %v", sub.Addition.Meta)
	}
	claims, err := beacon.VerifyBound(beacon.InText(text), beacon.Binding{Conversation: "anything", Unbound: true}, []string{c.URL}, keys, time.Now())
	if err != nil || claims.Thread != "" || claims.Call != "" {
		t.Fatalf("an unbound text beacon: %+v %v", claims, err)
	}

	// The plugin redeems it for its conversation (install ID and session key).
	const conv = "openclaw/0123456789abcdef/agent:main:main"
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, conv)
	if code != 200 {
		t.Fatalf("redeem: %d %v", code, out)
	}
	link, _ := out["url"].(string)
	again, _ := subscribe(app, caller{User: "oc", Client: "openclaw-bundle-mcp", Session: sub.Session}, "ci/acme/web", subOpts{})
	// The plugin said it's connected, in the result it connected from: the
	// core doesn't say it again.
	if !strings.Contains(again.Addition.Text, "Wakeshift's plugin for OpenClaw delivers to this conversation") || strings.Contains(again.Addition.Text, "curl") || strings.Contains(again.Addition.Text, "Wakeshift: connected") {
		t.Fatalf("again: %s", again.Addition.Text)
	}
	// Bound now: its beacons name the conversation, and another's redeem is refused.
	if c2, err := beacon.Verify(beacon.InText(again.Addition.Text), conv, []string{c.URL}, keys, time.Now()); err != nil {
		t.Fatalf("bound: %v", err)
	} else if code, _ := redeemTicket(t, c2.Redeem, c2.Ticket, "openclaw/ffffffffffffffff/agent:main:main"); code != 403 {
		t.Fatalf("another install's redeem: %d", code)
	}

	// SSE, acknowledged by POST: what's acked doesn't come back.
	if _, err := emitNote(app, []string{"ci/acme/web"}, note{Text: "deploy finished"}); err != nil {
		t.Fatal(err)
	}
	ev := sseFirst(t, streams.HTTPStreamURL(link, streams.FormatSSE))
	if ev.Event["subject"] != "deploy finished" || ev.DeliveryID == "" {
		t.Fatalf("event %+v", ev)
	}
	b, _ := json.Marshal(map[string]any{"delivery_ids": []string{ev.DeliveryID}})
	req, _ := http.NewRequest("POST", c.URL+"/v1/stream/ack", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+strings.SplitN(link, "link=", 2)[1])
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("ack: %v %v", err, resp)
	}
	resp.Body.Close()
	if _, err := emitNote(app, []string{"ci/acme/web"}, note{Text: "second"}); err != nil {
		t.Fatal(err)
	}
	if ev := sseFirst(t, streams.HTTPStreamURL(link, streams.FormatSSE)); ev.Event["subject"] != "second" {
		t.Fatalf("after the ack: %+v", ev)
	}

	// OpenClaw running the turn in Codex: Codex's client, OpenClaw's thread.
	meta := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "01a155a9-3a11-7830-aa96-233228455669", "thread_source": "openclaw"},
		"callId": "exec-357f2628"}
	viaCodex, err := subscribe(app, caller{User: "oc2", Client: "codex-mcp-client", UserAgent: "codex-mcp-client/0.160.0", Meta: meta}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := viaCodex.Addition.Agent; got.Harness != "openclaw" || got.Via != "meta" {
		t.Fatalf("agent %+v", got)
	}
	tok, _ := viaCodex.Addition.Meta[beacon.MetaKey].(string)
	if !strings.Contains(viaCodex.Addition.Text, "/install/openclaw.sh") || strings.Contains(viaCodex.Addition.Text, "codex.sh") || beacon.InText(viaCodex.Addition.Text) != "" || tok == "" {
		t.Fatalf("via Codex: %+v", viaCodex.Addition)
	}
	cl, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "x", Unbound: true}, []string{c.URL}, keys, time.Now())
	if err != nil || cl.Thread != "" {
		t.Fatalf("via Codex, not bound to Codex's thread: %+v %v", cl, err)
	}
	// Codex showed the model that result before the plugin connected from
	// it, and the plugin's note for the next turn doesn't reach a woken one:
	// the core says it, on the next call, once.
	if code, _ := redeemTicket(t, cl.Redeem, cl.Ticket, "openclaw/0123456789abcdef/agent:main:codex"); code != 200 {
		t.Fatalf("redeem via Codex: %d", code)
	}
	for i, want := range []bool{true, false} {
		next, err := subscribe(app, caller{User: "oc2", Client: "codex-mcp-client", Meta: meta, Session: viaCodex.Session}, "ci/acme/web", subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.HasPrefix(next.Addition.Text, "Wakeshift: connected. Wakeshift's plugin for OpenClaw delivers"); got != want || strings.Count(next.Addition.Text, "Wakeshift: connected") > 1 {
			t.Fatalf("via Codex, call %d after connecting: %s", i+1, next.Addition.Text)
		}
	}
	// The agent there may say it's Codex: the thread's source wins, and the
	// note tells it what to say.
	claimsCodex, err := subscribe(app, caller{User: "oc3", Client: "codex-mcp-client", Meta: meta, Harness: "codex"}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := claimsCodex.Addition.Agent; got.Harness != "openclaw" || got.Claimed != "codex" {
		t.Fatalf("claims codex: agent %+v", got)
	}
	if text := claimsCodex.Addition.Text; !strings.Contains(text, `pass harness="openclaw"`) || !strings.Contains(text, "/install/openclaw.sh") || strings.Contains(text, "codex.sh") {
		t.Fatalf("claims codex: %s", text)
	}
	if strings.Contains(viaCodex.Addition.Text, `pass harness=`) {
		t.Fatalf("no claim, nothing to correct: %s", viaCodex.Addition.Text)
	}
	// Codex's own threads are still Codex's.
	meta["x-codex-turn-metadata"].(map[string]any)["thread_source"] = "user"
	codex, _ := subscribe(app, caller{User: "cx", Client: "codex-mcp-client", Meta: meta}, "ci/acme/web", subOpts{})
	if got := codex.Addition.Agent; got.Harness != "codex" || !strings.Contains(codex.Addition.Text, "/install/codex.sh") {
		t.Fatalf("codex: %+v", codex.Addition)
	}
}

// redeemFor redeems a beacon's ticket for thread, handing back the link the
// hook held for it before (supersedes; "" for none).
func redeemFor(t *testing.T, cl beacon.Claims, thread, supersedes string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"ticket": cl.Ticket, "thread": thread, "supersedes": supersedes})
	resp, err := http.Post(cl.Redeem, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ URL string }
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.URL
}

// After a restart OpenClaw runs the session's turns in a new Codex thread,
// and a call that doesn't pass its session key starts a new Wakeshift
// session. When the plugin connects the same OpenClaw conversation to it,
// handing back the link it held, the two become one (in a user's
// workspace the earlier merges into the new one; in conversation
// workspaces the new one folds into the earlier), and the link handed back ends: one OpenClaw
// conversation stays one subscription, and nothing waits on a session no
// one reads. A link for another conversation, or one Wakeshift didn't
// sign, ends nothing.
func TestOpenClawSupersedes(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	keys := coreKeys(t, c)
	const topic = "ci/acme/web"
	const conv = "openclaw/0123456789abcdef/agent:main:main"
	// A call from OpenClaw running the turn in Codex thread n, and its beacon.
	viaCodex := func(user string, n int) beacon.Claims {
		t.Helper()
		meta := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": fmt.Sprintf("01a155a9-0000-7000-8000-%012d", n), "thread_source": "openclaw"}}
		sub, err := subscribe(app, caller{User: user, Client: "codex-mcp-client", Meta: meta}, topic, subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
		cl, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: conv, Unbound: true}, []string{c.URL}, keys, time.Now())
		if err != nil {
			t.Fatalf("beacon: %v", err)
		}
		return cl
	}
	subscriptions := func() int {
		t.Helper()
		s, err := noteStatus(app, topic)
		if err != nil {
			t.Fatal(err)
		}
		return s.Subscriptions
	}

	for i, user := range []string{"", "oc"} { // a conversation's own workspace, and a user's
		before := subscriptions()
		code, first := redeemFor(t, viaCodex(user, 10*i), conv, "")
		if code != 200 || first == "" {
			t.Fatalf("%q: first redeem: %d", user, code)
		}
		// The gateway restarted: a new Codex thread, and no session key.
		cl := viaCodex(user, 10*i+1)
		if got := subscriptions(); got != before+2 {
			t.Fatalf("%q: two sessions for now: %d (before %d)", user, got, before)
		}
		code, second := redeemFor(t, cl, conv, first)
		if code != 200 || second == "" || second == first {
			t.Fatalf("%q: second redeem: %d", user, code)
		}
		if got := subscriptions(); got != before+1 {
			t.Fatalf("%q: the earlier session's subscription didn't end: %d (before %d)", user, got, before)
		}
		// The earlier link is revoked; the new one delivers.
		resp, err := http.Get(streams.HTTPStreamURL(first, streams.FormatSSE))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatalf("%q: the superseded link still opens a stream", user)
		}
		if _, err := emitNote(app, []string{topic}, note{Text: "after " + user}); err != nil {
			t.Fatal(err)
		}
		if ev := sseFirst(t, streams.HTTPStreamURL(second, streams.FormatSSE)); ev.Event["subject"] != "after "+user {
			t.Fatalf("%q: event %+v", user, ev)
		}
	}

	// A refused redeem ends nothing: the earlier session keeps its
	// subscription and its link (here the app's grant expired in between).
	{
		before := subscriptions()
		code, first := redeemFor(t, viaCodex("oc2", 50), conv, "")
		if code != 200 {
			t.Fatalf("refused case, first redeem: %d", code)
		}
		cl := viaCodex("oc2", 51)
		tenant := strings.SplitN(cl.Ticket, "_", 3)[1]
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE grants SET expires_at = now() - interval '1 second'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if code, _ := redeemFor(t, cl, conv, first); code != 403 {
			t.Fatalf("redeem with the grant gone: %d", code)
		}
		if got := subscriptions(); got != before+2 {
			t.Fatalf("a refused redeem ended the earlier session: %d subscriptions (before %d)", got, before)
		}
		var revoked bool
		if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_links WHERE revoked_at IS NOT NULL)`).Scan(&revoked)
		}); err != nil || revoked {
			t.Fatalf("a refused redeem revoked a link: %v %v", revoked, err)
		}
	}

	// What ends nothing: a link for another conversation, and one Wakeshift
	// didn't sign (the redeem works all the same).
	const other = "openclaw/0123456789abcdef/agent:main:other"
	code, otherLink := redeemFor(t, viaCodex("", 100), other, "")
	if code != 200 {
		t.Fatalf("other: %d", code)
	}
	before := subscriptions()
	if code, _ = redeemFor(t, viaCodex("", 101), conv+":2", otherLink); code != 200 || subscriptions() != before+1 {
		t.Fatalf("another conversation's link: %d, %d subscriptions (before %d)", code, subscriptions(), before)
	}
	forged := strings.Replace(otherLink, "link=", "link=x", 1)
	if code, _ = redeemFor(t, viaCodex("", 102), other, forged); code != 200 || subscriptions() != before+2 {
		t.Fatalf("a link Wakeshift didn't sign: %d", code)
	}
	// Another app's conversation, even for the same OpenClaw conversation,
	// can't end this app's session with its link.
	app2 := noteApp(t, c, "other")
	meta := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "01a155a9-0000-7000-8000-999999999999", "thread_source": "openclaw"}}
	sub2, err := subscribe(app2, caller{Client: "codex-mcp-client", Meta: meta}, "x/y", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := sub2.Addition.Meta[beacon.MetaKey].(string)
	cl2, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: other, Unbound: true}, []string{c.URL}, keys, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code, _ = redeemFor(t, cl2, other, otherLink); code != 200 || subscriptions() != before+2 {
		t.Fatalf("another app's redeem ended this app's session: %d, %d subscriptions (before %d)", code, subscriptions(), before+2)
	}
}

// sseFrame is a stream frame as SSE carries it.
type sseFrame struct {
	DeliveryID string         `json:"delivery_id"`
	Event      map[string]any `json:"event"`
}

// sseFirst opens a stream as SSE and returns its first event frame.
func sseFirst(t *testing.T, u string) sseFrame {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("SSE: %v %v", err, resp)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var f sseFrame
			if json.Unmarshal([]byte(d), &f) == nil && f.DeliveryID != "" {
				return f
			}
		}
	}
	t.Fatal("SSE ended without an event")
	return sseFrame{}
}

// OpenClaw running its turns in Codex, with Wakeshift's plugin installed
// mid-turn (issue #1125): Codex hooks the plugin into the thread only from
// its next turn, so the beacons of the install turn's results go by, and
// agents leave session_key off the app's tools that subscribe to nothing.
// In a user's workspace, such a call from the Codex thread the
// conversation subscribed from still finds its session, so its result
// carries a beacon the plugin connects from. Another thread, a session the
// plugin connected, a conversation's own workspace (no user) and Codex's
// own threads get none.
func TestOpenClawInCodexConnectsFromTheThreadsNextCall(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	keys := coreKeys(t, c)
	viaCodex := func(thread, source string) map[string]any {
		return map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread, "thread_source": source}}
	}
	const thread = "01a155a9-3a11-7830-aa96-0000000c1125"
	call := caller{User: "oc", Client: "codex-mcp-client", UserAgent: "codex-mcp-client/0.164.0", Meta: viaCodex(thread, "openclaw")}
	watch := call
	watch.Tool = "watch"
	sub, err := subscribe(app, watch, "ci/acme/web", subOpts{})
	if err != nil || sub.Addition.Meta[beacon.MetaKey] == nil {
		t.Fatalf("watch: %+v %v", sub.Addition, err)
	}
	beaconOf := func(what string, cl caller) string {
		t.Helper()
		cl.Tool = "recent_posts"
		a, err := annotate(app, cl)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		tok, _ := a.Meta[beacon.MetaKey].(string)
		return tok
	}
	tok := beaconOf("the thread's next call, no session_key", call)
	if tok == "" {
		t.Fatal("the thread's next call, with no session_key, carries no beacon")
	}
	cl, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "x", Unbound: true}, []string{c.URL}, keys, time.Now())
	if err != nil || cl.Thread != "" {
		t.Fatalf("not bound to Codex's thread: %+v %v", cl, err)
	}
	other := call
	other.Meta = viaCodex("01a155a9-3a11-7830-aa96-0000000c2222", "openclaw")
	if beaconOf("another thread", other) != "" {
		t.Fatal("a call from another thread got a beacon")
	}
	codex := call
	codex.Meta = viaCodex(thread, "user")
	if beaconOf("Codex's own thread", codex) != "" {
		t.Fatal("Codex's own thread, with no session_key, got a beacon")
	}
	// Redeemed by the plugin for its conversation, the session needs none.
	if code, _ := redeemTicket(t, cl.Redeem, cl.Ticket, "openclaw/0123456789abcdef/agent:main:codex"); code != 200 {
		t.Fatalf("redeem: %d", code)
	}
	if beaconOf("after the plugin connected it", call) != "" {
		t.Fatal("a connected session's thread got a beacon")
	}

	// A conversation's own workspace (no user): only its key names it.
	anon := caller{Client: "codex-mcp-client", Meta: viaCodex("01a155a9-3a11-7830-aa96-0000000c3333", "openclaw"), Tool: "watch"}
	if _, err := subscribe(app, anon, "ci/acme/web", subOpts{}); err != nil {
		t.Fatal(err)
	}
	if beaconOf("no user", anon) != "" {
		t.Fatal("a call without a user or a key got a beacon")
	}
}
