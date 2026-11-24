package auth_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/orgs"
)

// inviteFor makes an organization invite from ada (signed up through the
// allow list) to email, returning its token.
func inviteFor(t *testing.T, a *app, email string) string {
	t.Helper()
	ctx := context.Background()
	ada, err := a.accounts.SignIn(ctx, accounts.Identity{Provider: "google", Subject: "ada", Email: "ada@example.com", Name: "Ada", HostedDomain: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	var token string
	err = pgx.BeginFunc(ctx, a.accounts.Pool, func(tx pgx.Tx) error {
		org, err := orgs.Default(ctx, tx, ada.ID)
		if err != nil {
			return err
		}
		s, err := orgs.CreateInvite(ctx, tx, ada.ID, org, email, orgs.Developer)
		token = s.Token
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// keepToken is the join page handing the invite's token to the sign-in
// that follows (a cookie, never the URL).
func keepToken(t *testing.T, a *app, token, origin string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", a.URL+"/api/v1/join/sign-in", strings.NewReader(`{"token": "`+token+`"}`))
	req.Header.Set("Origin", origin)
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Signing up with Google through an organization invite: only for the
// invited address, joining the organization at once; the token travels in
// signed cookies, never a URL.
func TestSignUpThroughAnOrgInvite(t *testing.T) {
	a := setup(t, "ada@example.com") // only ada is on the allow list
	token := inviteFor(t, a, "cy@example.com")
	if code := keepToken(t, a, token, "https://evil.example"); code != 403 {
		t.Fatalf("another site keeping a token: %d", code)
	}

	// Another address, through the same invite: refused, back to the join
	// page.
	if code := keepToken(t, a, token, a.URL); code != 200 {
		t.Fatalf("keeping the token: %d", code)
	}
	eve := map[string]any{"sub": "google-eve", "email": "eve@example.com", "email_verified": true, "name": "Eve"}
	if to := a.signIn(t, eve, nil); to != "/join?sign_in_error=org_invite_email" {
		t.Fatalf("eve redirected to %s", to)
	}
	if a.session(t)["authenticated"] == true {
		t.Fatal("eve signed up")
	}

	// The invited address: signed up, and a member.
	a.client.Jar, _ = cookiejar.New(nil)
	keepToken(t, a, token, a.URL)
	cy := map[string]any{"sub": "google-cy", "email": "cy@example.com", "email_verified": true, "name": "Cy"}
	resp, err := a.client.Get(a.URL + "/auth/google/start?return_to=/join")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if strings.Contains(resp.Header.Get("Location"), token) {
		t.Fatal("the token is in the provider's URL")
	}
	a.client.Jar, _ = cookiejar.New(nil)
	keepToken(t, a, token, a.URL)
	if to := a.signIn(t, cy, nil); to != "/" {
		t.Fatalf("cy redirected to %s", to)
	}
	var role string
	if err := a.accounts.Pool.QueryRow(context.Background(), `SELECT m.role FROM organization_members m JOIN users u ON u.id = m.user_id
		WHERE u.email = 'cy@example.com' AND m.org_id IN (SELECT id FROM organizations WHERE personal_of IS DISTINCT FROM u.id)`).Scan(&role); err != nil || role != "developer" {
		t.Fatalf("cy's membership: %q %v", role, err)
	}
	// The invite is spent: someone else signing up with it is refused.
	a.client.Jar, _ = cookiejar.New(nil)
	keepToken(t, a, token, a.URL)
	again := map[string]any{"sub": "google-cy-2", "email": "cy2@example.com", "email_verified": true}
	if to := a.signIn(t, again, nil); to != "/join?sign_in_error=org_invite_invalid" {
		t.Fatalf("a second sign-up redirected to %s", to)
	}
}
