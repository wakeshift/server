package streams_test

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// The grace path (grace.go): a session's link that expired while nothing
// used it renews once, within a day, while it's the latest of its chain,
// its grant is live and its session watches; anything else is refused as
// before.

const connectorName = "wakeshift/9.9.9 claude-code"

// graceSession registers a watching session of codex's and mints its link:
// a connector's (acks, connector) or, with slide, a Monitor's (sliding,
// no acks), with ttl.
func (f *fixture) graceSession(label string, slide bool, ttl time.Duration) (link, id string) {
	f.t.Helper()
	f.tx(func(tx pgx.Tx) error {
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "codex", Label: label, CreatedBy: "test"})
		if err != nil {
			return err
		}
		id = s.ID
		if err := sessions.SetWatching(ctx, tx, id, true); err != nil {
			return err
		}
		link, err = f.mintFor(tx, id, slide, ttl)
		return err
	})
	return link, id
}

func (f *fixture) mintFor(tx pgx.Tx, session string, slide bool, ttl time.Duration) (string, error) {
	n := streams.NewLink{Agent: "codex", Target: session, Ack: true, CreatedBy: "test", TTL: ttl, Connector: connectorName}
	if slide {
		n = streams.NewLink{Agent: "codex", Target: session, CreatedBy: "test", TTL: ttl, Slide: streams.SessionLinkMax}
	}
	u, _, err := f.s.Mint(ctx, tx, f.tenant, n)
	return u, err
}

// expire moves a link's expiry back by ago, as if nothing had used it
// since (its token's own expiry, if later, still checks out: the stored
// expiry is what ended it).
func (f *fixture) expire(link string, ago time.Duration) {
	f.t.Helper()
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE stream_links SET expires_at = now() - $2::interval WHERE id = $1`, linkID(f.t, link), ago.String())
		return err
	})
}

// linkID is the stored link's ID a link's token names.
func linkID(t *testing.T, link string) string {
	t.Helper()
	b, _ := base64.RawURLEncoding.DecodeString(strings.Split(token(link), ".")[0])
	var c struct{ L string }
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c.L
}

// renewed reports whether the link was renewed (and so revoked).
func (f *fixture) renewed(link string) bool {
	f.t.Helper()
	var r bool
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT renewed_at IS NOT NULL AND revoked_at IS NOT NULL FROM stream_links WHERE id = $1`, linkID(f.t, link)).Scan(&r)
	})
	return r
}

