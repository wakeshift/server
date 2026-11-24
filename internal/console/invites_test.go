package console_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/mail"
	"github.com/wakeshift/server/internal/orgs"
)

// Invites, with the invited addresses, are deleted 30 days after they
// ended (accepted, revoked or expired); a deleted organization's log 30
// days after the deletion. Pending invites and live organizations' logs
// stay.
func TestOrgRetention(t *testing.T) {
	c := coretest.StartWith(t, withMail(&mail.Fake{}))
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	org := newOrg(t, ada, "Acme")
	ids := map[string]string{}
	for _, who := range []string{"accepted", "revoked", "expired", "recent", "pending"} {
		s, _ := invite(t, ada, org, who+"@example.com", "developer")
		ids[who] = s.Invite.ID
	}
	for who, q := range map[string]string{
		"accepted": `UPDATE organization_invites SET accepted_at = now() - interval '31 days' WHERE id = $1`,
		"revoked":  `UPDATE organization_invites SET revoked_at = now() - interval '31 days' WHERE id = $1`,
		"expired":  `UPDATE organization_invites SET expires_at = now() - interval '31 days' WHERE id = $1`,
		"recent":   `UPDATE organization_invites SET revoked_at = now() - interval '29 days', expires_at = now() - interval '25 days' WHERE id = $1`,
	} {
		if _, err := c.DB.Pool.Exec(ctx, q, ids[who]); err != nil {
			t.Fatal(err)
		}
	}
	// Two deleted organizations, one long enough ago.
	old, recent := newOrg(t, ada, "Old"), newOrg(t, ada, "Recent")
	invite(t, ada, old, "someone@example.com", "developer")
	for _, o := range []string{old, recent} {
		if code := ada.Do("DELETE", "/api/v1/orgs/"+o, nil, nil); code != 200 {
			t.Fatalf("deleting: %d", code)
		}
	}
	c.DB.Pool.Exec(ctx, `UPDATE organizations SET deleted_at = now() - interval '31 days' WHERE id = $1`, old)
	c.DB.Pool.Exec(ctx, `UPDATE organization_invites SET revoked_at = now() - interval '31 days' WHERE org_id = $1`, old)
	c.DB.Pool.Exec(ctx, `UPDATE organizations SET deleted_at = now() - interval '29 days' WHERE id = $1`, recent)
	acmeLog, oldLog := orgLogCount(t, c, org), orgLogCount(t, c, old)
	if oldLog == 0 || orgLogCount(t, c, recent) == 0 {
		t.Fatal("no log to delete")
	}

	invites, logs, err := orgs.Prune(ctx, c.DB.Pool, 1000)
	if err != nil || invites != 4 || logs != int64(oldLog) {
		t.Fatalf("pruned %d invites and %d log entries (want 4 and %d): %v", invites, logs, oldLog, err)
	}
	left := map[string]bool{}
	rows, _ := c.DB.Pool.Query(ctx, `SELECT id::text FROM organization_invites`)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		left[id] = true
	}
	rows.Close()
	if len(left) != 2 || !left[ids["recent"]] || !left[ids["pending"]] {
		t.Fatalf("invites left: %v of %v", left, ids)
	}
	if orgLogCount(t, c, old) != 0 || orgLogCount(t, c, recent) == 0 || orgLogCount(t, c, org) != acmeLog {
		t.Fatalf("logs: old %d, recent %d, acme %d (was %d)", orgLogCount(t, c, old), orgLogCount(t, c, recent), orgLogCount(t, c, org), acmeLog)
	}
	// The deleted organization's own row stays.
	var n int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM organizations WHERE id = $1`, old).Scan(&n)
	if n != 1 {
		t.Error("the deleted organization's row went")
	}
	if invites, logs, err := orgs.Prune(ctx, c.DB.Pool, 1000); err != nil || invites+logs != 0 {
		t.Fatalf("a second pass: %d %d %v", invites, logs, err)
	}
}

type sentInvite struct {
	Invite struct {
		ID, Email, Role string
		Expired         bool
	}
	URL     string
	Emailed bool
}

// invite sends an invite as u, returning the response and its token.
func invite(t *testing.T, u *coretest.User, org, email, role string) (sentInvite, string) {
	t.Helper()
	var s sentInvite
	var e apiError
	if code := u.Do("POST", "/api/v1/orgs/"+org+"/invites", map[string]string{"email": email, "role": role}, &struct {
		*sentInvite
		*apiError
	}{&s, &e}); code != 200 {
		t.Fatalf("invite %s as %s: %d %+v", email, role, code, e)
	}
	_, token, ok := strings.Cut(s.URL, "/join#")
	if !ok || token == "" {
		t.Fatalf("invite link %q", s.URL)
	}
	return s, token
}

func accept(u *coretest.User, token string) (int, apiError) {
	var e apiError
	code := u.Do("POST", "/api/v1/join", map[string]string{"token": token}, &e)
	return code, e
}

func withMail(m mail.Sender) func(*core.Options) { return func(o *core.Options) { o.Mail = m } }

// An invite, end to end: emailed with the link (token in the fragment),
// previewed by whoever holds it, accepted only by the invited address,
// once. Every step is in the organization's log, with the address masked
// and never the token.
func TestInvites(t *testing.T) {
	fake := &mail.Fake{}
	c := coretest.StartWith(t, withMail(fake))
	ada := c.SignIn("ada@example.com")
	org := newOrg(t, ada, "Acme")
	s, token := invite(t, ada, org, "Cy@Example.com", "developer")
	if !s.Emailed || s.Invite.Email != "cy@example.com" || !strings.HasPrefix(s.URL, c.URL+"/join#") {
		t.Fatalf("sent: %+v", s)
	}
	sent := fake.Sent()
	if len(sent) != 1 || sent[0].To != "cy@example.com" || !strings.Contains(sent[0].Text, s.URL) || !strings.Contains(sent[0].HTML, s.URL) ||
		sent[0].Subject != "You're invited to an organization on Wakeshift" || !strings.Contains(sent[0].Text, "ada@example.com invited you to join Acme") {
		t.Fatalf("email: %+v", sent)
	}

	// The preview: the organization, who sent it, the role; nothing else.
	anon := c.SignIn("someone@example.com")
	var p map[string]any
	if code := anon.Do("POST", "/api/v1/join/preview", map[string]string{"token": token}, &p); code != 200 ||
		p["org_name"] != "Acme" || p["invited_by"] != "ada@example.com" || p["role"] != "developer" || len(p) != 3 {
		t.Fatalf("preview: %d %v", code, p)
	}
	if code := anon.Do("POST", "/api/v1/join/preview", map[string]string{"token": "nope"}, nil); code != 404 {
		t.Fatalf("a made-up token's preview: %d", code)
	}

	// Someone else, signed in, holding the link: refused, with the address
	// masked, and nothing changes.
	before := orgLogCount(t, c, org)
	var wrong struct {
		apiError
		Email string
	}
	if code := anon.Do("POST", "/api/v1/join", map[string]string{"token": token}, &wrong); code != 403 || wrong.Error.Code != "wrong_account" ||
		wrong.Email != "c•••@example.com" || strings.Contains(wrong.Error.Message, "cy@") {
		t.Fatalf("the wrong account: %d %+v", code, wrong)
	}
	if orgLogCount(t, c, org) != before {
		t.Fatal("a refused accept was logged")
	}

	cy := c.SignIn("cy@example.com")
	if code, e := accept(cy, token); code != 200 {
		t.Fatalf("accepting: %d %+v", code, e)
	}
	var o struct{ Role string }
	if code := cy.Do("GET", "/api/v1/orgs/"+org, nil, &o); code != 200 || o.Role != "developer" {
		t.Fatalf("cy in Acme: %d %+v", code, o)
	}
	// Accepting again is a no-op for cy; for anyone else the link is dead.
	if code, e := accept(cy, token); code != 200 {
		t.Fatalf("accepting again: %d %+v", code, e)
	}
	if code, e := accept(anon, token); code != 404 || e.Error.Code != "invalid_invite" {
		t.Fatalf("a used invite: %d %+v", code, e)
	}
	var entries struct {
		Entries []struct {
			Action string
			Detail map[string]any
		}
	}
	ada.Do("GET", "/api/v1/orgs/"+org+"/log", nil, &entries)
	actions := []string{}
	for _, e := range entries.Entries {
		actions = append(actions, e.Action)
		if email, _ := e.Detail["email"].(string); strings.Contains(email, "cy@") {
			t.Errorf("an unmasked address in the log: %+v", e)
		}
	}
	if strings.Join(actions, " ") != "org.member_joined org.invite_accepted org.invite_sent org.created" {
		t.Errorf("the log: %v", actions)
	}
	var leaked int
	c.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM org_log WHERE detail::text LIKE '%' || $1 || '%'`, token).Scan(&leaked)
	if leaked != 0 {
		t.Error("the token is in the log")
	}

	// Inviting a member: 409.
	var e apiError
	if code := ada.Do("POST", "/api/v1/orgs/"+org+"/invites", map[string]string{"email": "cy@example.com", "role": "admin"}, &e); code != 409 || e.Error.Code != "already_member" {
		t.Fatalf("inviting a member: %d %+v", code, e)
	}
}

