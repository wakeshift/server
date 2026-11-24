package adminapi_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/adminapi"
	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/iap/iaptest"
	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/internal/orgs"
)

const (
	admin          = "admin@example.com"
	serviceAccount = "admin-reader@example-project.iam.gserviceaccount.com"
	publicURL      = "https://app.wakeshift.test"
)

var ctx = context.Background()

// google signs ID tokens as Google's metadata server would.
type google struct{ key *ecdsa.PrivateKey }

func newGoogle(t *testing.T) *google {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &google{k}
}

func (g *google) verifier() adminapi.IDTokenVerifier {
	return oidc.NewVerifier(adminapi.GoogleIssuer, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&g.key.PublicKey}},
		&oidc.Config{ClientID: adminapi.Audience(publicURL), SupportedSigningAlgs: []string{oidc.ES256}})
}

type idClaims struct {
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	Subject  string `json:"sub"`
	Email    string `json:"email"`
	Verified bool   `json:"email_verified"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

func (g *google) token(t *testing.T, mutate func(*idClaims)) string {
	now := time.Now()
	c := idClaims{Issuer: adminapi.GoogleIssuer, Audience: adminapi.Audience(publicURL), Subject: "1234", Email: serviceAccount, Verified: true,
		IssuedAt: now.Unix(), Expires: now.Add(time.Hour).Unix()}
	if mutate != nil {
		mutate(&c)
	}
	return iaptest.SignWith(t, g.key, jose.ES256, c)
}

type fixture struct {
	d      *db.DB
	reg    *apps.Registry
	h      http.Handler
	google *google
	iap    *iaptest.Signer
	owner  string
	key    string // the app's key
}

func setup(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.New(t)
	reg := &apps.Registry{Pool: d.Pool, Issuer: publicURL}
	f := &fixture{d: d, reg: reg, google: newGoogle(t), iap: iaptest.New(t)}
	api := &adminapi.API{DB: d, Apps: reg, AppAPI: &appapi.API{DB: d, Apps: reg, Issuer: publicURL},
		Service: f.google.verifier(), ServiceAccount: serviceAccount, IAP: f.iap.Verifier(t, iaptest.Audience, admin)}
	mux := http.NewServeMux()
	api.Register(mux)
	f.h = mux
	if err := d.Pool.QueryRow(ctx, `INSERT INTO users (email, name) VALUES ('dev@example.com', 'Dev') RETURNING id::text`).Scan(&f.owner); err != nil {
		t.Fatal(err)
	}
	err := pgx.BeginFunc(ctx, d.Pool, func(tx pgx.Tx) error {
		// Their personal organization, as signing up makes it.
		if _, err := orgs.CreatePersonal(ctx, tx, f.owner, "Dev", "dev@example.com"); err != nil {
			return err
		}
		_, _, key, err := reg.Create(ctx, tx, f.owner, "", apps.NewApp{ID: "acme-bot", Name: "Acme bot", MCPURL: "https://acme.example/mcp"})
		f.key = key
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) good(t *testing.T) (bearer, assertion string) {
	return f.google.token(t, nil), f.iap.Assertion(t, iaptest.Audience, admin)
}

func (f *fixture) do(t *testing.T, method, path, bearer, assertion string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if assertion != "" {
		req.Header.Set(adminapi.AssertionHeader, assertion)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// counts is how many app_log and admin_log entries there are for the app
// (app_log: only the admin's).
func (f *fixture) counts(t *testing.T, app string) (appLog, adminLog int) {
	t.Helper()
	if err := f.d.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM app_log WHERE app_id = $1 AND actor = 'admin'),
		(SELECT count(*) FROM admin_log WHERE target_id = $1)`, app).Scan(&appLog, &adminLog); err != nil {
		t.Fatal(err)
	}
	return
}

var routes = []struct{ method, path string }{
	{"POST", "/internal/admin/v1/apps/acme-bot/suspend"},
	{"POST", "/internal/admin/v1/apps/acme-bot/unsuspend"},
	{"PUT", "/internal/admin/v1/apps/acme-bot/workspace-cap"},
	{"POST", "/internal/admin/v1/apps/acme-bot/delete"},
	{"POST", "/internal/admin/v1/apps/acme-bot/users/forget"},
	{"PUT", "/internal/admin/v1/signups/cap"},
	{"POST", "/internal/admin/v1/waitlist/invite"},
	{"POST", "/internal/admin/v1/waitlist/remove"},
	{"POST", "/internal/admin/v1/apps/acme-bot/voucher-subscriptions/end"},
	{"POST", "/internal/admin/v1/voucher-subscriptions/end"},
}

