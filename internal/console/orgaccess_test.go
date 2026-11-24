package console_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/coretest"
)

// orgOf is the organization the developer API lists for u by default:
// their personal one.
func orgOf(t *testing.T, u *coretest.User) string {
	t.Helper()
	var out struct {
		OrgID string `json:"org_id"`
	}
	if code := u.Do("GET", "/api/v1/developer/apps", nil, &out); code != 200 || out.OrgID == "" {
		t.Fatalf("default organization: %d %+v", code, out)
	}
	return out.OrgID
}

// userID is u's user ID.
func userID(t *testing.T, u *coretest.User) string {
	t.Helper()
	var s struct{ User struct{ ID string } }
	u.Do("GET", "/api/v1/session", nil, &s)
	if s.User.ID == "" {
		t.Fatal("no user")
	}
	return s.User.ID
}

// join makes u a member of org with role, as an accepted invite would.
func join(t *testing.T, c *coretest.Core, org string, u *coretest.User, role string) {
	t.Helper()
	if _, err := c.DB.Pool.Exec(context.Background(), `INSERT INTO organization_members (org_id, user_id, role) VALUES ($1, $2, $3)
		ON CONFLICT (org_id, user_id) DO UPDATE SET role = $3`, org, userID(t, u), role); err != nil {
		t.Fatal(err)
	}
}

type apiError struct {
	Error struct{ Code, Message string }
}

