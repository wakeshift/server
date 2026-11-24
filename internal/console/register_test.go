package console_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// cliKey is a key as @wakeshift/cli makes it: a seed on the developer's
// machine, written as a wst_ key, and the public key in the link.
type cliKey struct {
	key string // the wst_ key, which stays in .env
	pub ed25519.PublicKey
}

func newCLIKey(t *testing.T) cliKey {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	return cliKey{key: appkey.Format(seed), pub: ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)}
}

func (k cliKey) link() string { return base64.RawURLEncoding.EncodeToString(k.pub) }

// code is the key code the console and the CLI both show.
func (k cliKey) code() string { return appkey.KeyID(k.pub)[:8] }

type registered struct {
	App struct {
		ID, Name string
		OrgID    string `json:"org_id"`
		Keys     int
	}
	KeyInfo struct {
		ID   string `json:"key_id"`
		Name string
	} `json:"key_info"`
	Error struct{ Code, Message string }
}

func register(u *coretest.User, body map[string]string) (int, registered) {
	var out registered
	code := u.Do("POST", "/api/v1/developer/apps/register", typed(body), &out)
	return code, out
}

func registerKey(u *coretest.User, app string, body map[string]string) (int, registered) {
	var out registered
	code := u.Do("POST", "/api/v1/developer/apps/"+app+"/keys/register", typed(body), &out)
	return code, out
}

// typed adds the key code the person types from their terminal, for the
// body's public key, unless the body has a key_code already.
func typed(body map[string]string) map[string]string {
	if _, ok := body["key_code"]; ok {
		return body
	}
	if pub, err := base64.RawURLEncoding.DecodeString(body["public_key"]); err == nil && len(pub) == ed25519.PublicKeySize {
		body["key_code"] = appkey.KeyID(pub)[:8]
	}
	return body
}