// Expired, revoked and resent invites: only the newest link works, and
// inviting an address whose invite expired replaces it.
func TestInviteLifetimes(t *testing.T) {
	c := coretest.StartWith(t, withMail(&mail.Fake{}))
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	cy := c.SignIn("cy@example.com")
	org := newOrg(t, ada, "Acme")
	p := "/api/v1/orgs/" + org

	s, token := invite(t, ada, org, "cy@example.com", "developer")
	var e apiError
	if code := ada.Do("POST", p+"/invites", map[string]string{"email": "cy@example.com", "role": "developer"}, &e); code != 409 || e.Error.Code != "invite_pending" {
		t.Fatalf("inviting twice: %d %+v", code, e)
	}
	// Expired: refused; inviting again revokes it in the same go.
	c.DB.Pool.Exec(ctx, `UPDATE organization_invites SET expires_at = now() - interval '1 second' WHERE id = $1`, s.Invite.ID)
	if code, _ := accept(cy, token); code != 404 {
		t.Fatalf("an expired invite: %d", code)
	}
	var list struct {
		Invites []struct {
			ID      string
			Expired bool
		}
	}
	ada.Do("GET", p+"/invites", nil, &list)
	if len(list.Invites) != 1 || !list.Invites[0].Expired {
		t.Fatalf("invites: %+v", list)
	}
	s2, token2 := invite(t, ada, org, "cy@example.com", "developer")
	var revoked int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM organization_invites WHERE id = $1 AND revoked_at IS NOT NULL`, s.Invite.ID).Scan(&revoked)
	if revoked != 1 || s2.Invite.ID == s.Invite.ID {
		t.Fatalf("the expired invite wasn't replaced")
	}
	// Resent: a new token; the old one is dead.
	var resent sentInvite
	if code := ada.Do("POST", p+"/invites/"+s2.Invite.ID+"/resend", nil, &resent); code != 200 || !strings.Contains(resent.URL, "/join#") {
		t.Fatalf("resend: %d %+v", code, resent)
	}
	_, token3, _ := strings.Cut(resent.URL, "/join#")
	if token3 == token2 {
		t.Fatal("resending kept the token")
	}
	if code, _ := accept(cy, token2); code != 404 {
		t.Fatalf("the old link after a resend: %d", code)
	}
	// Revoked: dead.
	if code := ada.Do("POST", p+"/invites/"+s2.Invite.ID+"/revoke", nil, nil); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ := accept(cy, token3); code != 404 {
		t.Fatalf("a revoked invite: %d", code)
	}
	if code := ada.Do("POST", p+"/invites/"+s2.Invite.ID+"/revoke", nil, nil); code != 404 {
		t.Fatalf("revoking twice: %d", code)
	}
}

// Who may invite whom: owners any role; admins only developers, and only
// their invites; developers none. An invite whose sender was removed or
// demoted is refused when accepted.
func TestInvitesByRole(t *testing.T) {
	c := coretest.StartWith(t, withMail(&mail.Fake{}))
	ada := c.SignIn("ada@example.com") // owner
	bob := c.SignIn("bob@example.com") // admin
	cy := c.SignIn("cy@example.com")   // developer
	eve := c.SignIn("eve@example.com") // not a member
	org := newOrg(t, ada, "Acme")
	join(t, c, org, bob, "admin")
	join(t, c, org, cy, "developer")
	p := "/api/v1/orgs/" + org
	for _, r := range []struct {
		who   *coretest.User
		email string
		role  string
		code  int
	}{
		{bob, "x1@example.com", "admin", 403}, {bob, "x2@example.com", "owner", 403},
		{cy, "x3@example.com", "developer", 403}, {eve, "x4@example.com", "developer", 404},
		{ada, "x5@example.com", "boss", 400}, {ada, "not an address", "developer", 400},
	} {
		before := orgLogCount(t, c, org)
		if code := r.who.Do("POST", p+"/invites", map[string]string{"email": r.email, "role": r.role}, nil); code != r.code {
			t.Errorf("inviting %s as %s: %d, want %d", r.email, r.role, code, r.code)
		}
		if orgLogCount(t, c, org) != before {
			t.Errorf("a refused invite was logged")
		}
	}
	for _, u := range []*coretest.User{cy, eve} {
		if code := u.Do("GET", p+"/invites", nil, nil); code != 403 && code != 404 {
			t.Errorf("listing invites: %d", code)
		}
	}
	owners, _ := invite(t, ada, org, "admin-to-be@example.com", "admin")
	if code := bob.Do("POST", p+"/invites/"+owners.Invite.ID+"/revoke", nil, nil); code != 403 {
		t.Errorf("an admin revoking an admin's invite: %d", code)
	}
	if code := bob.Do("POST", p+"/invites/"+owners.Invite.ID+"/resend", nil, nil); code != 403 {
		t.Errorf("an admin resending an admin's invite: %d", code)
	}
	_, devToken := invite(t, bob, org, "dan@example.com", "developer")
	_, devToken2 := invite(t, bob, org, "fay@example.com", "developer")
	// Bob demoted: his invites are refused.
	join(t, c, org, bob, "developer")
	dan := c.SignIn("dan@example.com")
	if code, e := accept(dan, devToken); code != 409 || e.Error.Code != "inviter_cant" {
		t.Fatalf("a demoted sender's invite: %d %+v", code, e)
	}
	// Removed: the same.
	if code := ada.Do("DELETE", p+"/members/"+userID(t, bob), nil, nil); code != 200 {
		t.Fatal(code)
	}
	fay := c.SignIn("fay@example.com")
	if code, e := accept(fay, devToken2); code != 409 || e.Error.Code != "inviter_cant" {
		t.Fatalf("a removed sender's invite: %d %+v", code, e)
	}
}

// Two accepts at once: one membership, one set of log entries.
func TestConcurrentAccepts(t *testing.T) {
	c := coretest.StartWith(t, withMail(&mail.Fake{}))
	ada := c.SignIn("ada@example.com")
	org := newOrg(t, ada, "Acme")
	for i := range 5 {
		email := "cy" + string(rune('a'+i)) + "@example.com"
		_, token := invite(t, ada, org, email, "developer")
		cy := c.SignIn(email)
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() { defer wg.Done(); accept(cy, token) }()
		}
		wg.Wait()
		var members, joined int
		c.DB.Pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM organization_members m JOIN users u ON u.id = m.user_id WHERE m.org_id = $1 AND u.email = $2),
			(SELECT count(*) FROM org_log WHERE org_id = $1 AND action = 'org.member_joined' AND detail->>'user_id' = (SELECT id::text FROM users WHERE email = $2))`, org, email).Scan(&members, &joined)
		if members != 1 || joined != 1 {
			t.Fatalf("round %d: %d memberships, %d joins logged", i, members, joined)
		}
	}
}

