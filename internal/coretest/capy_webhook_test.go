package coretest_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// fakeCapy stands in for Capy's webhook endpoint on this machine
// (agenthooks.AllowTestOrigin): it records each POST and answers from a
// plan, then 202 accepted.
type fakeCapy struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []capyReq
	plan []int // statuses to answer in turn: capyNoMatch, capyDrop and capyRedirect are special
	max  int   // > 0: 413 for a body longer than this
	to   string
}

type capyReq struct {
	Path, Key, Type string
	Body            []byte
}

const (
	capyNoMatch  = 2021 // 202 no-match
	capyDrop     = 999  // the connection is cut: a network error
	capyRedirect = 302
)

func newFakeCapy(t *testing.T) *fakeCapy {
	f := &fakeCapy{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, capyReq{Path: r.URL.Path, Key: r.Header.Get("Idempotency-Key"), Type: r.Header.Get("Content-Type"), Body: b})
		status := http.StatusAccepted
		if len(f.plan) > 0 {
			status, f.plan = f.plan[0], f.plan[1:]
		}
		if f.max > 0 && len(b) > f.max {
			status = http.StatusRequestEntityTooLarge
		}
		to := f.to
		f.mu.Unlock()
		switch status {
		case capyDrop:
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		case capyNoMatch:
			w.WriteHeader(http.StatusAccepted)
			io.WriteString(w, `{"status":"no-match"}`)
		case capyRedirect:
			http.Redirect(w, r, to, http.StatusFound)
		case http.StatusAccepted:
			w.WriteHeader(status)
			io.WriteString(w, `{"status":"accepted"}`)
		default:
			w.WriteHeader(status)
		}
	}))
	t.Cleanup(f.srv.Close)
	t.Cleanup(agenthooks.AllowTestOrigin(f.srv.URL))
	return f
}

func (f *fakeCapy) watch(secret string) string { return f.srv.URL + agenthooks.CapyPath + secret }

func (f *fakeCapy) got() []capyReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capyReq(nil), f.reqs...)
}

func (f *fakeCapy) answer(plan ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plan = plan
}

// fastHooks shortens the agent webhooks' backoff and spacing for a test
// (package settings, changed atomically: a deliverer may still be reading
// them while the cleanup restores them).
func fastHooks(t *testing.T) {
	t.Cleanup(agenthooks.SetTestTimings(200*time.Millisecond, time.Millisecond))
}

var registerRE = regexp.MustCompile(`curl -X POST '([^']+)' -H 'content-type: application/json' -d '\{"url":"<the watch URL>"\}'`)

// registrationLink is the link a Capy result gives.
func registrationLink(t *testing.T, text string) string {
	t.Helper()
	m := registerRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no registration command: %s", text)
	}
	return m[1]
}