// The confirm page creates an app whose only key is the one the CLI made:
// the key works at once for the app API, by the kid in its assertions
// (GET /api/apps/v1/app, which the CLI polls), nothing secret comes back,
// and the app's log and the person's own audit log say it came from the
// CLI.
func TestRegisterAppFromPublicKey(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	k := newCLIKey(t)

	// Before the person confirms, the CLI's poll is refused.
	if _, err := coretest.KeyClient(t, k.key, c.URL).Info(ctx); !wakeshift.IsCode(err, "unauthenticated") {
		t.Fatalf("polling before the app exists: %v", err)
	}

	var raw json.RawMessage
	if code := ada.Do("POST", "/api/v1/developer/apps/register", map[string]string{"id": "acme-ci-dev", "name": "Acme CI", "public_key": k.link(), "key_code": k.code()}, &raw); code != 200 {
		t.Fatalf("register: %d %s", code, raw)
	}
	if strings.Contains(string(raw), "wst_") || strings.Contains(string(raw), `"key"`) {
		t.Fatalf("the response carries a key: %s", raw)
	}
	var out registered
	json.Unmarshal(raw, &out)
	if out.App.ID != "acme-ci-dev" || out.App.Name != "Acme CI" || out.App.Keys != 1 || out.KeyInfo.ID != k.code() || out.KeyInfo.Name != "Default" {
		t.Fatalf("registered %s", raw)
	}

	// The CLI's poll now returns the app, with the key code it showed.
	app := coretest.KeyClient(t, k.key, c.URL)
	if id, err := app.AppID(ctx); err != nil || id != "acme-ci-dev" {
		t.Fatalf("polling after: %q %v", id, err)
	}
	info, err := app.Info(ctx)
	if err != nil || info.Key.KeyID != k.code() {
		t.Fatalf("info: %+v %v", info, err)
	}
	// The keys list shows it as the console shows every key.
	var keys struct {
		Keys []struct {
			ID   string `json:"key_id"`
			Name string
		}
	}
	ada.Do("GET", "/api/v1/developer/apps/acme-ci-dev/keys", nil, &keys)
	if len(keys.Keys) != 1 || keys.Keys[0].ID != k.code() {
		t.Fatalf("keys: %+v", keys)
	}

	// The app's log: created and key_created, each with via cli and the
	// key's ID.
	var log struct {
		Entries []struct {
			Action string
			KeyID  string `json:"key_id"`
			Detail map[string]any
		}
	}
	ada.Do("GET", "/api/v1/developer/apps/acme-ci-dev/log", nil, &log)
	got := map[string]bool{}
	for _, e := range log.Entries {
		if e.Detail["via"] == "cli" && e.KeyID == k.code() {
			got[e.Action] = true
		}
	}
	// (And key_first_used, from the CLI's poll.)
	if len(log.Entries) != 3 || !got[apps.LogCreated] || !got[apps.LogKeyCreated] || log.Entries[0].Action != apps.LogKeyFirstUsed {
		t.Fatalf("log: %+v", log.Entries)
	}

	// The person's own audit log has the registration, in their personal
	// workspace only: bob, in his, sees nothing of it.
	type auditPage struct {
		Entries []struct {
			Actor, Action string
			AppID         string `json:"app_id"`
			Detail        map[string]any
		}
	}
	var mine, his auditPage
	ada.Do("GET", "/api/v1/audit", nil, &mine)
	found := false
	for _, e := range mine.Entries {
		if e.Action == "app.key_registered" && e.AppID == "acme-ci-dev" && e.Detail["key_id"] == k.code() && e.Detail["via"] == "cli" && e.Detail["new_app"] == true {
			found = true
		}
	}
	if !found {
		t.Fatalf("ada's audit log: %+v", mine.Entries)
	}
	bob := c.SignIn("bob@example.com")
	bob.Do("GET", "/api/v1/audit", nil, &his)
	for _, e := range his.Entries {
		if e.Action == "app.key_registered" {
			t.Fatalf("bob sees %+v", e)
		}
	}
	// Row-level security: the entry is in ada's workspace, and reads as
	// absent outside any workspace.
	var tenant string
	if err := c.DB.InTenant(ctx, ada.Workspace(t), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT tenant_id::text FROM audit_log WHERE action = 'app.key_registered'`).Scan(&tenant)
	}); err != nil || tenant != ada.Workspace(t) {
		t.Fatalf("in ada's workspace: %q %v", tenant, err)
	}
	var outside int
	if err := c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'app.key_registered'`).Scan(&outside); err != nil || outside != 0 {
		t.Fatalf("outside any workspace: %d %v", outside, err)
	}
	if err := c.DB.InTenant(ctx, bob.Workspace(t), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'app.key_registered'`).Scan(&outside)
	}); err != nil || outside != 0 {
		t.Fatalf("in bob's workspace: %d %v", outside, err)
	}
}

// A bad public key, or one already registered anywhere, is refused with its
// own code, and nothing is made: the app's ID stays free.
func TestRegisterRefusesBadAndUsedKeys(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	bob := c.SignIn("bob@example.com")
	k := newCLIKey(t)
	for name, pk := range map[string]string{
		"empty":       "",
		"short":       base64.RawURLEncoding.EncodeToString(k.pub[:31]),
		"long":        base64.RawURLEncoding.EncodeToString(append(append([]byte{}, k.pub...), 1)),
		"padded":      base64.URLEncoding.EncodeToString(k.pub),
		"small order": base64.RawURLEncoding.EncodeToString(append([]byte{1}, make([]byte, 31)...)),
		"the key":     k.key,
	} {
		if code, out := register(ada, map[string]string{"id": "acme-dev", "name": "Acme", "public_key": pk}); code != 400 || out.Error.Code != "invalid_key" {
			t.Errorf("%s: %d %+v", name, code, out.Error)
		}
	}
	if code := ada.Do("GET", "/api/v1/developer/apps/acme-dev", nil, nil); code != 404 {
		t.Fatalf("a refused registration made the app: %d", code)
	}

	if code, out := register(ada, map[string]string{"id": "acme-dev", "name": "Acme", "public_key": k.link()}); code != 200 {
		t.Fatalf("register: %d %+v", code, out.Error)
	}
	// The same key again, for another app: by ada, or by bob in his own
	// organization; or added to an app as one more key.
	for _, u := range []*coretest.User{ada, bob} {
		if code, out := register(u, map[string]string{"id": "other-dev", "name": "Other", "public_key": k.link()}); code != 409 || out.Error.Code != "key_used" {
			t.Errorf("a used key: %d %+v", code, out.Error)
		}
	}
	if code, out := registerKey(ada, "acme-dev", map[string]string{"public_key": k.link()}); code != 409 || out.Error.Code != "key_used" {
		t.Errorf("a used key as one more key: %d %+v", code, out.Error)
	}
	createApp(t, bob, "bobs", "Bob's", "")
	if code, out := registerKey(bob, "bobs", map[string]string{"public_key": k.link()}); code != 409 || out.Error.Code != "key_used" {
		t.Errorf("ada's key on bob's app: %d %+v", code, out.Error)
	}
	for _, id := range []string{"other-dev"} {
		if code := ada.Do("GET", "/api/v1/developer/apps/"+id, nil, nil); code != 404 {
			t.Errorf("%s exists: %d", id, code)
		}
	}
	// A revoked key's kid stays used: revoking and registering it again
	// doesn't revive it.
	if code := ada.Do("POST", "/api/v1/developer/apps/acme-dev/keys/"+k.code()+"/revoke", nil, nil); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if code, out := registerKey(ada, "acme-dev", map[string]string{"public_key": k.link()}); code != 409 || out.Error.Code != "key_used" {
		t.Errorf("a revoked key again: %d %+v", code, out.Error)
	}
	// Deleting the app deletes its keys but keeps each kid's fingerprint:
	// its key stays used, for a new app or as one more key, by anyone.
	if code := ada.Do("DELETE", "/api/v1/developer/apps/acme-dev", nil, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	var rows, tombstoned int
	c.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM app_keys WHERE app_id = 'acme-dev'`).Scan(&rows)
	c.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM deleted_key_ids WHERE kid_hash = sha256(convert_to($1::text, 'UTF8'))`, appkey.KeyID(k.pub)).Scan(&tombstoned)
	if rows != 0 || tombstoned != 1 {
		t.Errorf("after deleting the app: %d keys (want 0), the kid's fingerprint %d times (want 1)", rows, tombstoned)
	}
	for _, u := range []*coretest.User{ada, bob} {
		if code, out := register(u, map[string]string{"id": "after-dev", "name": "After", "public_key": k.link()}); code != 409 || out.Error.Code != "key_used" {
			t.Errorf("a deleted app's key for a new app: %d %+v", code, out.Error)
		}
	}
	if code, out := registerKey(bob, "bobs", map[string]string{"public_key": k.link()}); code != 409 || out.Error.Code != "key_used" {
		t.Errorf("a deleted app's key as one more key: %d %+v", code, out.Error)
	}
	// The app's other fields are checked as for any new app.
	if code, out := register(ada, map[string]string{"id": "Bad ID", "name": "X", "public_key": newCLIKey(t).link()}); code != 400 || out.Error.Code != "bad_request" {
		t.Errorf("a bad ID: %d %+v", code, out.Error)
	}
	if code, out := register(ada, map[string]string{"id": "acme-dev", "name": "Again", "public_key": newCLIKey(t).link()}); code != 409 || out.Error.Code != "exists" { // deleted, and not reused
		t.Errorf("a taken ID: %d %+v", code, out.Error)
	}
}

// Adding a key from the CLI to an existing app: the key works at once
// beside the others, its log entry says via cli, and only owners and admins
// may (TestLinkRegistrationGuards: a developer gets 403); a non-member gets
// 404, as for no such app.
func TestRegisterKeyForAnApp(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	cy := c.SignIn("cy@example.com")
	eve := c.SignIn("eve@example.com")
	org := orgOf(t, ada)
	join(t, c, org, cy, "admin")
	made := createApp(t, ada, "quizzer", "Quizzer", "")

	k := newCLIKey(t)
	if code, out := registerKey(eve, "quizzer", map[string]string{"public_key": k.link(), "name": "Eve's"}); code != 404 || out.Error.Code != "not_found" {
		t.Fatalf("a non-member: %d %+v", code, out.Error)
	}
	if code, out := registerKey(ada, "no-such-app", map[string]string{"public_key": k.link()}); code != 404 {
		t.Fatalf("no such app: %d %+v", code, out.Error)
	}
	if code, out := register(eve, map[string]string{"org_id": org, "id": "eves-dev", "name": "Eve's", "public_key": k.link()}); code != 404 || out.Error.Code != "not_found" {
		t.Fatalf("a non-member's app in the organization: %d %+v", code, out.Error)
	}
	// An admin adds a link's key.
	code, out := registerKey(cy, "quizzer", map[string]string{"public_key": k.link(), "name": "Cy's laptop"})
	if code != 200 || out.KeyInfo.ID != k.code() || out.KeyInfo.Name != "Cy's laptop" {
		t.Fatalf("an admin: %d %+v", code, out)
	}
	if id, err := coretest.KeyClient(t, k.key, c.URL).AppID(ctx); err != nil || id != "quizzer" {
		t.Fatalf("the added key: %q %v", id, err)
	}
	if err := publish(kit(t, c, made.Key, "")); err != nil {
		t.Fatalf("the first key still works: %v", err)
	}
	var entries int
	if err := c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM app_log WHERE app_id = 'quizzer' AND action = 'app.key_created' AND key_id = $1
		AND detail->>'via' = 'cli' AND actor = 'user:' || $2`, k.code(), userID(t, cy)).Scan(&entries); err != nil || entries != 1 {
		t.Fatalf("log entries: %d %v", entries, err)
	}
	// Recorded in cy's own audit log, not ada's.
	var page struct{ Entries []struct{ Action string } }
	ada.Do("GET", "/api/v1/audit", nil, &page)
	for _, e := range page.Entries {
		if e.Action == "app.key_registered" {
			t.Fatal("cy's registration is in ada's audit log")
		}
	}
	cy.Do("GET", "/api/v1/audit", nil, &page)
	if len(page.Entries) == 0 || page.Entries[0].Action != "app.key_registered" {
		t.Fatalf("cy's audit log: %+v", page.Entries)
	}
	// A deleted app takes no keys.
	if code := ada.Do("DELETE", "/api/v1/developer/apps/quizzer", nil, nil); code != 200 {
		t.Fatal("delete")
	}
	if code, out := registerKey(ada, "quizzer", map[string]string{"public_key": newCLIKey(t).link()}); code != 409 || out.Error.Code != "deleted" {
		t.Fatalf("a deleted app: %d %+v", code, out.Error)
	}
}