// A connector's link that expired while nothing polled it (its token's
// expiry too: a real expiry, not a stored one): the next poll gets no
// deliveries and its successor in rotate; the successor gets what waited;
// the old link is used up (revoked, with renewed_at), and the renewal is
// in the audit log.
func TestGraceRenewsAnExpiredConnectorLink(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	link, id := f.graceSession("slept", false, time.Second)
	time.Sleep(2100 * time.Millisecond) // asleep past its expiry
	f.toSession(id, "while you slept")

	b := mustPoll(t, base, token(link), "wait=0")
	if len(b.Deliveries) != 0 || b.Rotate == nil || b.Rotate.Link == "" || b.Rotate.Link == token(link) || token(b.Rotate.URL) != b.Rotate.Link {
		t.Fatalf("the renewal: %+v %+v", b, b.Rotate)
	}
	if !f.renewed(link) {
		t.Fatal("the old link wasn't used up")
	}
	got := mustPoll(t, base, b.Rotate.Link, "wait=2s")
	if len(got.Deliveries) != 1 || got.Deliveries[0].Event.Subject != "while you slept" {
		t.Fatalf("the successor: %+v", got)
	}
	// Single use: the old link again is refused (its token expired: as
	// link_expired, as before).
	if code, again := poll(t, base, token(link), "wait=0"); code != 401 || again.Rotate != nil || again.Error.Code != "link_expired" {
		t.Fatalf("the old link again: %d %+v", code, again.Error)
	}
	var detail string
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT detail::text FROM audit_log WHERE action = 'stream_link.renewed'`).Scan(&detail)
	})
	if !strings.Contains(detail, linkID(t, link)) || !strings.Contains(detail, `"kind": "connector"`) || strings.Contains(detail, token(link)) || strings.Contains(detail, b.Rotate.Link) {
		t.Fatalf("audit: %s", detail)
	}
	// The successor rotates and renews as any link does: it's the latest now.
	f.expire(b.Rotate.URL, time.Hour)
	if again := mustPoll(t, base, b.Rotate.Link, "wait=0"); again.Rotate == nil {
		t.Fatalf("the successor's own renewal: %+v", again)
	}
}

// Everything else keeps today's refusal: a link superseded by a newer one
// of its chain, a revoked link, one expired over a day ago, one whose grant
// ended, one whose session stopped watching, an agent's link (no session)
// and a session link of a kind that doesn't rotate.
func TestGraceRefusals(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	refused := func(name, link, code string) {
		t.Helper()
		if c, b := poll(t, base, token(link), "wait=0"); c != 401 || b.Error.Code != code || b.Rotate != nil {
			t.Errorf("%s: %d %+v %+v", name, c, b.Error, b.Rotate)
		}
		if f.renewed(link) {
			t.Errorf("%s: renewed", name)
		}
	}

	// Superseded: a newer link for the same session (a newer redeem, or a
	// rotation's successor), expired too.
	old, id := f.graceSession("superseded", false, time.Hour)
	var newer string
	f.tx(func(tx pgx.Tx) (err error) { newer, err = f.mintFor(tx, id, false, time.Hour); return err })
	f.expire(old, 2*time.Hour)
	f.expire(newer, time.Hour)
	refused("superseded", old, "link_revoked")
	// The newer one is the latest: it renews.
	if b := mustPoll(t, base, token(newer), "wait=0"); b.Rotate == nil {
		t.Errorf("the latest: %+v", b)
	}

	revoked, id := f.graceSession("revoked", false, time.Hour)
	f.expire(revoked, time.Minute)
	f.tx(func(tx pgx.Tx) error { _, err := streams.RevokeSessionLinks(ctx, tx, "codex", id); return err })
	refused("revoked", revoked, "link_revoked")

	long, _ := f.graceSession("long", false, time.Hour)
	f.expire(long, 25*time.Hour)
	refused("expired a day ago", long, "link_revoked")

	unwatched, id := f.graceSession("unwatched", false, time.Hour)
	f.expire(unwatched, time.Minute)
	f.tx(func(tx pgx.Tx) error { return sessions.SetWatching(ctx, tx, id, false) })
	refused("not watching", unwatched, "link_revoked")

	if _, err := f.d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('pacer', 'Pacer')`); err != nil {
		t.Fatal(err)
	}
	var granted string
	f.tx(func(tx pgx.Tx) error {
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "codex", Label: "granted", CreatedBy: "test"})
		if err != nil {
			return err
		}
		if err := sessions.SetWatching(ctx, tx, s.ID, true); err != nil {
			return err
		}
		grant := dbtest.Grant(t, tx, "pacer", "codex", time.Hour)
		granted, _, err = f.s.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: "codex", Target: s.ID, Ack: true, CreatedBy: "app:pacer", GrantID: grant,
			TTL: time.Hour, Connector: connectorName})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE grants SET expires_at = now() - interval '1 second' WHERE id = $1`, grant)
		return err
	})
	f.expire(granted, time.Minute)
	refused("grant ended", granted, "link_revoked")

	agentLink := f.link(true, time.Hour)
	f.expire(agentLink, time.Minute)
	refused("an agent's link", agentLink, "link_revoked")

	plain, _ := f.sessionLinkFor("plain", time.Hour) // no acks, doesn't slide
	f.expire(plain, time.Minute)
	refused("a link that doesn't rotate", plain, "link_revoked")

	// A real expiry over a day ago is refused before any database work,
	// as link_expired.
	stale := forged(t, f, f.tenant, linkID(t, long), time.Now().Add(-25*time.Hour))
	if c, b := poll(t, base, stale, "wait=0"); c != 401 || b.Error.Code != "link_expired" {
		t.Errorf("a token expired a day ago: %d %+v", c, b.Error)
	}
}

// forged is a link token for id in tenant, expiring at exp, signed with
// the fixture's key (as the server signs: urlOf).
func forged(t *testing.T, f *fixture, tenant, id string, exp time.Time) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"t": tenant, "l": id, "e": exp.Unix()})
	b := base64.RawURLEncoding.EncodeToString(body)
	m := hmac.New(sha256.New, f.s.Key)
	m.Write([]byte("stream-link:" + b))
	return b + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Two renewals of the same link at once: exactly one gets a successor.
func TestGraceRenewsOnce(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	link, _ := f.graceSession("race", false, time.Hour)
	f.expire(link, time.Minute)
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	rotated := make([]bool, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, b := poll(t, base, token(link), "wait=0")
			codes[i], rotated[i] = c, b.Rotate != nil
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for i := range n {
		switch {
		case codes[i] == 200 && rotated[i]:
			ok++
		case codes[i] != 401:
			t.Errorf("request %d: %d", i, codes[i])
		}
	}
	if ok != 1 {
		t.Fatalf("%d renewals of one link (codes %v)", ok, codes)
	}
	var successors int
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM stream_links WHERE id <> $1`, linkID(t, link)).Scan(&successors)
	})
	if successors != 1 {
		t.Fatalf("%d successors", successors)
	}
}

