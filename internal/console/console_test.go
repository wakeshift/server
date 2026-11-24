package console_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/auth"
	"github.com/wakeshift/server/internal/console"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/server"
	"github.com/wakeshift/server/internal/streams"
)

type client struct {
	t    *testing.T
	base string
	http *http.Client
}

// setup starts the console API with dev sign-in (the test server is on
// localhost).
func setup(t *testing.T) (*db.DB, *client) {
	t.Helper()
	d := dbtest.New(t)
	signIn := &auth.Console{Key: bytes.Repeat([]byte("k"), 32), DevSignIn: true, DevSignInToken: "console-test-dev-token", Accounts: &accounts.Store{Pool: d.Pool}}
	links := &streams.Server{DB: d, Key: bytes.Repeat([]byte("s"), 32)}
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	t.Cleanup(srv.Close)
	signIn.PublicURL, links.PublicURL = srv.URL, srv.URL
	// Routes depend on PublicURL (dev sign-in), so build them after it's known.
	srv.Config.Handler = (&server.Server{DB: d, Web: fstest.MapFS{}, APIs: []server.Registrar{signIn, &console.API{DB: d, Auth: signIn, Streams: links}}}).Handler()
	return d, newClient(t, srv.URL)
}

func newClient(t *testing.T, base string) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: base, http: &http.Client{Jar: jar}}
}

// do sends a request from the console's origin and decodes the response.
func (c *client) do(method, path string, body any, out any) int {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, c.base+path, &buf)
	req.Header.Set("Origin", c.base)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (c *client) signInAs(email string) {
	c.t.Helper()
	if code := c.do("POST", "/api/v1/session/dev", map[string]string{"email": email, "token": "console-test-dev-token"}, nil); code != 200 {
		c.t.Fatalf("sign in: %d", code)
	}
}

func (c *client) signIn() { c.signInAs("ada@example.com") }

func TestSignIn(t *testing.T) {
	_, c := setup(t)
	if code := c.do("GET", "/api/v1/agents", nil, nil); code != 401 {
		t.Fatalf("signed out: %d", code)
	}
	c.signIn()
	var s struct {
		Authenticated bool
		User          struct{ Email string }
		Workspace     struct{ Kind string }
	}
	c.do("GET", "/api/v1/session", nil, &s)
	if !s.Authenticated || s.User.Email != "ada@example.com" || s.Workspace.Kind != "personal" {
		t.Fatalf("session = %+v", s)
	}
	if code := c.do("GET", "/api/v1/agents", nil, nil); code != 200 {
		t.Fatalf("signed in: %d", code)
	}
	c.do("DELETE", "/api/v1/session", nil, nil)
	if code := c.do("GET", "/api/v1/agents", nil, nil); code != 401 {
		t.Fatalf("after sign-out: %d", code)
	}
}

// Two people each work in their own workspace and can't see each other's.
func TestWorkspacesAreSeparate(t *testing.T) {
	_, ada := setup(t)
	ada.signIn()
	bob := newClient(t, ada.base)
	bob.signInAs("bob@example.com")
	ada.do("POST", "/api/v1/agents", map[string]string{"id": "claude-code"}, nil)
	var list struct{ Agents []map[string]any }
	bob.do("GET", "/api/v1/agents", nil, &list)
	if len(list.Agents) != 0 {
		t.Fatalf("bob sees %v", list.Agents)
	}
	// Bob can't switch into Ada's workspace.
	var s struct{ Workspace struct{ ID string } }
	ada.do("GET", "/api/v1/session", nil, &s)
	if code := bob.do("POST", "/api/v1/session/workspace", map[string]string{"id": s.Workspace.ID}, nil); code != 403 {
		t.Fatalf("switch into another's workspace: %d", code)
	}
	// An ID that isn't a workspace's is a bad request, not a server error.
	for _, id := range []string{"", "not-a-uuid", "'; --"} {
		if code := bob.do("POST", "/api/v1/session/workspace", map[string]string{"id": id}, nil); code != 400 {
			t.Errorf("switch to %q: %d", id, code)
		}
	}
}