// Every route is 404 unless both tokens are valid: the admin service's ID
// token (issuer, audience, account, verified email, unexpired) and the
// person's IAP assertion (audience, allowlist, unexpired). Nothing is
// written by a refused request.
func TestEveryRouteNeedsBothTokens(t *testing.T) {
	f := setup(t)
	bearer, assertion := f.good(t)
	otherGoogle := newGoogle(t)
	otherIAP := iaptest.New(t)
	now := time.Now()
	type creds struct{ bearer, assertion string }
	refused := map[string]creds{
		"no tokens":                  {"", ""},
		"only the service account":   {bearer, ""},
		"only the person":            {"", assertion},
		"the assertion as bearer":    {assertion, assertion},
		"service token, wrong aud":   {f.google.token(t, func(c *idClaims) { c.Audience = publicURL + "/api/apps" }), assertion},
		"another service account":    {f.google.token(t, func(c *idClaims) { c.Email = "other-service@example-project.iam.gserviceaccount.com" }), assertion},
		"unverified email":           {f.google.token(t, func(c *idClaims) { c.Verified = false }), assertion},
		"service token, expired":     {f.google.token(t, func(c *idClaims) { c.Expires = now.Add(-time.Minute).Unix() }), assertion},
		"service token, another iss": {f.google.token(t, func(c *idClaims) { c.Issuer = "https://evil.example" }), assertion},
		"service token, forged":      {otherGoogle.token(t, nil), assertion},
		"assertion, wrong aud":       {bearer, f.iap.Assertion(t, "/projects/123/global/backendServices/999", admin)},
		"assertion, off the list":    {bearer, f.iap.Assertion(t, iaptest.Audience, "someone@example.com")},
		"assertion, expired":         {bearer, f.iap.Sign(t, iaptest.Claims{Audience: iaptest.Audience, Email: admin, Now: now.Add(-time.Hour)})},
		"assertion, forged":          {bearer, otherIAP.Assertion(t, iaptest.Audience, admin)},
	}
	entry := f.waitlisted(t, "wait@example.com", "Wait")
	for name, c := range refused {
		for _, r := range routes {
			rec := f.do(t, r.method, r.path, c.bearer, c.assertion, map[string]any{"reason": "test", "confirm": "acme-bot", "approve": true, "user": "u", "request": "r", "cap": 5,
				"ids": []string{entry}})
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: %s %s: %d, want 404", name, r.method, r.path, rec.Code)
			}
		}
	}
	if a, l := f.counts(t, "acme-bot"); a != 0 || l != 0 {
		t.Fatalf("refused requests wrote %d app_log and %d admin_log entries", a, l)
	}
	var logged, entries, limit int
	f.d.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM admin_log), (SELECT count(*) FROM waitlist WHERE invited_at IS NULL), (SELECT cap FROM signup_cap)`).Scan(&logged, &entries, &limit)
	if logged != 0 || entries != 1 || limit != 50 {
		t.Fatalf("refused requests changed sign-ups: %d logged, %d waiting, cap %d", logged, entries, limit)
	}
	// Both valid: it acts.
	if rec := f.do(t, "POST", routes[0].path, bearer, assertion, map[string]string{"reason": "Terms §14: spam"}); rec.Code != 200 {
		t.Fatalf("both tokens: %d %s", rec.Code, rec.Body)
	}
	// An API that isn't configured answers 404 to everything.
	mux := http.NewServeMux()
	(&adminapi.API{DB: f.d, Apps: f.reg}).Register(mux)
	req := httptest.NewRequest("POST", routes[0].path, strings.NewReader(`{"reason":"x"}`))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(adminapi.AssertionHeader, assertion)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unconfigured: %d", rec.Code)
	}
}

func (f *fixture) appAssertion(t *testing.T) string {
	t.Helper()
	// The console's key names no app: sign by its kid, as the SDKs do.
	k, err := appkey.Parse(f.key)
	if err != nil {
		t.Fatal(err)
	}
	const id = "acme-bot"
	now := time.Now()
	tok, err := jws.Sign(k.Private(), k.KeyID(), "app+jwt", apps.Assertion{Issuer: id, Subject: id, Audience: f.reg.AssertionAudience(),
		IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix(), ID: base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprint(now.UnixNano())))})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// Suspending stops every key at once, writes exactly one entry in each log,
// and shows the owner the reason; unsuspending restores it.
func TestSuspend(t *testing.T) {
	f := setup(t)
	bearer, assertion := f.good(t)
	if _, err := f.reg.Authenticate(ctx, f.appAssertion(t), time.Now()); err != nil {
		t.Fatalf("before: %v", err)
	}
	// A missing reason refuses, and writes nothing.
	if rec := f.do(t, "POST", routes[0].path, bearer, assertion, map[string]string{"reason": "  "}); rec.Code != 400 {
		t.Fatalf("no reason: %d", rec.Code)
	}
	if a, l := f.counts(t, "acme-bot"); a != 0 || l != 0 {
		t.Fatalf("a refused suspend wrote %d, %d", a, l)
	}
	if rec := f.do(t, "POST", routes[0].path, bearer, assertion, map[string]string{"reason": "Terms §14: sends spam"}); rec.Code != 200 {
		t.Fatalf("suspend: %d %s", rec.Code, rec.Body)
	}
	if a, l := f.counts(t, "acme-bot"); a != 1 || l != 1 {
		t.Fatalf("suspend wrote %d app_log and %d admin_log entries", a, l)
	}
	var actor, reason string
	if err := f.d.Pool.QueryRow(ctx, `SELECT actor, reason FROM admin_log WHERE target_id = 'acme-bot'`).Scan(&actor, &reason); err != nil || actor != admin || reason != "Terms §14: sends spam" {
		t.Fatalf("admin_log: %q %q %v", actor, reason, err)
	}
	if _, err := f.reg.Authenticate(ctx, f.appAssertion(t), time.Now()); !errors.Is(err, apps.ErrSuspended) {
		t.Fatalf("a suspended app's key: %v", err)
	}
	// The owner sees it, with the reason, and can't make a key.
	var o apps.Owned
	err := pgx.BeginFunc(ctx, f.d.Pool, func(tx pgx.Tx) (err error) {
		o, err = f.reg.GetOwned(ctx, tx, f.owner, "acme-bot")
		if err != nil {
			return err
		}
		_, _, err = f.reg.CreateKey(ctx, tx, f.owner, "acme-bot", "another")
		return err
	})
	if !errors.Is(err, apps.ErrSuspended) || o.SuspendedAt == nil || o.SuspendedReason != "Terms §14: sends spam" {
		t.Fatalf("owner: %+v, %v", o, err)
	}
	// Again: no change, nothing written.
	if rec := f.do(t, "POST", routes[0].path, bearer, assertion, map[string]string{"reason": "again"}); rec.Code != http.StatusConflict {
		t.Fatalf("suspending again: %d", rec.Code)
	}
	if rec := f.do(t, "POST", routes[1].path, bearer, assertion, map[string]string{"reason": "appeal accepted"}); rec.Code != 200 {
		t.Fatalf("unsuspend: %d %s", rec.Code, rec.Body)
	}
	if a, l := f.counts(t, "acme-bot"); a != 2 || l != 2 {
		t.Fatalf("after unsuspend: %d, %d", a, l)
	}
	if _, err := f.reg.Authenticate(ctx, f.appAssertion(t), time.Now()); err != nil {
		t.Fatalf("after unsuspend: %v", err)
	}
}

func TestWorkspaceCap(t *testing.T) {
	f := setup(t)
	bearer, assertion := f.good(t)
	path := routes[2].path
	for _, bad := range []map[string]any{{"cap": -1, "reason": "x"}, {"cap": 20_000_000, "reason": "x"}, {"cap": 5}} {
		if rec := f.do(t, "PUT", path, bearer, assertion, bad); rec.Code != 400 {
			t.Errorf("%v: %d", bad, rec.Code)
		}
	}
	if rec := f.do(t, "PUT", path, bearer, assertion, map[string]any{"cap": 2, "reason": "beta"}); rec.Code != 200 {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}
	app, err := f.reg.Get(ctx, "acme-bot")
	if err != nil || apps.WorkspaceCap(app) != 2 {
		t.Fatalf("cap: %d %v", apps.WorkspaceCap(app), err)
	}
	if err := apps.CheckWorkspaceCap(ctx, f.d.Pool, app, "u1"); err != nil {
		t.Fatal(err)
	}
	f.d.Pool.Exec(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('a', 'app', 'acme-bot', 'u1'), ('b', 'app', 'acme-bot', 'u2')`)
	if err := apps.CheckWorkspaceCap(ctx, f.d.Pool, app, "u3"); !apps.IsWorkspaceLimit(err) {
		t.Fatalf("over the override: %v", err)
	}
	if rec := f.do(t, "PUT", path, bearer, assertion, map[string]any{"cap": nil, "reason": "back to the default"}); rec.Code != 200 {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body)
	}
	if app, _ := f.reg.Get(ctx, "acme-bot"); app.WorkspaceCapOverride != nil {
		t.Fatal("not reset")
	}
	if a, l := f.counts(t, "acme-bot"); a != 2 || l != 2 {
		t.Fatalf("cap changes wrote %d, %d", a, l)
	}
	// Ours are set in WAKESHIFT_APPS (Sync writes it on every start).
	f.reg.Sync(ctx, []apps.Config{{ID: "agentchat", Name: "Agent Chat", PublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}})
	if rec := f.do(t, "PUT", "/internal/admin/v1/apps/agentchat/workspace-cap", bearer, assertion, map[string]any{"cap": 5, "reason": "x"}); rec.Code != http.StatusConflict {
		t.Fatalf("ours: %d %s", rec.Code, rec.Body)
	}
}