// Every route of the developer API, as each role of the app's organization
// and a non-member. A non-member gets 404 on every route, exactly as for
// an app that doesn't exist; a member whose role doesn't allow the action
// gets 403 forbidden.
func TestDeveloperRoutesByRole(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com") // the owner
	bob := c.SignIn("bob@example.com") // an admin
	cy := c.SignIn("cy@example.com")   // a developer
	eve := c.SignIn("eve@example.com") // not a member
	org := orgOf(t, ada)
	orgOf(t, eve)
	join(t, c, org, bob, "admin")
	join(t, c, org, cy, "developer")
	made := createApp(t, ada, "quizzer", "Quizzer", "")
	app := noted(t, c, made.Key)
	ctx := context.Background()
	if err := publish(app); err != nil {
		t.Fatal(err)
	}
	sub, err := coretest.Subscribe(app, coretest.Caller{User: "player-1", Client: "claude-code"}, "game/1", coretest.SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	pubURL, err := coretest.NotePublishURL(app, "game/1")
	if err != nil {
		t.Fatal(err)
	}

	const (
		all   = "owner admin developer"
		admin = "owner admin"
	)
	p := "/api/v1/developer/apps/quizzer"
	// In order: each change is allowed for some role, so later rows still
	// find what they need (a fresh key to revoke, say). Deleting is last.
	routes := []struct {
		method, path string
		body         func(role string) string
		allowed      string
	}{
		{"GET", "/api/v1/developer/apps?org=" + org, nil, all},
		{"POST", "/api/v1/developer/apps", func(role string) string {
			return `{"org_id": "` + org + `", "id": "by-` + role + `", "name": "By ` + role + `"}`
		}, all},
		{"POST", "/api/v1/developer/apps/register", func(role string) string {
			k := newCLIKey(t)
			return `{"org_id": "` + org + `", "id": "cli-by-` + role + `", "name": "CLI by ` + role + `", "public_key": "` + k.link() + `", "key_code": "` + k.code() + `"}`
		}, all},
		{"GET", p, nil, all},
		{"PATCH", p, func(role string) string { return `{"name": "Renamed by ` + role + `"}` }, admin},
		{"GET", p + "/keys", nil, all},
		{"POST", p + "/keys", func(role string) string { return `{"name": "` + role + `"}` }, all},
		// Named as the keys the next rows revoke before each try.
		{"POST", p + "/keys/register", func(role string) string {
			k := newCLIKey(t)
			return `{"name": "to try", "public_key": "` + k.link() + `", "key_code": "` + k.code() + `"}`
		}, admin},
		{"PATCH", p + "/keys/{key}", func(role string) string { return `{"name": "renamed by ` + role + `"}` }, all},
		{"POST", p + "/keys/{key}/rotate", nil, all},
		{"POST", p + "/keys/{key}/revoke", nil, all},
		{"GET", p + "/log", nil, all},
		{"GET", p + "/overview", nil, all},
		{"GET", p + "/subscriptions", nil, all},
		{"GET", p + "/activity", nil, all},
		{"GET", p + "/users", nil, all},
		{"GET", p + "/publish-urls", nil, all},
		{"POST", p + "/publish-urls", func(string) string { return `{"event": "note.two", "arguments": {"a": "game", "b": "2"}}` }, all},
		{"POST", p + "/publish-urls/" + pubURL.ID + "/revoke", nil, all},
		{"POST", p + "/subscriptions/" + sub.ID + "/end", nil, all},
		{"POST", p + "/users/forget", func(string) string { return `{"user": "player-1"}` }, admin},
		{"DELETE", p, nil, admin},
	}
	members := []struct {
		role string
		u    *coretest.User
	}{{"developer", cy}, {"admin", bob}, {"owner", ada}}
	for _, r := range routes {
		body := func(role string) any {
			if r.body == nil {
				return nil
			}
			var v any
			json.Unmarshal([]byte(r.body(role)), &v)
			return v
		}
		// Rotating and revoking need a key that's still live: a new one
		// for each try.
		path := func() string {
			if !strings.Contains(r.path, "{key}") {
				return r.path
			}
			// The last tries' keys go first: an app has at most 10 live
			// keys.
			if _, err := c.DB.Pool.Exec(ctx, `UPDATE app_keys SET revoked_at = now() WHERE app_id = 'quizzer' AND name = 'to try' AND revoked_at IS NULL`); err != nil {
				t.Fatal(err)
			}
			var k madeApp
			if code := ada.Do("POST", p+"/keys", map[string]string{"name": "to try"}, &k); code != 200 {
				t.Fatalf("a key: %d", code)
			}
			return strings.Replace(r.path, "{key}", k.KeyInfo.ID, 1)
		}
		var e apiError
		if code := eve.Do(r.method, r.path, body("eve"), &e); code != 404 || e.Error.Code != "not_found" {
			t.Errorf("a non-member: %s %s: %d %+v", r.method, r.path, code, e)
		}
		for _, m := range members {
			var e apiError
			code := m.u.Do(r.method, path(), body(m.role), &e)
			switch ok := strings.Contains(r.allowed, m.role); {
			case !ok && (code != 403 || e.Error.Code != "forbidden"):
				t.Errorf("a %s: %s %s: %d %+v; want 403 forbidden", m.role, r.method, r.path, code, e)
			case ok && code != 200:
				t.Errorf("a %s: %s %s: %d %+v; want 200", m.role, r.method, r.path, code, e)
			}
		}
	}

	// Keys: every member makes, rotates and revokes them (a leaked key is
	// revoked at once, without waiting for an admin).
	for _, m := range members {
		var k madeApp
		if code := m.u.Do("POST", "/api/v1/developer/apps/by-owner/keys", map[string]string{"name": "for " + m.role}, &k); code != 200 {
			t.Fatalf("a %s's key: %d", m.role, code)
		}
		rotated := rotate(t, m.u, "by-owner", k.KeyInfo.ID)
		if code := m.u.Do("POST", "/api/v1/developer/apps/by-owner/keys/"+rotated.KeyInfo.ID+"/revoke", nil, nil); code != 200 {
			t.Errorf("a %s revoking: %d", m.role, code)
		}
		for _, path := range []string{"/keys/" + k.KeyInfo.ID + "/rotate", "/keys/" + rotated.KeyInfo.ID + "/revoke"} {
			if code := eve.Do("POST", "/api/v1/developer/apps/by-owner"+path, nil, nil); code != 404 {
				t.Errorf("a non-member: %s: %d", path, code)
			}
		}
	}

	// A non-member's 404 is the same as for an app that doesn't exist, on
	// the app's routes and on the organization's.
	var mine, none json.RawMessage
	eve.Do("GET", "/api/v1/developer/apps/by-owner", nil, &mine)
	ada.Do("GET", "/api/v1/developer/apps/no-such-app", nil, &none)
	if string(mine) != string(none) {
		t.Errorf("a non-member's 404 %s differs from a missing app's %s", mine, none)
	}
	eve.Do("GET", "/api/v1/developer/apps?org="+org, nil, &mine)
	ada.Do("GET", "/api/v1/developer/apps?org=00000000-0000-0000-0000-000000000000", nil, &none)
	if string(mine) != string(none) {
		t.Errorf("a non-member's organization 404 %s differs from a missing one's %s", mine, none)
	}
	var e apiError
	if code := ada.Do("GET", "/api/v1/developer/apps?org=not-a-uuid", nil, &e); code != 404 {
		t.Errorf("a malformed organization: %d %+v", code, e)
	}

	// A deleted app still refuses changes as before (409), whoever asks.
	for _, m := range members {
		var e apiError
		if code := m.u.Do("POST", p+"/keys", map[string]string{"name": "late"}, &e); code != 409 && code != 403 {
			t.Errorf("a %s, a deleted app's key: %d %+v", m.role, code, e)
		}
	}
	if code := ada.Do("POST", p+"/keys", map[string]string{"name": "late"}, &e); code != 409 || e.Error.Code != "deleted" {
		t.Errorf("the owner, a deleted app's key: %d %+v", code, e)
	}

	// The app log names who did what, and every entry carries the app's
	// organization.
	var bad int
	if err := c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM app_log WHERE app_id IN ('quizzer', 'by-owner') AND org_id IS DISTINCT FROM $1`, org).Scan(&bad); err != nil || bad != 0 {
		t.Errorf("app log entries without the organization: %d %v", bad, err)
	}
	var created int
	if err := c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM apps WHERE id LIKE 'by-%' AND org_id = $1 AND created_by IS NOT NULL`, org).Scan(&created); err != nil || created != 3 {
		t.Errorf("apps made by members: %d %v", created, err)
	}
}