// A sliding link's URL is in the agent's text (its command), so a copy of
// it renews only within TextLinkGrace; a connector's link, which its hooks
// keep out of the text, still renews within RenewGrace.
func TestGraceIsShorterForLinksInText(t *testing.T) {
	f := setup(t)
	wb := f.waitServer()
	pb := f.pollServer()

	late, _ := f.graceSession("monitor-late", true, time.Hour)
	f.expire(late, streams.TextLinkGrace+time.Hour)
	if w := wait(t, waitURL(wb, late, "timeout=1s")); w.code != 401 || f.renewed(late) {
		t.Fatalf("a sliding link past TextLinkGrace: %d %v", w.code, w.last["type"])
	}
	inTime, _ := f.graceSession("monitor-in-time", true, time.Hour)
	f.expire(inTime, streams.TextLinkGrace-time.Hour)
	if w := wait(t, waitURL(wb, inTime, "timeout=1s")); w.code != 200 || w.last["type"] != "hub.next" || !f.renewed(inTime) {
		t.Fatalf("a sliding link within TextLinkGrace: %d %v", w.code, w.last["type"])
	}

	connector, _ := f.graceSession("connector", false, time.Hour)
	f.expire(connector, streams.TextLinkGrace+time.Hour)
	if b := mustPoll(t, pb, token(connector), "wait=0"); b.Rotate == nil || !f.renewed(connector) {
		t.Fatalf("a connector's link past TextLinkGrace: rotated %v", b.Rotate != nil)
	}
}