// Deleting needs the ID typed out; it then works as the owner's delete.
func TestDelete(t *testing.T) {
	f := setup(t)
	bearer, assertion := f.good(t)
	path := routes[3].path
	if rec := f.do(t, "POST", path, bearer, assertion, map[string]string{"reason": "abuse", "confirm": "acme"}); rec.Code != 400 {
		t.Fatalf("wrong confirm: %d", rec.Code)
	}
	if a, l := f.counts(t, "acme-bot"); a != 0 || l != 0 {
		t.Fatalf("a refused delete wrote %d, %d", a, l)
	}
	if rec := f.do(t, "POST", "/internal/admin/v1/apps/nope/delete", bearer, assertion, map[string]string{"reason": "abuse", "confirm": "nope"}); rec.Code != 404 {
		t.Fatalf("unknown app: %d", rec.Code)
	}
	if rec := f.do(t, "POST", path, bearer, assertion, map[string]string{"reason": "Terms §14", "confirm": "acme-bot"}); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if _, err := f.reg.Get(ctx, "acme-bot"); !errors.Is(err, apps.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	var pending bool
	f.d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_deletions WHERE app_id = 'acme-bot')`).Scan(&pending)
	if a, l := f.counts(t, "acme-bot"); a != 1 || l != 1 || !pending {
		t.Fatalf("delete wrote %d, %d; deletion recorded %v", a, l, pending)
	}
	// Our own apps aren't deleted here.
	f.reg.Sync(ctx, []apps.Config{{ID: "agentchat", Name: "Agent Chat", PublicKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}})
	if rec := f.do(t, "POST", "/internal/admin/v1/apps/agentchat/delete", bearer, assertion, map[string]string{"reason": "x", "confirm": "agentchat"}); rec.Code != http.StatusConflict {
		t.Fatalf("deleting ours: %d", rec.Code)
	}
}

// The accounts action, approving or revoking an app for account mode, is
// gone with it: like any unknown path, 404, even with both tokens.
func TestNoAccountsAction(t *testing.T) {
	f := setup(t)
	bearer, assertion := f.good(t)
	rec := f.do(t, "POST", "/internal/admin/v1/apps/acme-bot/accounts", bearer, assertion, map[string]any{"approve": false, "confirm": "acme-bot", "reason": "x"})
	if rec.Code != 404 {
		t.Fatalf("accounts: %d %s", rec.Code, rec.Body)
	}
	if a, l := f.counts(t, "acme-bot"); a != 0 || l != 0 {
		t.Fatalf("wrote %d, %d", a, l)
	}
}

// Forgetting a user needs a reason and a request reference, writes the
// request (never the user's ID or a hash of it) in admin_log, and nothing
// when there's no such user.
func TestForgetUser(t *testing.T) {
	f := setup(t)
	bearer, assertion := f.good(t)
	path := routes[4].path
	f.d.Pool.Exec(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('a', 'app', 'acme-bot', 'ada@example.com')`)
	if rec := f.do(t, "POST", path, bearer, assertion, map[string]string{"user": "ada@example.com", "reason": "data request"}); rec.Code != 400 {
		t.Fatalf("no request reference: %d", rec.Code)
	}
	if rec := f.do(t, "POST", path, bearer, assertion, map[string]string{"user": "nobody", "request": "DR-1", "reason": "data request"}); rec.Code != 404 {
		t.Fatalf("no such user: %d", rec.Code)
	}
	if _, l := f.counts(t, "acme-bot"); l != 0 {
		t.Fatalf("refused forgets wrote %d", l)
	}
	if rec := f.do(t, "POST", path, bearer, assertion, map[string]string{"user": "ada@example.com", "request": "DR-1", "reason": "data request"}); rec.Code != 200 {
		t.Fatalf("forget: %d %s", rec.Code, rec.Body)
	}
	var detail string
	if err := f.d.Pool.QueryRow(ctx, `SELECT detail::text FROM admin_log WHERE target_id = 'acme-bot'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, "ada@") || !strings.Contains(detail, "DR-1") || strings.Contains(detail, "sha256") {
		t.Fatalf("admin_log detail: %s", detail)
	}
	if err := f.d.Pool.QueryRow(ctx, `SELECT detail::text FROM app_log WHERE app_id = 'acme-bot' AND action = 'app.user_forgotten'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, "ada@") || strings.Contains(detail, "sha256") {
		t.Fatalf("app_log detail: %s", detail)
	}
	var n int
	f.d.Pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE app_id = 'acme-bot'`).Scan(&n)
	if a, l := f.counts(t, "acme-bot"); n != 0 || a != 1 || l != 1 {
		t.Fatalf("workspaces left %d; logs %d, %d", n, a, l)
	}
	// An entry written with the hash, as the previous revision did while
	// this one rolls out, is kept without it (095_forgotten_user_hash.sql).
	for _, q := range []string{
		`INSERT INTO app_log (app_id, actor, action, detail) VALUES ('acme-bot', 'hub', 'app.user_forgotten', '{"user_sha256": "abc", "conversations": 1}')`,
		`INSERT INTO admin_log (actor, action, target_kind, target_id, detail) VALUES ('x@example.com', 'app.user_forgotten', 'app', 'acme-bot', '{"user_sha256": "abc", "request": "DR-2"}')`,
	} {
		if _, err := f.d.Pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	f.d.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM app_log WHERE detail ? 'user_sha256') + (SELECT count(*) FROM admin_log WHERE detail ? 'user_sha256')
		+ (SELECT count(*) FROM app_log WHERE detail = '{"conversations": 1}') * 10 + (SELECT count(*) FROM admin_log WHERE detail = '{"request": "DR-2"}') * 10`).Scan(&n)
	if n != 20 {
		t.Fatalf("hashes kept or entries lost: %d", n)
	}
}