// Membership is read on every request: a removed member's next request is
// 404, and a changed role applies at once, open console sessions included.
func TestMembershipAppliesOnTheNextRequest(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	cy := c.SignIn("cy@example.com")
	org := orgOf(t, ada)
	createApp(t, ada, "shared", "Shared", "")
	join(t, c, org, cy, "developer")
	if code := cy.Do("GET", "/api/v1/developer/apps/shared", nil, nil); code != 200 {
		t.Fatalf("a developer reading: %d", code)
	}
	if code := cy.Do("PATCH", "/api/v1/developer/apps/shared", map[string]string{"name": "Mine"}, nil); code != 403 {
		t.Fatalf("a developer renaming: %d", code)
	}
	join(t, c, org, cy, "admin")
	if code := cy.Do("PATCH", "/api/v1/developer/apps/shared", map[string]string{"name": "Ours"}, nil); code != 200 {
		t.Fatalf("an admin renaming: %d", code)
	}
	if _, err := c.DB.Pool.Exec(context.Background(), `DELETE FROM organization_members WHERE org_id = $1 AND user_id = $2`, org, userID(t, cy)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/developer/apps/shared", "/api/v1/developer/apps/shared/log", "/api/v1/developer/apps?org=" + org} {
		if code := cy.Do("GET", path, nil, nil); code != 404 {
			t.Errorf("removed, %s: %d", path, code)
		}
	}
	// Their own console still works: their workspace is untouched.
	if code := cy.Do("GET", "/api/v1/agents", nil, nil); code != 200 {
		t.Errorf("removed, their own workspace: %d", code)
	}
	// A deleted organization's apps are no one's.
	if _, err := c.DB.Pool.Exec(context.Background(), `UPDATE organizations SET deleted_at = now() WHERE id = $1`, org); err != nil {
		t.Fatal(err)
	}
	if code := ada.Do("GET", "/api/v1/developer/apps/shared", nil, nil); code != 404 {
		t.Errorf("a deleted organization's app: %d", code)
	}
}