// Invite email is limited: 20 an hour per organization, 5 a day per
// address. Without email set up, the console shows the link instead.
func TestInviteLimits(t *testing.T) {
	c := coretest.StartWith(t, withMail(&mail.Fake{NotReady: true}))
	ada := c.SignIn("ada@example.com")
	org := newOrg(t, ada, "Acme")
	p := "/api/v1/orgs/" + org
	s, _ := invite(t, ada, org, "cy@example.com", "developer")
	if s.Emailed {
		t.Fatal("emailed without email set up")
	}
	var list struct{ Email bool }
	ada.Do("GET", p+"/invites", nil, &list)
	if list.Email {
		t.Fatal("the invites page says email is set up")
	}
	for i := range 4 {
		if code := ada.Do("POST", p+"/invites/"+s.Invite.ID+"/resend", nil, nil); code != 200 {
			t.Fatalf("resend %d: %d", i, code)
		}
	}
	var e apiError
	if code := ada.Do("POST", p+"/invites/"+s.Invite.ID+"/resend", nil, &e); code != 429 || e.Error.Code != "rate_limited" {
		t.Fatalf("a sixth email to one address: %d %+v", code, e)
	}
	for i := range 15 {
		invite(t, ada, org, "x"+string(rune('a'+i))+"@example.com", "developer")
	}
	if code := ada.Do("POST", p+"/invites", map[string]string{"email": "one-more@example.com", "role": "developer"}, &e); code != 429 {
		t.Fatalf("a 21st email in an hour: %d %+v", code, e)
	}
}

// Tokens and links never reach the logs, sent or not.
func TestInviteTokensAreNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	c := coretest.StartWith(t, withMail(&mail.Fake{Fail: errors.New("mail: Resend answered HTTP 500")}))
	ada := c.SignIn("ada@example.com")
	org := newOrg(t, ada, "Acme")
	s, token := invite(t, ada, org, "cy@example.com", "developer")
	if s.Emailed {
		t.Fatal("a failed email reported as sent")
	}
	cy := c.SignIn("cy@example.com")
	cy.Do("POST", "/api/v1/join/preview", map[string]string{"token": token}, nil)
	if code, _ := accept(cy, token); code != 200 {
		t.Fatal(code)
	}
	mu.Lock()
	logs := buf.String()
	mu.Unlock()
	if !strings.Contains(logs, "emailing an invite failed") {
		t.Fatalf("the failure wasn't logged: %s", logs)
	}
	for _, secret := range []string{token, s.URL, "cy@example.com"} {
		if strings.Contains(logs, secret) {
			t.Errorf("%q is in the logs", secret)
		}
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