func TestRemovedMemberIsSignedOut(t *testing.T) {
	d, c := setup(t)
	c.signIn()
	d.Pool.Exec(context.Background(), `DELETE FROM memberships`)
	if code := c.do("GET", "/api/v1/agents", nil, nil); code != 401 {
		t.Fatalf("after removal: %d", code)
	}
}

func TestChangesMustComeFromTheConsole(t *testing.T) {
	_, c := setup(t)
	c.signIn()
	req, _ := http.NewRequest("POST", c.base+"/api/v1/agents", strings.NewReader(`{"id":"evil"}`))
	req.Header.Set("Origin", "https://attacker.example")
	resp, _ := c.http.Do(req)
	if resp.StatusCode != 403 {
		t.Fatalf("cross-origin POST: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", c.base+"/api/v1/agents", strings.NewReader(`{"id":"evil"}`)) // no Origin
	if resp, _ := c.http.Do(req); resp.StatusCode != 403 {
		t.Fatalf("POST without Origin: %d", resp.StatusCode)
	}
}

func TestTamperedSessionIsRejected(t *testing.T) {
	_, c := setup(t)
	c.signIn()
	u, _ := http.NewRequest("GET", c.base, nil)
	ck := c.http.Jar.Cookies(u.URL)[0]
	body, sig, _ := strings.Cut(ck.Value, ".")
	forged := &http.Cookie{Name: ck.Name, Value: body + "x." + sig}
	req, _ := http.NewRequest("GET", c.base+"/api/v1/agents", nil)
	req.AddCookie(forged)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 401 {
		t.Fatalf("forged cookie: %d", resp.StatusCode)
	}
}

func TestAgentsLinksAndSubscriptions(t *testing.T) {
	d, c := setup(t)
	c.signIn()

	if code := c.do("POST", "/api/v1/agents", map[string]string{"id": "claude-code", "display_name": "Claude Code"}, nil); code != 200 {
		t.Fatalf("create agent: %d", code)
	}
	if code := c.do("POST", "/api/v1/agents", map[string]string{"id": "claude-code"}, nil); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	if code := c.do("POST", "/api/v1/agents", map[string]string{"id": "Bad ID"}, nil); code != 400 {
		t.Fatalf("bad ID: %d", code)
	}

	// A stream link for the agent, signed for this workspace.
	var link struct {
		URL       string `json:"url"`
		ExpiresAt string `json:"expires_at"`
	}
	if code := c.do("POST", "/api/v1/agents/claude-code/stream-link", map[string]bool{"ack": false}, &link); code != 200 || !strings.HasPrefix(link.URL, "ws://") || link.ExpiresAt == "" {
		t.Fatalf("stream link: %d %+v", code, link)
	}
	if code := c.do("POST", "/api/v1/agents/nobody/stream-link", nil, nil); code != 404 {
		t.Fatalf("link for a missing agent: %d", code)
	}

	// Subscriptions.
	if code := c.do("POST", "/api/v1/subscriptions", map[string]any{"agent_id": "claude-code", "event_type": "slack.*.*"}, nil); code != 400 {
		t.Fatalf("bad pattern: %d", code)
	}
	var sub struct {
		ID, Status string
		Filters    map[string]string
	}
	if code := c.do("POST", "/api/v1/subscriptions", map[string]any{
		"agent_id": "claude-code", "event_type": "slack.message", "filters": map[string]string{"channel": "C1"},
	}, &sub); code != 200 || sub.Status != "active" || sub.Filters["channel"] != "C1" {
		t.Fatalf("create subscription: %d %+v", code, sub)
	}
	var list struct{ Subscriptions []map[string]any }
	c.do("GET", "/api/v1/subscriptions?agent=claude-code", nil, &list)
	if len(list.Subscriptions) != 1 {
		t.Fatalf("list = %v", list)
	}
	var canceled map[string]any
	c.do("POST", "/api/v1/subscriptions/"+sub.ID+"/cancel", nil, &canceled)
	if canceled["status"] != "canceled" || canceled["canceled_at"] == nil {
		t.Fatalf("cancel: %+v", canceled)
	}
	// One the release before migration 116 canceled, while both served,
	// still reads as canceled.
	var tenant string
	if err := d.Pool.QueryRow(context.Background(), `SELECT m.tenant_id::text FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE u.email = 'ada@example.com'`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if err := d.InTenant(context.Background(), tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE subscriptions SET status = 'cancelled' WHERE id = $1`, sub.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	c.do("GET", "/api/v1/subscriptions?agent=claude-code", nil, &list)
	if len(list.Subscriptions) != 1 || list.Subscriptions[0]["status"] != "canceled" {
		t.Fatalf("list after an old cancel = %v", list)
	}
}

func TestSendTestEvent(t *testing.T) {
	_, c := setup(t)
	c.signIn()
	if code := c.do("POST", "/api/v1/events/test", map[string]any{"type": "app.agentchat.channel.x", "subject": "forged"}, nil); code != 400 {
		t.Fatalf("non-test type: %d", code)
	}
	var e struct {
		Seq     int
		Type    string
		Trust   string
		Subject string
	}
	if code := c.do("POST", "/api/v1/events/test", map[string]any{"type": "test.ping", "subject": "hello", "attrs": map[string]string{"channel": "dev"}}, &e); code != 200 || e.Seq != 1 || e.Trust != "internal" {
		t.Fatalf("test event: %d %+v", code, e)
	}
	var list struct{ Events []map[string]any }
	c.do("GET", "/api/v1/events", nil, &list)
	if len(list.Events) != 1 || list.Events[0]["subject"] != "hello" {
		t.Fatalf("events = %+v", list)
	}
}

// Signing out ends the session everywhere: a copy of the cookie stops
// working too.
func TestSignOutEndsCopiedSessions(t *testing.T) {
	_, c := setup(t)
	c.signIn()
	u, _ := http.NewRequest("GET", c.base, nil)
	copied := newClient(t, c.base)
	copied.http.Jar.SetCookies(u.URL, c.http.Jar.Cookies(u.URL))
	if code := copied.do("GET", "/api/v1/agents", nil, nil); code != 200 {
		t.Fatalf("copied cookie before sign-out: %d", code)
	}
	c.do("DELETE", "/api/v1/session", nil, nil)
	if code := copied.do("GET", "/api/v1/agents", nil, nil); code != 401 {
		t.Fatalf("copied cookie after sign-out: %d", code)
	}
	c.signIn()
	if code := c.do("GET", "/api/v1/agents", nil, nil); code != 200 {
		t.Fatalf("signing in again: %d", code)
	}
}

func TestRevokeStreamLinks(t *testing.T) {
	_, c := setup(t)
	c.signIn()
	c.do("POST", "/api/v1/agents", map[string]string{"id": "claude-code"}, nil)
	c.do("POST", "/api/v1/agents/claude-code/stream-link", map[string]bool{}, nil)
	var out struct{ Revoked int }
	if code := c.do("POST", "/api/v1/agents/claude-code/stream-links/revoke", nil, &out); code != 200 || out.Revoked != 1 {
		t.Fatalf("revoke: %d %+v", code, out)
	}
}

func TestRequestValidation(t *testing.T) {
	_, c := setup(t)
	c.signIn()
	for _, tc := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"POST", "/api/v1/subscriptions/not-a-uuid/cancel", nil, 404},
		{"GET", "/api/v1/events?before=abc", nil, 400},
		{"POST", "/api/v1/events/test", map[string]string{"type": "test."}, 400},
		{"POST", "/api/v1/events/test", map[string]string{"type": "Test.Ping"}, 400},
		{"POST", "/api/v1/events/test", map[string]string{"type": "test.ping\r"}, 400},
		{"POST", "/api/v1/events/test", map[string]string{"type": "test.ping", "subject": strings.Repeat("x", 70<<10)}, 413},
		{"POST", "/api/v1/agents", map[string]string{"id": "ok", "extra": "field"}, 400},
	} {
		if code := c.do(tc.method, tc.path, tc.body, nil); code != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, code, tc.want)
		}
	}
	// Uppercase event types are rejected (matching is case-sensitive).
	c.do("POST", "/api/v1/agents", map[string]string{"id": "claude-code"}, nil)
	if code := c.do("POST", "/api/v1/subscriptions", map[string]any{"agent_id": "claude-code", "event_type": "Slack.Message"}, nil); code != 400 {
		t.Errorf("uppercase type: %d", code)
	}
}
