package apps_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/internal/orgs"
)

var ctx = context.Background()

func setup(t *testing.T) (*apps.Registry, ed25519.PrivateKey) {
	t.Helper()
	r, priv, _ := setupDB(t)
	return r, priv
}

func setupDB(t *testing.T) (*apps.Registry, ed25519.PrivateKey, *db.DB) {
	t.Helper()
	d := dbtest.New(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r := &apps.Registry{Pool: d.Pool, Issuer: "https://wakeshift.test"}
	if err := r.Sync(ctx, []apps.Config{{ID: "agentchat", Name: "Agent Chat", MCPURL: "https://chat.test/mcp", PublicKey: base64.StdEncoding.EncodeToString(pub)}}); err != nil {
		t.Fatal(err)
	}
	return r, priv, d
}

func assertion(key ed25519.PrivateKey, mutate func(*apps.Assertion)) string {
	now := time.Now()
	a := apps.Assertion{Issuer: "agentchat", Subject: "agentchat", Audience: "https://wakeshift.test/api/apps",
		IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix(), ID: base64.RawURLEncoding.EncodeToString([]byte(now.String()))}
	if mutate != nil {
		mutate(&a)
	}
	tok, _ := jws.Sign(key, "", "app+jwt", a)
	return tok
}

func TestAssertions(t *testing.T) {
	r, key := setup(t)
	good := assertion(key, nil)
	if app, err := r.Authenticate(ctx, good, time.Now()); err != nil || app.ID != "agentchat" {
		t.Fatalf("good assertion: %v", err)
	}
	if _, err := r.Authenticate(ctx, good, time.Now()); err == nil {
		t.Fatal("a replayed assertion was accepted")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	for name, tok := range map[string]string{
		"another key":    assertion(other, nil),
		"wrong audience": assertion(key, func(a *apps.Assertion) { a.Audience = "https://elsewhere/api/apps"; a.ID = "x1" }),
		"expired":        assertion(key, func(a *apps.Assertion) { a.Expires = time.Now().Add(-time.Second).Unix(); a.ID = "x2" }),
		"too long-lived": assertion(key, func(a *apps.Assertion) { a.Expires = time.Now().Add(time.Hour).Unix(); a.ID = "x3" }),
		"unknown app":    assertion(key, func(a *apps.Assertion) { a.Issuer, a.Subject, a.ID = "nope", "nope", "x4" }),
		"no jti":         assertion(key, func(a *apps.Assertion) { a.ID = "" }),
		"garbage":        "a.b.c",
	} {
		if _, err := r.Authenticate(ctx, tok, time.Now()); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !errors.Is(err, apps.ErrUnauthenticated) {
			t.Errorf("%s: %v isn't a refusal", name, err)
		}
	}
	// An app that doesn't exist reads as a bad signature: the refusal (sent
	// back to the caller) doesn't say which app IDs exist.
	_, unknown := r.Authenticate(ctx, assertion(key, func(a *apps.Assertion) { a.Issuer, a.Subject, a.ID = "nope", "nope", "x5" }), time.Now())
	_, badKey := r.Authenticate(ctx, assertion(other, func(a *apps.Assertion) { a.ID = "x6" }), time.Now())
	if unknown == nil || badKey == nil || unknown.Error() != badKey.Error() {
		t.Fatalf("unknown app %v, another key %v", unknown, badKey)
	}
}

// A replay is refused by every instance, not only the one that saw the
// assertion first, and expired IDs are forgotten.
func TestAssertionReplayAcrossInstances(t *testing.T) {
	r, key := setup(t)
	other := &apps.Registry{Pool: r.Pool, Issuer: r.Issuer}
	tok := assertion(key, func(a *apps.Assertion) { a.ID = "cross-instance" })
	if _, err := r.Authenticate(ctx, tok, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Authenticate(ctx, tok, time.Now()); !errors.Is(err, apps.ErrUnauthenticated) {
		t.Fatalf("a replay on another instance: %v", err)
	}
	if _, err := r.Pool.Exec(ctx, `UPDATE app_assertion_ids SET expires_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if n, err := apps.DeleteExpiredAssertionIDs(ctx, r.Pool, 100); err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
}

// A failure reading the app isn't a refusal (callers answer 500, not 401
// with the database's words), and neither is a manifest the database
// couldn't store.
func TestInternalErrorsArentRefusals(t *testing.T) {
	_, url := dbtest.NewWithURL(t)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r := &apps.Registry{Pool: pool, Issuer: "https://wakeshift.test"}
	if err := r.Sync(ctx, []apps.Config{{ID: "agentchat", Name: "Agent Chat", MCPURL: "https://chat.test/mcp", PublicKey: base64.StdEncoding.EncodeToString(pub)}}); err != nil {
		t.Fatal(err)
	}
	bad := apps.Manifest{Agent: &apps.AgentScope{Wake: []apps.ScopeEvent{{Pattern: "app.other.*"}}, TTLDays: 30}}
	if err := r.SetManifest(ctx, "agentchat", bad); !errors.Is(err, apps.ErrInvalidManifest) {
		t.Fatalf("invalid manifest: %v", err)
	}
	pool.Close()
	_, err = r.Authenticate(ctx, assertion(priv, nil), time.Now())
	if err == nil || errors.Is(err, apps.ErrUnauthenticated) {
		t.Fatalf("database down: %v", err)
	}
	ok := apps.Manifest{Agent: &apps.AgentScope{Wake: []apps.ScopeEvent{{Pattern: "app.agentchat.*"}}, TTLDays: 30}}
	if err := r.SetManifest(ctx, "agentchat", ok); err == nil || errors.Is(err, apps.ErrInvalidManifest) {
		t.Fatalf("database down, storing a manifest: %v", err)
	}
}

func TestManifestStaysInTheAppsNamespace(t *testing.T) {
	r, _ := setup(t)
	ok := apps.Manifest{Agent: &apps.AgentScope{Wake: []apps.ScopeEvent{{Pattern: "app.agentchat.channel.*"}}, TTLDays: 30}}
	if err := r.SetManifest(ctx, "agentchat", ok); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]apps.Manifest{
		"another app's events": {Agent: &apps.AgentScope{Wake: []apps.ScopeEvent{{Pattern: "app.sources.*"}}, TTLDays: 30}},
		"a source's events":    {Agent: &apps.AgentScope{Wake: []apps.ScopeEvent{{Pattern: "slack.message"}}, TTLDays: 30}},
		"everything":           {Agent: &apps.AgentScope{Wake: []apps.ScopeEvent{{Pattern: "*"}}, TTLDays: 30}},
		"a long TTL":           {Agent: &apps.AgentScope{TTLDays: 1000}},
		"post elsewhere":       {Agent: &apps.AgentScope{Post: []apps.ScopeEvent{{Pattern: "app.other.x"}}, TTLDays: 30}},
	} {
		if err := r.SetManifest(ctx, "agentchat", m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Account mode is turned off: no manifest asks for anything that reaches
// people's own workspaces, even our own apps' (which were approved for
// accounts), and the refusal says so.
func TestManifestRefusesAccountMode(t *testing.T) {
	r, _ := setup(t)
	scope := &apps.AgentScope{TTLDays: 30}
	workspace := json.RawMessage(`{"publish":[{"pattern":"app.agentchat.x"}],"ttl_days":30}`)
	for name, m := range map[string]apps.Manifest{
		"accounts wakeshift": {Accounts: apps.AccountsWakeshift, Agent: scope},
		"a workspace scope":  {Workspace: &workspace},
		"redirect URIs":      {Agent: &apps.AgentScope{TTLDays: 30, RedirectURIs: []string{"https://chat.test/cb"}}},
	} {
		err := r.SetManifest(ctx, "agentchat", m)
		if !errors.Is(err, apps.ErrInvalidManifest) || !strings.Contains(err.Error(), "no longer available") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := r.SetManifest(ctx, "agentchat", apps.Manifest{Accounts: "other", Agent: scope}); !errors.Is(err, apps.ErrInvalidManifest) {
		t.Errorf("accounts other: %v", err)
	}
	for _, accounts := range []string{"", apps.AccountsApp} {
		if err := r.SetManifest(ctx, "agentchat", apps.Manifest{Accounts: accounts, Agent: scope}); err != nil {
			t.Errorf("accounts %q: %v", accounts, err)
		}
	}
	var m apps.Manifest
	if err := json.Unmarshal([]byte(`{"accounts":"app","workspace":null,"agent":{"ttl_days":30}}`), &m); err != nil || m.Validate("agentchat") != nil {
		t.Errorf("a null workspace: %v %v", err, m.Validate("agentchat"))
	}
}

// An app whose stored manifest is still on account mode (from before it
// was turned off) publishes a headless one like any other app.
func TestAccountModeAppsPublishHeadless(t *testing.T) {
	r, _, d := setupDB(t)
	if _, err := d.Pool.Exec(ctx, `INSERT INTO apps (id, name, manifest, manifest_updated_at) VALUES ('legacy', 'legacy', '{"accounts":"wakeshift","agent":{"wake":[],"post":[],"ttl_days":30,"redirect_uris":["https://legacy.test/cb"]},"workspace":{"publish":[],"ttl_days":30}}', now())`); err != nil {
		t.Fatal(err)
	}
	if err := r.SetManifest(ctx, "legacy", apps.Manifest{Agent: &apps.AgentScope{TTLDays: 30}}); err != nil {
		t.Fatal(err)
	}
	if a, err := r.Get(ctx, "legacy"); err != nil || a.Manifest.Accounts != "" || a.Manifest.Workspace != nil || len(a.Manifest.Agent.RedirectURIs) != 0 {
		t.Fatalf("%+v %v", a.Manifest, err)
	}
}

// A key's last use is written on its first request, then at most once a
// minute, however busy it is.
func TestKeyUseIsWrittenAtMostOnceAMinute(t *testing.T) {
	r, key := setup(t)
	lastUsed := func() (at *time.Time) {
		r.Pool.QueryRow(ctx, `SELECT last_used_at FROM app_keys WHERE app_id = 'agentchat'`).Scan(&at)
		return at
	}
	if lastUsed() != nil {
		t.Fatal("used before any request")
	}
	n := 0
	auth := func() {
		n++
		tok := assertion(key, func(a *apps.Assertion) { a.ID = fmt.Sprint("use-", n) })
		if _, err := r.Authenticate(ctx, tok, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	auth()
	first := lastUsed()
	if first == nil {
		t.Fatal("the first use wasn't written")
	}
	r.Pool.Exec(ctx, `UPDATE app_keys SET last_used_at = now() - interval '30 seconds'`)
	recent := lastUsed()
	auth()
	auth()
	if got := lastUsed(); !got.Equal(*recent) {
		t.Fatalf("written again within a minute: %v -> %v", recent, got)
	}
	r.Pool.Exec(ctx, `UPDATE app_keys SET last_used_at = now() - interval '2 minutes'`)
	stale := lastUsed()
	auth()
	if got := lastUsed(); !got.After(*stale) {
		t.Fatalf("not written after a minute: %v -> %v", stale, got)
	}
}

// Requests that all read a stale last_used_at at once still write it once:
// the update itself checks the minute.
func TestConcurrentKeyUseWritesOnce(t *testing.T) {
	r, key, d := setupDB(t)
	auth := func(id string) {
		if _, err := r.Authenticate(ctx, assertion(key, func(a *apps.Assertion) { a.ID = id }), time.Now()); err != nil {
			t.Error(err)
		}
	}
	auth("first")
	r.Pool.Exec(ctx, `UPDATE app_keys SET last_used_at = now() - interval '2 minutes'`)
	// Count the writes: a trigger notes each update of last_used_at.
	if _, err := dbtest.Owner(t, d).Exec(ctx, `CREATE TABLE key_writes (at timestamptz);
		CREATE FUNCTION note_key_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO key_writes VALUES (now()); RETURN NEW; END $$;
		CREATE TRIGGER note_key_write AFTER UPDATE OF last_used_at ON app_keys FOR EACH ROW EXECUTE FUNCTION note_key_write();`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); auth(fmt.Sprint("concurrent-", i)) }()
	}
	wg.Wait()
	var n int
	r.Pool.QueryRow(ctx, `SELECT count(*) FROM key_writes`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d writes of last_used_at", n)
	}
}

// 031_app_keys.sql names the keys it moves with the same key IDs as KeyID.
func TestMigratedKeyIDs(t *testing.T) {
	r, _ := setup(t)
	for range 20 {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		var got string
		r.Pool.QueryRow(ctx, `SELECT translate(encode(substring(sha256($1) FROM 1 FOR 6), 'base64'), '+/', '-_')`, []byte(pub)).Scan(&got)
		if got != apps.KeyID(pub) {
			t.Fatalf("SQL %q, Go %q", got, apps.KeyID(pub))
		}
	}
}

// The configured key is our app's one key: a new one in WAKESHIFT_APPS
// replaces the old one at once, and a key can't be two apps'.
// An app registered in the console has an organization: Sync
// (WAKESHIFT_APPS) never takes it over.
func TestSyncNeverTakesOverAConsoleApp(t *testing.T) {
	r, _ := setup(t)
	var user string
	if err := r.Pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('ada@example.com') RETURNING id::text`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		if _, err := orgs.CreatePersonal(ctx, tx, user, "", "ada@example.com"); err != nil {
			return err
		}
		_, _, _, err := r.Create(ctx, tx, user, "", apps.NewApp{ID: "quizzer", Name: "Quizzer"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var orgSet bool
	if err := r.Pool.QueryRow(ctx, `SELECT org_id IS NOT NULL FROM apps WHERE id = 'quizzer'`).Scan(&orgSet); err != nil || !orgSet {
		t.Fatalf("org_id %v (%v)", orgSet, err)
	}
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	err := r.Sync(ctx, []apps.Config{{ID: "quizzer", Name: "Ours now", PublicKey: base64.StdEncoding.EncodeToString(pub)}})
	if err == nil || !strings.Contains(err.Error(), "registered in the console") {
		t.Fatalf("Sync over a console app: %v", err)
	}
	if a, err := r.Get(ctx, "quizzer"); err != nil || a.Name != "Quizzer" {
		t.Fatalf("the console app: %+v %v", a, err)
	}
}

func TestSyncReplacesTheKey(t *testing.T) {
	r, old := setup(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := r.Sync(ctx, []apps.Config{{ID: "agentchat", Name: "Agent Chat", MCPURL: "https://chat.test/mcp", PublicKey: base64.StdEncoding.EncodeToString(pub)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Authenticate(ctx, assertion(old, func(a *apps.Assertion) { a.ID = "old" }), time.Now()); err == nil {
		t.Fatal("the replaced key still works")
	}
	if _, err := r.Authenticate(ctx, assertion(priv, func(a *apps.Assertion) { a.ID = "new" }), time.Now()); err != nil {
		t.Fatalf("the new key: %v", err)
	}
	err := r.Sync(ctx, []apps.Config{{ID: "sources", Name: "Sources", PublicKey: base64.StdEncoding.EncodeToString(pub)}})
	if err == nil || !strings.Contains(err.Error(), "another app's") {
		t.Fatalf("one key for two apps: %v", err)
	}
}