// A Monitor's (sliding) link that expired while nothing used it: its wait
// answers at once with the hub.next line naming its successor, keeping the
// wait's timeout and until; the old link's wait then says it was renewed.
// A lines stream on an expired link gets the hub.rotate line, a WebSocket
// the hub.rotate message.
func TestGraceRenewsWaitsAndStreams(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, id := f.graceSession("monitor", true, time.Hour)
	f.expire(link, 40*time.Minute)
	f.toSession(id, "after the sleep")
	// A confirm-only request (a command about to exit) doesn't renew it:
	// its answer would be thrown away.
	if cf := wait(t, waitURL(base, link, "after=00000000000000aa&confirm=1")); cf.code != 401 || f.renewed(link) {
		t.Fatalf("a confirm on the expired link: %d %v", cf.code, cf.last)
	}
	w := wait(t, waitURL(base, link, "timeout=50s&until=hooks"))
	next, _ := w.last["url"].(string)
	if w.code != 200 || w.last["type"] != "hub.next" || len(w.events) != 0 || token(next) == token(link) || token(next) == "" ||
		!strings.Contains(next, "timeout=50s") || !strings.Contains(next, "until=hooks") || !strings.Contains(w.last["note"].(string), "renewed") {
		t.Fatalf("the renewal: %d %v", w.code, w.last)
	}
	if got := wait(t, onServer(base, next)); len(got.events) != 1 {
		t.Fatalf("the successor's wait: %+v", got)
	}
	if again := wait(t, waitURL(base, link, "timeout=1s")); again.code != 401 || again.last["reason"] != "renewed" {
		t.Fatalf("the old link again: %d %v", again.code, again.last)
	}

	// Lines.
	lines, _ := f.graceSession("lines", true, time.Hour)
	f.expire(lines, time.Hour)
	resp, err := http.Get(onServer(base, streams.HTTPStreamURL(lines, streams.FormatLines)+"&max=5"))
	if err != nil {
		t.Fatal(err)
	}
	var rot map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() != "" {
			json.Unmarshal(sc.Bytes(), &rot)
		}
	}
	resp.Body.Close()
	u, _ := rot["url"].(string)
	if resp.StatusCode != 200 || rot["type"] != "hub.rotate" || token(u) == token(lines) || !strings.Contains(u, "format=lines") || !strings.Contains(u, "max=5") {
		t.Fatalf("lines: %d %v", resp.StatusCode, rot)
	}

	// WebSocket (Codex's connector).
	ws, _ := f.graceSession("ws", false, time.Hour)
	f.expire(ws, time.Hour)
	c, _, err := websocket.Dial(ctx, onServer(base, ws), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, msg, err := c.Read(rctx)
	var m map[string]any
	json.Unmarshal(msg, &m)
	if err != nil || m["type"] != "hub.rotate" || token(m["url"].(string)) == token(ws) {
		t.Fatalf("websocket: %v %s", err, msg)
	}
	if _, _, err := c.Read(rctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("websocket close: %v", err)
	}
}

// Renewal stays inside the link's session and workspace: another session's
// newer link doesn't supersede it, its successor reads only its own
// session, and a token naming the link's ID in another workspace finds
// nothing there to renew.
func TestGraceStaysInItsSessionAndWorkspace(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	mine, id := f.graceSession("mine", false, time.Hour)
	_, other := f.graceSession("other", false, time.Hour) // newer, another session
	f.expire(mine, time.Minute)
	f.toSession(other, "for the other session")
	f.toSession(id, "for mine")

	// Another workspace, with the same agent and a session of its own.
	tenantB := dbtest.NewTenant(t, f.d)
	if err := f.d.InTenant(ctx, tenantB, func(tx pgx.Tx) error {
		if _, err := agents.Create(ctx, tx, "codex", ""); err != nil {
			return err
		}
		s, err := sessions.Register(ctx, tx, sessions.New{Agent: "codex", Label: "b", CreatedBy: "test"})
		if err != nil {
			return err
		}
		return sessions.SetWatching(ctx, tx, s.ID, true)
	}); err != nil {
		t.Fatal(err)
	}
	if c, b := poll(t, base, forged(t, f, tenantB, linkID(t, mine), time.Now().Add(-time.Minute)), "wait=0"); c != 401 || b.Rotate != nil {
		t.Fatalf("its ID in another workspace: %d %+v", c, b)
	}

	b := mustPoll(t, base, token(mine), "wait=0")
	if b.Rotate == nil {
		t.Fatalf("not renewed beside another session's newer link: %+v", b)
	}
	got := mustPoll(t, base, b.Rotate.Link, "wait=2s")
	if len(got.Deliveries) != 1 || got.Deliveries[0].Event.Subject != "for mine" || got.Session != "codex@mine" {
		t.Fatalf("the successor read: %+v", got)
	}
	var claims struct{ T string }
	body, _ := base64.RawURLEncoding.DecodeString(strings.Split(b.Rotate.Link, ".")[0])
	json.Unmarshal(body, &claims)
	if claims.T != f.tenant {
		t.Fatalf("the successor's workspace: %s", claims.T)
	}
}