// hookCall sends the registration link a request.
func hookCall(t *testing.T, method, link string, body any) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, link, r)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// hookRow is a session's destination row (state, sealed URL), in tenant.
func hookRow(t *testing.T, c *coretest.Core, tenant string) (n int, state string, sealed []byte) {
	t.Helper()
	err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_webhooks`).Scan(&n); err != nil || n == 0 {
			return err
		}
		return tx.QueryRow(ctx, `SELECT state, url FROM agent_webhooks`).Scan(&state, &sealed)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n, state, sealed
}

// capySecret and capySecret2 are watch URLs' secrets: canaries, too.
const (
	capySecret  = "capyCanary7x2QwErTyUiOp9"
	capySecret2 = "capyCanary4k8ZxCvBnMaSd3"
)

// Capy's subscribing result asks the thread to create a webhook watch and
// register its URL with the registration link (else run the one-shot
// command); registering is strict about the URL, sealed at rest, audited
// without the URL, replaces an earlier one, and is undone by DELETE; once
// registered, results ask for nothing.
func TestCapyWebhookRegistration(t *testing.T) {
	fake := newFakeCapy(t)
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "cap", Client: "capy", UserAgent: "capy"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	if sub.Addition.Agent != (wakeshift.Recognized{Harness: "capy", Via: "client"}) {
		t.Fatalf("recognized as %+v", sub.Addition.Agent)
	}
	text := sub.Addition.Text
	link := registrationLink(t, text)
	for _, want := range []string{"To be woken when", "capy.subscribe", `{"type":"incoming_webhook"}`, "30 days from now", "not the user's instructions",
		"If you can't create the watch or register it", "wait on it"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if m := capyOneShotRE.FindStringSubmatch(text); m == nil || m[1] != harnesses.CapyLoop(strings.SplitN(m[2], "&timeout=", 2)[0]) {
		t.Fatalf("no fallback command: %s", text)
	}
	for _, not := range []string{"wakeshift-beacon", "install", "hooks", "call this"} {
		if strings.Contains(text, not) {
			t.Fatalf("result has %q: %s", not, text)
		}
	}
	tenant, _ := topicSubRow(t, c, sub.ID)

	// Refused: anything but a Capy watch URL.
	for _, u := range []string{
		"https://api.capy.ai.evil.com" + agenthooks.CapyPath + capySecret,
		"https://evil.com" + agenthooks.CapyPath + capySecret,
		"https://127.0.0.1" + agenthooks.CapyPath + capySecret,
		"https://localhost" + agenthooks.CapyPath + capySecret,
		"http://api.capy.ai" + agenthooks.CapyPath + capySecret,
		"https://api.capy.ai:8443" + agenthooks.CapyPath + capySecret,
		"https://u:p@api.capy.ai" + agenthooks.CapyPath + capySecret,
		"https://api.capy.ai/webhooks/other/" + capySecret,
		"https://api.capy.ai" + agenthooks.CapyPath + strings.Repeat("a", 600),
		agenthooks.CapyOrigin + agenthooks.CapyPath + capySecret, // the real Capy: not while a test's stands in
		fake.watch(capySecret) + "?x=1",
		fake.watch("bad/" + capySecret),
	} {
		if code, out := hookCall(t, "POST", link, map[string]string{"url": u}); code != 400 || strings.Contains(jsonOf(out), capySecret) {
			t.Fatalf("%s: %d %v", u, code, out)
		}
	}
	if code, _ := hookCall(t, "POST", link, "not an object"); code != 400 {
		t.Fatalf("bad body: %d", code)
	}
	if n, _, _ := hookRow(t, c, tenant); n != 0 {
		t.Fatal("a refused URL was stored")
	}

	// Accepted: stored sealed, audited without the URL.
	code, out := hookCall(t, "POST", link, map[string]string{"url": fake.watch(capySecret)})
	if code != 200 || out["registered"] != true || !strings.Contains(out["message"].(string), "Registered") {
		t.Fatalf("register: %d %v", code, out)
	}
	n, state, sealed := hookRow(t, c, tenant)
	if n != 1 || state != "live" || bytes.Contains(sealed, []byte(capySecret)) {
		t.Fatalf("row: %d %s sealed=%v", n, state, bytes.Contains(sealed, []byte(capySecret)))
	}
	if count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.registered' AND detail->>'kind' = 'capy' AND NOT (detail->>'replaced')::bool`) != 1 ||
		count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE detail::text LIKE '%' || $1 || '%'`, "capyCanary") != 0 {
		t.Fatal("audit entry missing, or it has the URL")
	}

	// Registered: the next subscribing result asks for nothing; other
	// results add nothing.
	again, _ := subscribe(app, call, "ci/acme/web", subOpts{})
	if !strings.HasPrefix(again.Addition.Text, harnesses.CapyRegisteredNote("Ci")) || strings.Contains(again.Addition.Text, "curl") ||
		strings.Contains(again.Addition.Text, "capy.subscribe") {
		t.Fatalf("registered: %s", again.Addition.Text)
	}
	if r, _ := annotate(app, call); r.Text != "" || len(r.Meta) != 0 {
		t.Fatalf("annotate: %+v", r)
	}
	if st, _ := noteStatus(app, "ci/acme"); st.Connected != 1 || st.Via["webhook"] != 1 {
		t.Fatalf("status: %+v", st)
	}

	// Registering again replaces it: deliveries go to the new URL.
	if code, _ := hookCall(t, "POST", link, map[string]string{"url": fake.watch(capySecret2)}); code != 200 {
		t.Fatal(code)
	}
	if count(t, c, tenant, `SELECT count(*) FROM agent_webhooks`) != 1 ||
		count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.registered' AND (detail->>'replaced')::bool`) != 1 {
		t.Fatal("not replaced")
	}
	emitNote(app, []string{"ci/acme"}, note{Text: "deploy finished"})
	waitFor(t, "the delivery", func() bool { return len(fake.got()) == 1 })
	if p := fake.got()[0].Path; p != agenthooks.CapyPath+capySecret2 {
		t.Fatalf("delivered to %s", p)
	}

	// Another conversation (another user's) gets its own link, which
	// registers only its own destination.
	call2 := caller{User: "cap2", Client: "capy"}
	sub2, _ := subscribe(app, call2, "ci/acme", subOpts{})
	link2 := registrationLink(t, sub2.Addition.Text)
	tenant2, _ := topicSubRow(t, c, sub2.ID)
	if link2 == link || tenant2 == tenant {
		t.Fatal("the same link or workspace")
	}
	if code, _ := hookCall(t, "POST", link2, map[string]string{"url": fake.watch(capySecret)}); code != 200 {
		t.Fatal(code)
	}
	if n, _, _ := hookRow(t, c, tenant2); n != 1 || count(t, c, tenant, `SELECT count(*) FROM agent_webhooks`) != 1 {
		t.Fatal("registered for the wrong conversation")
	}

	// A tampered or made-up link, a stream link, or a link of another
	// harness: refused.
	if code, _ := hookCall(t, "POST", link+"x", map[string]string{"url": fake.watch(capySecret)}); code != 401 {
		t.Fatalf("tampered: %d", code)
	}
	cur, _ := subscribe(app, caller{User: "cur", Client: "cursor"}, "ci/acme", subOpts{})
	// Cursor's command claims its link (#1152): claimed here, as it would.
	streamURL := ""
	if m := coretest.ClaimRE.FindStringSubmatch(shellCommand(t, cur.Addition.Text)); m != nil {
		streamURL, _ = coretest.ClaimLink(m[2], m[1], "")
	}
	if streamURL == "" {
		t.Fatalf("no cursor link: %s", cur.Addition.Text)
	}
	asHook := strings.Replace(streamURL, "/v1/stream/wait", agenthooks.RegisterPath, 1)
	if code, _ := hookCall(t, "POST", asHook, map[string]string{"url": fake.watch(capySecret)}); code != 401 {
		t.Fatalf("a stream link registered: %d", code)
	}
	curTenant, curLink, ok := c.Streams.Held(streamURL)
	if !ok {
		t.Fatal("not a link")
	}
	forged := agenthooks.NewLinks(coreKey("agent-webhook-register")).URL(c.URL, curTenant, curLink, "cursor", time.Now().Add(time.Hour))
	if code, _ := hookCall(t, "POST", forged, map[string]string{"url": fake.watch(capySecret)}); code != 403 {
		t.Fatalf("a cursor conversation registered: %d", code)
	}
	if n, _, _ := hookRow(t, c, curTenant); n != 0 {
		t.Fatal("a cursor conversation has a destination")
	}

	// DELETE unregisters (audited); the next subscribing result offers
	// registration again, briefly.
	if code, out := hookCall(t, "DELETE", link, nil); code != 200 || out["removed"] != true {
		t.Fatalf("delete: %d %v", code, out)
	}
	if count(t, c, tenant, `SELECT count(*) FROM agent_webhooks`) != 0 ||
		count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'agent_webhook.unregistered'`) != 1 {
		t.Fatal("not unregistered")
	}
	after, _ := subscribe(app, call, "ci/acme", subOpts{})
	if !strings.HasPrefix(after.Addition.Text, "This thread has no webhook watch registered") || registerRE.FindString(after.Addition.Text) == "" {
		t.Fatalf("after delete: %s", after.Addition.Text)
	}

	// Unsubscribing from everything ends the destination with the links.
	if code, _ := hookCall(t, "POST", registrationLink(t, after.Addition.Text), map[string]string{"url": fake.watch(capySecret)}); code != 200 {
		t.Fatal(code)
	}
	for _, topic := range []string{"ci/acme", "ci/acme/web"} {
		if _, err := unsubscribe(app, call, topic); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "the destination ended", func() bool { return count(t, c, tenant, `SELECT count(*) FROM agent_webhooks`) == 0 })
	if code, _ := hookCall(t, "POST", link, map[string]string{"url": fake.watch(capySecret)}); code != 401 {
		t.Fatalf("a revoked link registered: %d", code)
	}
}

// capyOneShotRE is Capy's fallback command (harnesses.CapyLoop) and its url.
var capyOneShotRE = cmdRE{re: regexp.MustCompile(loopCmd(`c=; p=; s=\$\(date \+%s\); `) + ` \.`)}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

// coreKey is the core's key for a purpose, as coretest derives it.
func coreKey(purpose string) []byte {
	return coretest.Key(purpose)
}

// deliveryOf is an event's delivery's ID and state, by its text.
func deliveryOf(t *testing.T, c *coretest.Core, tenant, text string) (id, state string) {
	t.Helper()
	err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT d.id::text, d.state FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id
			WHERE e.subject = $1 OR e.body = $1`, text).Scan(&id, &state)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "" // not made yet (the pipeline runs after the emit)
	}
	if err != nil {
		t.Fatal(err)
	}
	return id, state
}

