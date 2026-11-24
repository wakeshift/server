package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/wakeshift/server/internal/accounts"
)

const (
	githubCookie = "ws_signin_gh"
	githubPath   = "/auth/github"
)

// GitHub is "Sign in with GitHub": an OAuth app's authorization code flow
// with PKCE and state. GitHub isn't OpenID Connect, so the identity comes
// from its API: the account's ID (stable, unlike the login) and its primary
// verified email.
type GitHub struct {
	ClientID     string
	ClientSecret string
	// Web and API are GitHub's; tests point them at a fake.
	Web, API string
}

func (g *GitHub) web() string {
	if g.Web != "" {
		return strings.TrimSuffix(g.Web, "/")
	}
	return "https://github.com"
}

func (g *GitHub) api() string {
	if g.API != "" {
		return strings.TrimSuffix(g.API, "/")
	}
	return "https://api.github.com"
}

func (c *Console) githubConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.GitHub.ClientID,
		ClientSecret: c.GitHub.ClientSecret,
		Endpoint: oauth2.Endpoint{AuthURL: c.GitHub.web() + "/login/oauth/authorize", TokenURL: c.GitHub.web() + "/login/oauth/access_token",
			AuthStyle: oauth2.AuthStyleInParams},
		RedirectURL: strings.TrimSuffix(c.PublicURL, "/") + githubPath + "/callback",
		Scopes:      []string{"read:user", "user:email"},
	}
}

func (c *Console) githubStart(w http.ResponseWriter, r *http.Request) {
	f := c.newFlow(r)
	c.clear(w, joinCookie, joinPath) // carried in the flow now
	http.SetCookie(w, c.cookie(githubCookie, githubPath, c.sign("sign-in-github", f), c.now().Add(flowTTL), http.SameSiteLaxMode))
	http.Redirect(w, r, c.githubConfig().AuthCodeURL(f.State, oauth2.S256ChallengeOption(f.Verifier)), http.StatusFound)
}

func (c *Console) githubCallback(w http.ResponseWriter, r *http.Request) {
	c.clear(w, githubCookie, githubPath)
	u, f, err := c.finishGitHub(r)
	c.finish(w, r, "GitHub", u, f, err)
}

func (c *Console) finishGitHub(r *http.Request) (accounts.User, flow, error) {
	f, err := c.readFlow(r, githubCookie, "sign-in-github")
	if err != nil {
		return accounts.User{}, f, err
	}
	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, githubClient)
	tok, err := c.githubConfig().Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		return accounts.User{}, f, err
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := c.githubGet(ctx, tok.AccessToken, "/user", &user); err != nil {
		return accounts.User{}, f, err
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := c.githubGet(ctx, tok.AccessToken, "/user/emails", &emails); err != nil {
		return accounts.User{}, f, err
	}
	email := ""
	for _, e := range emails {
		if e.Primary && e.Verified {
			email = e.Email
		}
	}
	if user.ID == 0 || email == "" {
		return accounts.User{}, f, errors.New("no verified primary email")
	}
	name := user.Name
	if name == "" {
		name = user.Login
	}
	u, err := c.Accounts.SignIn(ctx, accounts.Identity{Provider: "github", Subject: strconv.FormatInt(user.ID, 10), Email: email, Name: name, Invite: f.Invite, OrgInvite: f.OrgInvite, Ref: f.Ref})
	return u, f, err
}

func (c *Console) githubGet(ctx context.Context, token, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.GitHub.api()+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := githubClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("auth: GitHub API", "path", path, "status", resp.StatusCode)
		return fmt.Errorf("GitHub %s: %d", path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// githubClient is for GitHub's token endpoint and API, during a sign-in.
var githubClient = &http.Client{Timeout: 15 * time.Second}