// The emergency stop for subscribe vouchers: one app's, or every app's;
// each recorded in admin_log with what it ended (internal/coretest's
// TestVoucherEmergencyStop ends real ones).
func TestEndVoucherSubscriptions(t *testing.T) {
	f := setup(t)
	bearer, assertion := f.good(t)
	if rec := f.do(t, "POST", "/internal/admin/v1/apps/acme-bot/voucher-subscriptions/end", bearer, assertion, map[string]string{}); rec.Code != 400 {
		t.Fatalf("no reason: %d", rec.Code)
	}
	if rec := f.do(t, "POST", "/internal/admin/v1/apps/nope/voucher-subscriptions/end", bearer, assertion, map[string]string{"reason": "incident"}); rec.Code != 404 {
		t.Fatalf("no such app: %d", rec.Code)
	}
	rec := f.do(t, "POST", "/internal/admin/v1/apps/acme-bot/voucher-subscriptions/end", bearer, assertion, map[string]string{"reason": "incident 12"})
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"ended":0,"workspaces":0}` {
		t.Fatalf("one app: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "POST", "/internal/admin/v1/voucher-subscriptions/end", bearer, assertion, map[string]string{"reason": "incident 12"}); rec.Code != 200 {
		t.Fatalf("every app: %d %s", rec.Code, rec.Body)
	}
	rows, _ := f.d.Pool.Query(ctx, `SELECT actor || ' ' || action || ' ' || target_kind || ' ' || target_id || ' ' || reason || ' ' || detail::text FROM admin_log ORDER BY id`)
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		admin + ` subscribe_vouchers.subscriptions_ended app acme-bot incident 12 {"ended": 0, "complete": true, "workspaces": 0}`,
		admin + ` subscribe_vouchers.subscriptions_ended platform * incident 12 {"ended": 0, "complete": true, "workspaces": 0}`,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("admin_log:\n%v\nwant\n%v", got, want)
	}
}