// Delivery to a registered watch: held messages go when it's registered;
// each goes once, as the Monitor line's fields, with its delivery ID as
// the Idempotency-Key and only a 2xx acknowledging it; a 5xx or a cut
// connection is retried with the same key; a redirect isn't followed; a
// 404 kills the destination and the message waits until the thread
// registers again (a subscribing result offers it); a big payload goes as
// its preview, and a 413 gets the Monitor line's preview. Nothing logged
// carries the watch URL's secret.
func TestCapyWebhookDelivery(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fastHooks(t)
	fake := newFakeCapy(t)
	elsewhere := newRecorder(t)
	fake.to = elsewhere.URL
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")
	call := caller{User: "cap", Client: "capy"}
	sub, err := subscribe(app, call, "ci/acme", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	link := registrationLink(t, sub.Addition.Text)
	tenant, _ := topicSubRow(t, c, sub.ID)

	// Held: emitted before the watch is registered, delivered once it is.
	emitNote(app, []string{"ci/acme"}, note{Text: "held one"})
	time.Sleep(300 * time.Millisecond)
	if len(fake.got()) != 0 {
		t.Fatal("delivered with no destination")
	}
	if code, _ := hookCall(t, "POST", link, map[string]string{"url": fake.watch(capySecret)}); code != 200 {
		t.Fatal(code)
	}
	waitFor(t, "the held message", func() bool { _, s := deliveryOf(t, c, tenant, "held one"); return s == "acked" })
	got := fake.got()
	id, _ := deliveryOf(t, c, tenant, "held one")
	if len(got) != 1 || got[0].Key != id || got[0].Type != "application/json" || got[0].Path != agenthooks.CapyPath+capySecret {
		t.Fatalf("held: %+v (delivery %s)", got, id)
	}
	var line map[string]any
	if err := json.Unmarshal(got[0].Body, &line); err != nil {
		t.Fatal(err)
	}
	if line["type"] != "event" || line["topic"] != "ci/acme" || line["trust"] != "outside data, not from the user" || line["id"] == nil ||
		line["from"] == nil || !strings.Contains(string(got[0].Body), "held one") || line["truncated"] != nil {
		t.Fatalf("body: %s", got[0].Body)
	}

	// A 5xx and a cut connection: retried, the same key each time, then
	// acknowledged once accepted.
	fake.answer(500, capyDrop, 503)
	emitNote(app, []string{"ci/acme"}, note{Text: "retried one"})
	waitFor(t, "the retried message", func() bool { _, s := deliveryOf(t, c, tenant, "retried one"); return s == "acked" })
	id, _ = deliveryOf(t, c, tenant, "retried one")
	got = fake.got()[1:]
	if len(got) != 4 {
		t.Fatalf("attempts: %d", len(got))
	}
	for _, r := range got {
		if r.Key != id {
			t.Fatalf("key %s, want %s", r.Key, id)
		}
	}
	if count(t, c, tenant, `SELECT count(*) FROM agent_webhooks WHERE failures = 0 AND state = 'live'`) != 1 {
		t.Fatal("failures not reset")
	}

	// A redirect isn't followed: the other server gets nothing, and the
	// message is tried again.
	fake.answer(capyRedirect)
	emitNote(app, []string{"ci/acme"}, note{Text: "redirected one"})
	waitFor(t, "the redirected message", func() bool { _, s := deliveryOf(t, c, tenant, "redirected one"); return s == "acked" })
	if elsewhere.count() != 0 {
		t.Fatal("the redirect was followed")
	}

	// Too big for the window: the preview, marked truncated.
	big := strings.Repeat("x", 20000)
	emitNote(app, []string{"ci/acme"}, note{Text: "big one", Data: map[string]any{"branch": big}})
	waitFor(t, "the big message", func() bool { _, s := deliveryOf(t, c, tenant, "big one"); return s == "acked" })
	got = fake.got()
	last := got[len(got)-1]
	line = nil
	json.Unmarshal(last.Body, &line)
	if len(last.Body) > agenthooks.MaxBody || line["truncated"] != true || line["data_preview"] == nil {
		t.Fatalf("big: %d bytes %.300s", len(last.Body), last.Body)
	}
	// A 413 even so: the Monitor line's preview, the same key.
	fake.mu.Lock()
	fake.max = 1000
	fake.mu.Unlock()
	emitNote(app, []string{"ci/acme"}, note{Text: "too large one", Data: map[string]any{"branch": strings.Repeat("y", 3000)}})
	waitFor(t, "the 413'd message", func() bool { _, s := deliveryOf(t, c, tenant, "too large one"); return s == "acked" })
	got = fake.got()
	a, b := got[len(got)-2], got[len(got)-1]
	if len(a.Body) <= 1000 || len(b.Body) > 480 || a.Key != b.Key || !strings.Contains(string(b.Body), `"truncated":true`) {
		t.Fatalf("413: %d then %d bytes, keys %s %s", len(a.Body), len(b.Body), a.Key, b.Key)
	}
	fake.mu.Lock()
	fake.max = 0
	fake.mu.Unlock()

	// A 404: the destination is dead and the message waits; the next
	// subscribing result offers registration again; registering delivers
	// it, with the same key.
	fake.answer(404)
	emitNote(app, []string{"ci/acme"}, note{Text: "waiting one"})
	waitFor(t, "the destination dead", func() bool {
		return count(t, c, tenant, `SELECT count(*) FROM agent_webhooks WHERE state = 'dead' AND dead_reason = 'not_found'`) == 1
	})
	before := len(fake.got())
	time.Sleep(500 * time.Millisecond)
	if len(fake.got()) != before {
		t.Fatal("delivered to a dead destination")
	}
	wid, wstate := deliveryOf(t, c, tenant, "waiting one")
	if wstate != "pending" {
		t.Fatalf("state %s", wstate)
	}
	dead, _ := subscribe(app, call, "ci/acme", subOpts{})
	if !strings.HasPrefix(dead.Addition.Text, "This thread's webhook watch no longer takes Wakeshift's messages") {
		t.Fatalf("dead: %s", dead.Addition.Text)
	}
	if code, _ := hookCall(t, "POST", registrationLink(t, dead.Addition.Text), map[string]string{"url": fake.watch(capySecret2)}); code != 200 {
		t.Fatal(code)
	}
	waitFor(t, "the waiting message", func() bool { _, s := deliveryOf(t, c, tenant, "waiting one"); return s == "acked" })
	got = fake.got()
	if r := got[len(got)-1]; r.Key != wid || r.Path != agenthooks.CapyPath+capySecret2 || !strings.Contains(string(r.Body), "waiting one") {
		t.Fatalf("after re-registering: %+v", r)
	}

	// 202 no-match: the watch filters messages out, so it's dead too.
	fake.answer(capyNoMatch)
	emitNote(app, []string{"ci/acme"}, note{Text: "filtered one"})
	waitFor(t, "no-match", func() bool {
		return count(t, c, tenant, `SELECT count(*) FROM agent_webhooks WHERE state = 'dead' AND dead_reason = 'no_match'`) == 1
	})
	if _, s := deliveryOf(t, c, tenant, "filtered one"); s != "pending" {
		t.Fatalf("no-match acknowledged: %s", s)
	}

	// Each message went once (per accepted attempt): no acknowledged one
	// went again.
	seen := map[string]int{}
	for _, r := range fake.got() {
		seen[r.Key]++
	}
	if seen[id] != 4 {
		t.Fatalf("the retried one went %d times", seen[id])
	}

	// Secrecy: the logs carry the deliveries, never the watch URLs'
	// secrets (nor the registration link's token).
	out := logs.String()
	if !strings.Contains(out, "agent webhooks: delivered") || !strings.Contains(out, "agent webhooks: delivery failed; retrying") ||
		!strings.Contains(out, "agent webhooks: destination dead") {
		t.Fatalf("the test isn't seeing the deliverer's lines:\n%s", out)
	}
	token := strings.SplitN(link, "link=", 2)[1]
	for _, s := range []string{"capyCanary", token, fake.srv.URL + agenthooks.CapyPath} {
		if strings.Contains(out, s) {
			t.Fatalf("the logs carry %q", s)
		}
	}
}

// recorder counts the requests it gets.
type recorder struct {
	*httptest.Server
	mu sync.Mutex
	n  int
}

func newRecorder(t *testing.T) *recorder {
	r := &recorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		r.n++
		r.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}
