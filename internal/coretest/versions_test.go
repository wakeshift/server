package coretest_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/streams"
	"github.com/wakeshift/server/internal/version"
)

// asVersion makes this binary (the connector's code in this process)
// report v until the test ends.
func asVersion(t *testing.T, v string) {
	was := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = was })
}

// codexBeacon is the claims of the Codex beacon in a tool result.
func codexBeacon(t *testing.T, c *coretest.Core, a addition, thread string) beacon.Claims {
	t.Helper()
	tok, _ := a.Meta[beacon.MetaKey].(string)
	claims, err := beacon.Verify(tok, thread, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatalf("beacon: %v (%+v)", err, a)
	}
	return claims
}

// sessionConnector is what the core recorded for the session a ticket was
// for (its conversation, thread): the latest connector and its newest link's.
func sessionConnector(t *testing.T, c *coretest.Core, ticket, thread string) (session, link string) {
	t.Helper()
	tenant := strings.SplitN(ticket, "_", 3)[1]
	err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(s.connector, ''), coalesce((SELECT l.connector FROM stream_links l WHERE l.target = s.id::text ORDER BY l.created_at DESC LIMIT 1), '')
			FROM sessions s WHERE s.conversation = $1`, thread).Scan(&session, &link)
	})
	if err != nil {
		t.Fatal(err)
	}
	return session, link
}

// Without a minimum (the default), every connector is served, and the
// core records the one it sees: a redeem without the header counts as
// version 0 of the harness's connector, a poll naming itself replaces it,
// on the session and its link.
func TestConnectorVersionsAreRecorded(t *testing.T) {
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	const thread = "019a14bd-bbbc-7b70-a723-0abb9f381111"
	call := caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	claims := codexBeacon(t, c, sub.Addition, thread)
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, thread) // no header: an install from before it
	if code != 200 {
		t.Fatalf("redeem without the header: %d %v", code, out)
	}
	if s, l := sessionConnector(t, c, claims.Ticket, thread); s != "wakeshift/0 codex" || l != s {
		t.Fatalf("recorded %q (link %q), want version 0", s, l)
	}

	asVersion(t, "2026.10.20.1200+abc1234")
	p := &connector.HTTPPoller{Harness: "codex", Issuers: []string{c.URL}}
	link := out["url"].(string)
	if _, err := p.Poll(ctx, link, 0, false); err != nil {
		t.Fatal(err)
	}
	if s, l := sessionConnector(t, c, claims.Ticket, thread); s != "wakeshift/2026.10.20.1200+abc1234 codex" || l != s {
		t.Fatalf("after a poll: %q (link %q)", s, l)
	}
}

// With a minimum, an older connector is refused with 426 update_required
// on redeem and on poll, wait and stream, and given nothing: what arrives
// stays held for the session (no result hands it over). Subscribing asks
// the agent to have the user update, with the install command, once per
// connector seen, and after that says only that the hooks can't deliver;
// other results say nothing but carry a beacon. Once updated, the hooks
// connect from a result's beacon and get what comes
// next; an ack is recorded whatever the version.
func TestOutdatedConnectorsAreAskedToUpdate(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) {
		o.Connectors = connectorversion.Minimums{"wakeshift": "2026.10.20"}
	})
	app := noteApp(t, c, "ci")
	codex, _ := harnesses.Get("codex")
	const thread = "019a14bd-bbbc-7b70-a723-0abb9f382222"
	call := caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session

	// No header (an install from before versions): version 0, refused.
	claims := codexBeacon(t, c, sub.Addition, thread)
	code, out := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
	if e, _ := out["error"].(map[string]any); code != http.StatusUpgradeRequired || e["code"] != "update_required" ||
		!strings.Contains(e["message"].(string), harnesses.Outdated(codex)) || !strings.Contains(e["message"].(string), "/install/codex.sh | sh") {
		t.Fatalf("redeem without the header: %d %v", code, out)
	}

	// An old connector: refused too, with the note its hook shows. Another
	// result says nothing, but carries a beacon.
	asVersion(t, "2026.10.19.2300+old")
	r, err := annotate(app, call)
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "" {
		t.Fatalf("an ordinary result asks for the update: %s", r.Text)
	}
	claims = codexBeacon(t, c, r, thread)
	_, err = connector.Redeem(ctx, nil, "codex", claims, thread)
	var outdated *connector.UpdateRequired
	if !errors.As(err, &outdated) || !strings.HasPrefix(outdated.Message, harnesses.Outdated(codex)) {
		t.Fatalf("an old connector's redeem: %v", err)
	}

	// What arrives meanwhile waits for the session.
	if _, err := emitNote(app, []string{"ci/acme"}, note{Text: "held while outdated"}); err != nil {
		t.Fatal(err)
	}
	heldNotShown(t, c, app, call, sub.ID, "held while outdated")
	// Subscribing: the update note, once for the connector seen; then only
	// that the hooks can't deliver.
	first, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Addition.Text, harnesses.Outdated(codex)) || !strings.Contains(first.Addition.Text, "Only if they agree, run `curl -fsSL "+c.URL+"/install/codex.sh | sh`") ||
		strings.Contains(first.Addition.Text, "held while outdated") || strings.Contains(first.Addition.Text, "call this tool again") {
		t.Fatalf("subscribing doesn't ask for the update: %s", first.Addition.Text)
	}
	again, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(again.Addition.Text, harnesses.Outdated(codex)+" Until they're updated they can't deliver to this conversation.") || strings.Contains(again.Addition.Text, "Only if they agree") {
		t.Fatalf("subscribing again: %s", again.Addition.Text)
	}

	// Updated: the hooks connect from the result's beacon.
	asVersion(t, "2026.10.21.0800+new")
	claims = codexBeacon(t, c, again.Addition, thread)
	link, err := connector.Redeem(ctx, nil, "codex", claims, thread)
	if err != nil {
		t.Fatalf("the updated connector's redeem: %v", err)
	}
	r, _ = annotate(app, call)
	if strings.Contains(r.Text, harnesses.Outdated(codex)) {
		t.Fatalf("still asks for the update: %s", r.Text)
	}
	p := &connector.HTTPPoller{Harness: "codex", Issuers: []string{c.URL}}
	if _, err := emitNote(app, []string{"ci/acme"}, note{Text: "after the update"}); err != nil {
		t.Fatal(err)
	}
	// The updated hooks get what was held while they were refused, and
	// what came after.
	var ids []string
	var got string
	waitFor(t, "the updated hooks' deliveries", func() bool {
		b, err := p.Poll(ctx, link, 2*time.Second, false)
		if err != nil {
			t.Fatalf("the updated hooks' poll: %v", err)
		}
		for _, d := range b.Deliveries {
			ids = append(ids, d.ID)
			got += string(mustJSON(d.Event))
		}
		return strings.Contains(got, "held while outdated") && strings.Contains(got, "after the update")
	})

	// The old version again on the same link (a stale process): refused on
	// poll, wait and stream, and what comes stays for the updated one. Its
	// ack is still recorded.
	asVersion(t, "2026.10.19.2300+old")
	if err := p.Ack(ctx, link, ids); err != nil {
		t.Fatalf("an old connector's ack: %v", err)
	}
	if _, err := emitNote(app, []string{"ci/acme"}, note{Text: "held for the update"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Poll(ctx, link, time.Second, false); !errors.As(err, &outdated) {
		t.Fatalf("an old connector's poll: %v", err)
	}
	tok := linkToken(link)
	for _, path := range []string{"/v1/stream/wait?timeout=1s", "/v1/stream?format=sse"} {
		req, _ := http.NewRequest("GET", c.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set(connectorversion.Header, connector.ClientHeader("codex"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var e struct{ Error struct{ Code string } }
		json.Unmarshal(body, &e)
		if resp.StatusCode != http.StatusUpgradeRequired || e.Error.Code != "update_required" {
			t.Fatalf("%s for an old connector: %d %s", path, resp.StatusCode, body)
		}
	}
	asVersion(t, "2026.10.21.0800+new")
	b, err := p.Poll(ctx, link, 5*time.Second, false)
	if err != nil || len(b.Deliveries) != 1 || !strings.Contains(string(mustJSON(b.Deliveries[0].Event)), "held for the update") {
		t.Fatalf("what the old one was refused: %+v %v", b, err)
	}
}

// A subscribe's result (an app's own watch tool) says it too, once the
// session's connector was refused.
func TestWatchSaysTheHooksAreOutOfDate(t *testing.T) {
	c := coretest.StartWith(t, func(o *core.Options) {
		o.Connectors = connectorversion.Minimums{"wakeshift": "2026.10.20"}
	})
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")
	const thread = "01a14bd7-bbbc-7b70-a723-0abb9f384444"
	if _, err := agentkit.ConversationSession(ctx, codex, thread); err != nil {
		t.Fatal(err)
	}
	codexH, _ := harnesses.Get("codex")
	watched, err := threadWatch(t, c, app, codex, thread)
	if err != nil || watched.Beacon == "" || strings.Contains(watched.Text, harnesses.Outdated(codexH)) {
		t.Fatalf("watch: %+v %v", watched, err)
	}
	claims, err := beacon.Verify(watched.Beacon, thread, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code, out := redeemTicket(t, claims.Redeem, claims.Ticket, thread); code != http.StatusUpgradeRequired {
		t.Fatalf("redeem: %d %v", code, out)
	}
	if watched, err = threadWatch(t, c, app, codex, thread); err != nil || !strings.Contains(watched.Text, harnesses.Outdated(codexH)) || watched.Beacon == "" {
		t.Fatalf("watch after the refusal: %+v %v", watched, err)
	}
}

// Raising the minimum ends a live stream whose connector it no longer
// serves within one renewal: it closes with update_required (on a
// WebSocket, CodeUpdateRequired), and what comes after is held for the
// session (no tool result hands it over); subscribing asks for the update.
func TestRaisingTheMinimumEndsALiveStream(t *testing.T) {
	const renew = 300 * time.Millisecond
	c := coretest.StartWith(t, func(o *core.Options) {
		o.Connectors = connectorversion.Minimums{"wakeshift": "2026.10.20"}
		o.StreamRenew = renew
	})
	app := noteApp(t, c, "ci")
	const thread = "019a14bd-bbbc-7b70-a723-0abb9f385555"
	call := caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	asVersion(t, "2026.10.21.0800+new")
	link, err := connector.Redeem(ctx, nil, "codex", codexBeacon(t, c, sub.Addition, thread), thread)
	if err != nil {
		t.Fatal(err)
	}
	ws, _, err := websocket.Dial(ctx, link, &websocket.DialOptions{HTTPHeader: http.Header{connectorversion.Header: {connector.ClientHeader("codex")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if _, err := emitNote(app, []string{"ci/acme"}, note{Text: "while served"}); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, b, err := ws.Read(rctx)
	if err != nil || !strings.Contains(string(b), "while served") {
		t.Fatalf("before: %s %v", b, err)
	}
	var f struct {
		DeliveryID string `json:"delivery_id"`
	}
	json.Unmarshal(b, &f)
	ack, _ := json.Marshal(map[string]string{"type": "ack", "delivery_id": f.DeliveryID})
	ws.Write(ctx, websocket.MessageText, ack)

	raised := time.Now()
	c.Streams.SetMinimums(connectorversion.Minimums{"wakeshift": "2026.10.22"})
	for {
		_, b, err = ws.Read(rctx)
		if err != nil {
			break
		}
	}
	if websocket.CloseStatus(err) != streams.CodeUpdateRequired || !strings.Contains(err.Error(), "update_required") {
		t.Fatalf("the stream ended with %v", err)
	}
	if took := time.Since(raised); took > 2*renew+time.Second {
		t.Fatalf("ended %v after the minimum was raised (renewal %v)", took, renew)
	}
	if _, err := emitNote(app, []string{"ci/acme"}, note{Text: "after it was raised"}); err != nil {
		t.Fatal(err)
	}
	codex, _ := harnesses.Get("codex")
	heldNotShown(t, c, app, call, sub.ID, "after it was raised")
	if again, _ := subscribe(app, call, "ci/acme", subOpts{}); !strings.Contains(again.Addition.Text, harnesses.Outdated(codex)) {
		t.Fatalf("no update note: %s", again.Addition.Text)
	}
}

func linkToken(link string) string {
	u, _ := url.Parse(link)
	return u.Query().Get("link")
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