// The abuse limits: apps an account creates an hour and a day (console and
// CLI alike), keys made for one app a day, and public keys an account
// registers an hour. Each is refused with 429 rate_limited and a message
// saying which.
func TestRegisterRateLimits(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	limited := func(what string, code int, e struct{ Code, Message string }) {
		t.Helper()
		if code != 429 || e.Code != "rate_limited" || e.Message == "" {
			t.Fatalf("%s: %d %+v", what, code, e)
		}
	}

	// Apps an hour: the console's and the CLI's count together.
	for i := range apps.MaxAppsPerHour {
		id := "app-" + string(rune('a'+i))
		if i%2 == 0 {
			createApp(t, ada, id, "App", "")
		} else if code, out := register(ada, map[string]string{"id": id, "name": "App", "public_key": newCLIKey(t).link()}); code != 200 {
			t.Fatalf("app %d: %d %+v", i, code, out.Error)
		}
	}
	code, out := register(ada, map[string]string{"id": "one-more", "name": "App", "public_key": newCLIKey(t).link()})
	limited("an app past the hour's limit", code, out.Error)
	var e apiError
	limited("a console app past the hour's limit", ada.Do("POST", "/api/v1/developer/apps", map[string]string{"id": "one-more", "name": "App"}, &e), e.Error)
	// Another person isn't limited by ada's.
	bob := c.SignIn("bob@example.com")
	createApp(t, bob, "bobs", "Bob's", "")

	// Apps a day: ada's apps from earlier today (deleted since, so the
	// organization's cap isn't what refuses).
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE apps SET created_at = now() - interval '2 hours' WHERE created_by = (SELECT id FROM users WHERE email = 'ada@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO apps (id, name, org_id, created_by, created_at, deleted_at)
		SELECT 'old-' || g, 'Old', org_id, created_by, now() - interval '3 hours', now() FROM apps, generate_series(1, $1) g WHERE id = 'app-a'`,
		apps.MaxAppsPerDay-apps.MaxAppsPerHour); err != nil {
		t.Fatal(err)
	}
	code, out = register(ada, map[string]string{"id": "one-more", "name": "App", "public_key": newCLIKey(t).link()})
	limited("an app past the day's limit", code, out.Error)
	if !strings.Contains(out.Error.Message, "a day") {
		t.Fatalf("the day's message: %q", out.Error.Message)
	}
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE apps SET created_at = now() - interval '25 hours' WHERE id LIKE 'old-%'`); err != nil {
		t.Fatal(err)
	}

	// Public keys registered an hour: app-b and app-d's, then keys added
	// up to the limit; the next is refused, whether a key or an app.
	for range apps.MaxRegistrationsPerHour - 2 {
		if code, out := registerKey(ada, "app-a", map[string]string{"public_key": newCLIKey(t).link()}); code != 200 {
			t.Fatalf("a key: %d %+v", code, out.Error)
		}
	}
	code, out = registerKey(ada, "app-c", map[string]string{"public_key": newCLIKey(t).link()})
	limited("a key past the registrations' limit", code, out.Error)
	if !strings.Contains(out.Error.Message, "command line") {
		t.Fatalf("the registrations' message: %q", out.Error.Message)
	}
	// Keys made in the console aren't registrations.
	if code := ada.Do("POST", "/api/v1/developer/apps/app-c/keys", map[string]string{"name": "Console"}, nil); code != 200 {
		t.Fatalf("a console key: %d", code)
	}
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE app_keys SET created_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}

	// Keys made for one app a day, however they're made.
	if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO app_keys (app_id, key_id, kid, name, public_key, created_by, revoked_at)
		SELECT 'app-e', 'old' || g, 'old-kid-' || g, 'Old', sha256(g::text::bytea), 'user:x', now() FROM generate_series(1, $1) g`,
		apps.MaxKeysPerDay-1); err != nil {
		t.Fatal(err)
	}
	limited("a console key past the day's limit", ada.Do("POST", "/api/v1/developer/apps/app-e/keys", map[string]string{"name": "More"}, &e), e.Error)
	code, out = registerKey(ada, "app-e", map[string]string{"public_key": newCLIKey(t).link()})
	limited("a registered key past the day's limit", code, out.Error)
	var keys struct {
		Keys []struct {
			ID string `json:"key_id"`
		}
	}
	ada.Do("GET", "/api/v1/developer/apps/app-e/keys", nil, &keys)
	var live string
	for _, k := range keys.Keys {
		if !strings.HasPrefix(k.ID, "old") {
			live = k.ID
		}
	}
	limited("rotating past the day's limit", ada.Do("POST", "/api/v1/developer/apps/app-e/keys/"+live+"/rotate", nil, &e), e.Error)
	// Another app isn't limited by app-e's.
	if code := ada.Do("POST", "/api/v1/developer/apps/app-b/keys", map[string]string{"name": "More"}, nil); code != 200 {
		t.Fatalf("another app's key: %d", code)
	}
}