// A renewal racing an unsubscribe that stops the session (it locks the
// session's row, stops it watching, then revokes its links) waits for it,
// and is refused: no successor outlives the stop.
func TestGraceWaitsForAStoppingSession(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	link, id := f.graceSession("stopping", false, time.Hour)
	f.expire(link, time.Minute)

	locked, release, stopped := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		stopped <- f.d.InTenant(ctx, f.tenant, func(tx pgx.Tx) error {
			// As appapi's endListening: lockWatching, then stopListening.
			if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1::uuid FOR NO KEY UPDATE`, id); err != nil {
				return err
			}
			if err := sessions.SetWatching(ctx, tx, id, false); err != nil {
				return err
			}
			close(locked)
			<-release
			_, err := streams.RevokeSessionLinks(ctx, tx, "codex", id)
			return err
		})
	}()
	<-locked
	type answer struct {
		code    int
		rotated bool
	}
	answered := make(chan answer, 1)
	go func() {
		req, _ := http.NewRequest("GET", base+"/v1/stream/poll?wait=0", nil)
		req.Header.Set("Authorization", "Bearer "+token(link))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- answer{}
			return
		}
		defer resp.Body.Close()
		var b pollBody
		json.NewDecoder(resp.Body).Decode(&b)
		answered <- answer{resp.StatusCode, b.Rotate != nil}
	}()
	select {
	case a := <-answered:
		close(release)
		t.Fatalf("answered (%d, rotated %v) while the session was stopping", a.code, a.rotated)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if a := <-answered; a.code != 401 || a.rotated {
		t.Fatalf("after the stop: %d, rotated %v", a.code, a.rotated)
	}
	var live int
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM stream_links WHERE target = $1 AND revoked_at IS NULL`, id).Scan(&live)
	})
	if live != 0 {
		t.Fatalf("%d live links for the stopped session", live)
	}
}

// A connector's SSE stream with acks (Hermes's and OpenClaw's plugins) acks
// through POST /v1/stream/ack with its link's token, which stops working at
// the link's expiry while the connection itself could stay open until
// MaxAge. So the stream hands over the next link before the token expires
// (hub.rotate): a delivery that comes after the first link's expiry is
// acked with the link the stream gave, never refused as link_expired, which
// the plugins took for a link that's gone and forgot (#1124).
func TestSSEWithAcksRotatesBeforeItsLinkExpires(t *testing.T) {
	f := setup(t)
	f.s.RotateLead = time.Second
	base := f.pollServer()
	link, id := f.graceSession("sse-acks", false, 3*time.Second)
	sse := map[string]string{"Accept": "text/event-stream"}
	s := openStream(t, strings.Replace(link, "ws", "http", 1), sse)
	rotated := ""
	deadline := time.Now().Add(2500 * time.Millisecond) // before the first link's expiry
	for rotated == "" && time.Now().Before(deadline) {
		line, ok := s.next(time.Until(deadline))
		if !ok {
			break
		}
		if v, found := strings.CutPrefix(line, "data: "); found {
			var m struct{ Type, Link, URL string }
			if json.Unmarshal([]byte(v), &m) == nil && m.Type == "hub.rotate" {
				rotated = m.Link
			}
		}
	}
	if rotated == "" || rotated == token(link) {
		t.Fatalf("no hub.rotate before the link expired (got %q)", rotated)
	}
	time.Sleep(time.Until(deadline) + 1500*time.Millisecond) // the first link has expired now
	s = openStream(t, strings.Replace(link, "ws", "http", 1)[:strings.Index(link, "?")]+"?link="+rotated, sse)
	f.toSession(id, "after the first link expired")
	ev := s.event()
	for ev["event"] != "event" {
		ev = s.event()
	}
	if subject(data(t, ev)) != "after the first link expired" {
		t.Fatalf("event %v", ev)
	}
	req, _ := http.NewRequest("POST", base+"/v1/stream/ack", strings.NewReader(`{"delivery_ids":["`+ev["id"]+`"]}`))
	req.Header.Set("Authorization", "Bearer "+rotated)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ack with the stream's link: %d", resp.StatusCode)
	}
}
