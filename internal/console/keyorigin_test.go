package console_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/mail"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// A key carries its maker's role: through the app API, a key a developer
// made can't forget a user (owners and admins only, as in the console),
// while it still does everything else; an owner's or admin's key can. A key
// a developer rotates is theirs, and a key whose maker left keeps working
// but can't forget.
func TestDeveloperKeyCantForgetUsers(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com") // the owner
	bob := c.SignIn("bob@example.com") // an admin
	cy := c.SignIn("cy@example.com")   // a developer
	org := orgOf(t, ada)
	join(t, c, org, bob, "admin")
	join(t, c, org, cy, "developer")
	made := createApp(t, ada, "quizzer", "Quizzer", "")
	owners := noted(t, c, made.Key)

	newKey := func(u *coretest.User, name string) *wakeshift.Client {
		t.Helper()
		var k madeApp
		if code := u.Do("POST", "/api/v1/developer/apps/quizzer/keys", map[string]string{"name": name}, &k); code != 200 {
			t.Fatalf("%s's key: %d", name, code)
		}
		return kit(t, c, k.Key, "")
	}
	admins, devs := newKey(bob, "bob"), newKey(cy, "cy")
	subscribe := func(user string) {
		t.Helper()
		if _, err := coretest.Subscribe(owners, coretest.Caller{User: user, Client: "claude-code"}, "game/1", coretest.SubOpts{}); err != nil {
			t.Fatal(err)
		}
	}

	// The developer's key works for the app's ordinary calls...
	if _, err := coretest.Subscribe(devs, coretest.Caller{User: "player-1", Client: "claude-code"}, "game/1", coretest.SubOpts{}); err != nil {
		t.Fatalf("a developer's key subscribing: %v", err)
	}
	// ...but not to forget a user, whose workspace stays.
	if _, err := devs.ForgetUser(ctx, "player-1"); !wakeshift.IsCode(err, "forbidden") {
		t.Fatalf("a developer's key forgetting: %v, want forbidden", err)
	}
	if got, err := owners.ForgetUser(ctx, "player-1"); err != nil || !got {
		t.Fatalf("the owner's key forgetting after: %v %v (the workspace should have stayed)", got, err)
	}
	subscribe("player-2")
	if got, err := admins.ForgetUser(ctx, "player-2"); err != nil || !got {
		t.Fatalf("an admin's key forgetting: %v %v", got, err)
	}

	// A key the developer rotates is the developer's, whoever made the old one.
	var rotated madeApp
	var keys struct {
		Keys []struct {
			ID   string `json:"key_id"`
			Name string
		}
	}
	ada.Do("GET", "/api/v1/developer/apps/quizzer/keys", nil, &keys)
	var bobsKey string
	for _, k := range keys.Keys {
		if k.Name == "bob" {
			bobsKey = k.ID
		}
	}
	if code := cy.Do("POST", "/api/v1/developer/apps/quizzer/keys/"+bobsKey+"/rotate", nil, &rotated); code != 200 {
		t.Fatalf("rotate: %d", code)
	}
	subscribe("player-3")
	if _, err := kit(t, c, rotated.Key, "").ForgetUser(ctx, "player-3"); !wakeshift.IsCode(err, "forbidden") {
		t.Fatalf("a key the developer rotated, forgetting: %v, want forbidden", err)
	}

	// Bob leaves the organization: his key keeps working, but can't forget.
	again := newKey(bob, "bob again")
	if _, err := c.DB.Pool.Exec(ctx, `DELETE FROM organization_members WHERE org_id = $1 AND user_id = $2`, org, userID(t, bob)); err != nil {
		t.Fatal(err)
	}
	if _, err := again.AppID(ctx); err != nil {
		t.Fatalf("a left member's key: %v", err)
	}
	// Made while he was an admin, it keeps the admin's role: a key outlives
	// its maker's membership, as the removal dialog says (rotate it).
	if got, err := again.ForgetUser(ctx, "player-3"); err != nil || !got {
		t.Fatalf("an admin's key after the admin left: %v %v", got, err)
	}
	// A key whose maker's role isn't known (one from before the role was
	// recorded, whose maker has left) can't forget.
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE app_keys SET created_role = NULL WHERE app_id = 'quizzer' AND name = 'bob again'`); err != nil {
		t.Fatal(err)
	}
	subscribe("player-4")
	if _, err := again.ForgetUser(ctx, "player-4"); !wakeshift.IsCode(err, "forbidden") {
		t.Fatalf("a key of unknown role, forgetting: %v, want forbidden", err)
	}
	if _, err := again.AppID(ctx); err != nil {
		t.Fatalf("a key of unknown role, for ordinary calls: %v", err)
	}
}

// A key-registration link can be someone else's. Registering one takes the
// key code typed from the terminal (the page never shows it); on an app
// that exists only owners and admins may; the key is marked via cli_link;
// and the organization's owners and admins are told, in its log and by
// email.
func TestLinkRegistrationGuards(t *testing.T) {
	fake := &mail.Fake{}
	c := coretest.StartWith(t, withMail(fake))
	ada := c.SignIn("ada@example.com") // the owner
	bob := c.SignIn("bob@example.com") // an admin
	cy := c.SignIn("cy@example.com")   // a developer
	org := orgOf(t, ada)
	join(t, c, org, bob, "admin")
	join(t, c, org, cy, "developer")
	createApp(t, ada, "quizzer", "Quizzer", "")
	if _, err := c.DB.Pool.Exec(context.Background(), `UPDATE organizations SET name = 'Acme — verify at evil.example' WHERE id = $1`, org); err != nil {
		t.Fatal(err)
	}

	k := newCLIKey(t)
	post := func(u *coretest.User, body map[string]string) (int, registered) {
		var out registered
		code := u.Do("POST", "/api/v1/developer/apps/quizzer/keys/register", body, &out)
		return code, out
	}
	// No code, or another key's, is refused, and nothing is added.
	if code, out := post(ada, map[string]string{"public_key": k.link()}); code != 400 || out.Error.Code != "key_code_required" {
		t.Fatalf("no code: %d %+v", code, out.Error)
	}
	if code, out := post(ada, map[string]string{"public_key": k.link(), "key_code": newCLIKey(t).code()}); code != 400 || out.Error.Code != "wrong_key_code" {
		t.Fatalf("another key's code: %d %+v", code, out.Error)
	}
	// A developer can't add a link's key to an app that exists.
	if code, out := post(cy, map[string]string{"public_key": k.link(), "key_code": k.code()}); code != 403 || out.Error.Code != "forbidden" {
		t.Fatalf("a developer: %d %+v", code, out.Error)
	}
	if len(fake.Sent()) != 0 {
		t.Fatalf("emails before any key was added: %d", len(fake.Sent()))
	}
	// An admin can, typing the code in any case with spaces around it.
	if code, out := post(bob, map[string]string{"public_key": k.link(), "key_code": "  " + strings.ToLower(k.code()) + " "}); code != 200 || out.KeyInfo.ID != k.code() {
		t.Fatalf("an admin: %d %+v", code, out)
	}

	var keys struct {
		Keys []struct {
			ID  string `json:"key_id"`
			Via string
		}
	}
	ada.Do("GET", "/api/v1/developer/apps/quizzer/keys", nil, &keys)
	vias := map[string]string{}
	for _, key := range keys.Keys {
		vias[key.ID] = key.Via
	}
	if vias[k.code()] != "cli_link" || len(vias) != 2 {
		t.Fatalf("keys: %+v", keys)
	}
	for id, via := range vias {
		if id != k.code() && via != "" {
			t.Errorf("the console's key %s: via %q", id, via)
		}
	}

	// The organization's log says so; every member can read it.
	var log struct {
		Entries []struct {
			Actor  string
			Action string
			Detail map[string]any
		}
	}
	if code := cy.Do("GET", "/api/v1/orgs/"+org+"/log", nil, &log); code != 200 {
		t.Fatalf("log: %d", code)
	}
	found := false
	for _, e := range log.Entries {
		if e.Action == "org.app_key_from_link" {
			found = e.Detail["app_id"] == "quizzer" && e.Detail["key_id"] == k.code() && e.Detail["via"] == "cli_link" && e.Detail["new_app"] == false &&
				e.Actor == "user:"+userID(t, bob)
		}
	}
	if !found {
		t.Fatalf("no org.app_key_from_link entry: %+v", log)
	}
	// Each owner and admin is emailed, not the developer; the subject is
	// fixed, and the organization's name, which reads as a link, isn't in it.
	sent := fake.Sent()
	to := map[string]bool{}
	for _, m := range sent {
		to[m.To] = true
		if m.Subject != "A key was added to your organization's app from a CLI link" || strings.Contains(m.Text+m.HTML, "evil") ||
			!strings.Contains(m.Text, "bob@example.com added key "+k.code()+" to the app quizzer") || !strings.Contains(m.Text, "/apps/quizzer/keys") {
			t.Errorf("email to %s: %q\n%s", m.To, m.Subject, m.Text)
		}
	}
	if len(sent) != 2 || !to["ada@example.com"] || !to["bob@example.com"] {
		t.Fatalf("emailed %v, want the owner and the admin", to)
	}

	// A new app from a link: any role may, with the code; the owners and
	// admins are told.
	nk := newCLIKey(t)
	if code, out := register(cy, map[string]string{"org_id": org, "id": "cy-app", "name": "Cy", "public_key": nk.link(), "key_code": ""}); code != 400 || out.Error.Code != "key_code_required" {
		t.Fatalf("a new app without the code: %d %+v", code, out.Error)
	}
	if code, out := register(cy, map[string]string{"org_id": org, "id": "cy-app", "name": "Cy", "public_key": nk.link(), "key_code": nk.code()}); code != 200 {
		t.Fatalf("a new app: %d %+v", code, out.Error)
	}
	if n := len(fake.Sent()); n != 4 {
		t.Fatalf("emails after the new app: %d, want 4", n)
	}
	if last := fake.Sent()[3]; !strings.Contains(last.Text, "cy@example.com created the app cy-app") {
		t.Errorf("new app's email:\n%s", last.Text)
	}
}
