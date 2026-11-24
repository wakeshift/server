package console_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/wakeshift/server/internal/coretest"
)

// orgLogCount is how many entries org's log has.
func orgLogCount(t *testing.T, c *coretest.Core, org string) int {
	t.Helper()
	var n int
	if err := c.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM org_log WHERE org_id = $1`, org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// newOrg makes an organization as u, returning its ID.
func newOrg(t *testing.T, u *coretest.User, name string) string {
	t.Helper()
	var o struct{ ID, Role string }
	if code := u.Do("POST", "/api/v1/orgs", map[string]string{"name": name}, &o); code != 200 || o.ID == "" || o.Role != "owner" {
		t.Fatalf("new organization: %d %+v", code, o)
	}
	return o.ID
}

// Every organization route, as each role and a non-member. Each allowed
// change writes exactly one org_log entry; a refused one writes none.
func TestOrgRoutesByRole(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com") // the owner
	bob := c.SignIn("bob@example.com") // an admin
	cy := c.SignIn("cy@example.com")   // a developer
	eve := c.SignIn("eve@example.com") // not a member
	dan := c.SignIn("dan@example.com") // a developer to remove
	org := newOrg(t, ada, "Acme")
	join(t, c, org, bob, "admin")
	join(t, c, org, cy, "developer")
	join(t, c, org, dan, "developer")
	cyID, danID, adaID := userID(t, cy), userID(t, dan), userID(t, ada)
	p := "/api/v1/orgs/" + org
	type want struct {
		code int
		logs int // entries the request adds
	}
	ok, okLog, no := want{200, 0}, want{200, 1}, want{403, 0}
	for _, r := range []struct {
		method, path, body string
		dev, admin, owner  want
	}{
		{"GET", p, "", ok, ok, ok},
		{"GET", p + "/members", "", ok, ok, ok},
		{"GET", p + "/log", "", ok, ok, ok},
		{"PATCH", p, `{"name": "Acme by ROLE"}`, no, okLog, okLog},
		// Admins and developers can't change roles at all.
		{"PATCH", p + "/members/" + cyID, `{"role": "admin"}`, no, no, okLog},
		{"PATCH", p + "/members/" + cyID, `{"role": "developer"}`, no, no, okLog},
		// Removing: admins remove developers, never admins or owners.
		{"GET", p + "/members/" + danID + "/made", "", no, ok, ok},
		{"DELETE", p + "/members/" + adaID, "", no, no, want{0, 0}},
	} {
		for _, m := range []struct {
			role string
			u    *coretest.User
			want want
		}{{"developer", cy, r.dev}, {"admin", bob, r.admin}, {"owner", ada, r.owner}} {
			if m.want.code == 0 {
				continue
			}
			var body any
			if r.body != "" {
				json.Unmarshal([]byte(strings.ReplaceAll(r.body, "ROLE", m.role)), &body)
			}
			before := orgLogCount(t, c, org)
			var e apiError
			code := m.u.Do(r.method, r.path, body, &e)
			if code != m.want.code || (code == 403 && e.Error.Code != "forbidden") {
				t.Errorf("a %s: %s %s: %d %+v; want %d", m.role, r.method, r.path, code, e, m.want.code)
			}
			if got := orgLogCount(t, c, org) - before; got != m.want.logs {
				t.Errorf("a %s: %s %s wrote %d log entries; want %d", m.role, r.method, r.path, got, m.want.logs)
			}
		}
		var e apiError
		if code := eve.Do(r.method, r.path, nil, &e); code != 404 || e.Error.Code != "not_found" {
			t.Errorf("a non-member: %s %s: %d %+v", r.method, r.path, code, e)
		}
	}
	var list struct {
		Orgs []struct {
			ID, Name, Role string
			Personal       bool
			Members        int
		}
	}
	if code := cy.Do("GET", "/api/v1/orgs", nil, &list); code != 200 || len(list.Orgs) != 2 || !list.Orgs[0].Personal ||
		list.Orgs[1].ID != org || list.Orgs[1].Role != "developer" || list.Orgs[1].Name != "Acme by owner" || list.Orgs[1].Members != 4 {
		t.Fatalf("cy's organizations: %d %+v", code, list)
	}

	// An admin removes a developer, but not an admin or an owner; a
	// developer removes no one.
	bobID := userID(t, bob)
	fay := c.SignIn("fay@example.com") // another admin
	join(t, c, org, fay, "admin")
	fayID := userID(t, fay)
	for _, r := range []struct {
		who    *coretest.User
		target string
		code   int
	}{
		{cy, danID, 403}, {cy, adaID, 403}, {bob, adaID, 403},
		// Two admins can't remove each other.
		{bob, fayID, 403}, {fay, bobID, 403},
		{bob, danID, 200},
	} {
		before := orgLogCount(t, c, org)
		if code := r.who.Do("DELETE", p+"/members/"+r.target, nil, nil); code != r.code {
			t.Errorf("removing %s: %d, want %d", r.target, code, r.code)
		}
		if want := map[bool]int{true: 1, false: 0}[r.code == 200]; orgLogCount(t, c, org)-before != want {
			t.Errorf("removing %s: log entries %d", r.target, orgLogCount(t, c, org)-before)
		}
	}
	if code := dan.Do("GET", p, nil, nil); code != 404 {
		t.Errorf("removed, dan: %d", code)
	}
	// Removing someone who isn't a member: 404.
	if code := ada.Do("DELETE", p+"/members/"+danID, nil, nil); code != 404 {
		t.Errorf("removing a non-member: %d", code)
	}
	// An admin can't promote themselves, nor demote an owner.
	if code := bob.Do("PATCH", p+"/members/"+bobID, map[string]string{"role": "owner"}, nil); code != 403 {
		t.Errorf("an admin promoting themselves: %d", code)
	}
	if code := bob.Do("PATCH", p+"/members/"+adaID, map[string]string{"role": "developer"}, nil); code != 403 {
		t.Errorf("an admin demoting the owner: %d", code)
	}
	if code := ada.Do("PATCH", p+"/members/"+bobID, map[string]string{"role": "boss"}, nil); code != 400 {
		t.Errorf("an unknown role: %d", code)
	}
	// Anyone leaves: a developer, then an admin.
	for _, u := range []*coretest.User{cy, bob} {
		before := orgLogCount(t, c, org)
		if code := u.Do("DELETE", p+"/members/"+userID(t, u), nil, nil); code != 200 {
			t.Errorf("leaving: %d", code)
		}
		if orgLogCount(t, c, org)-before != 1 {
			t.Error("leaving wasn't logged once")
		}
		if code := u.Do("GET", p, nil, nil); code != 404 {
			t.Errorf("left, still a member: %d", code)
		}
	}
	var entries struct {
		Entries []struct{ Action, ActorEmail string } `json:"entries"`
	}
	ada.Do("GET", p+"/log", nil, &entries)
	if len(entries.Entries) == 0 || entries.Entries[0].Action != "org.member_left" || entries.Entries[len(entries.Entries)-1].Action != "org.created" {
		t.Errorf("the log: %+v", entries)
	}
}

// An organization always has an owner: the last can't be demoted, removed
// or leave; two owners demoting each other at once can't both succeed.
func TestLastOwner(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	bob := c.SignIn("bob@example.com")
	org := newOrg(t, ada, "Acme")
	p := "/api/v1/orgs/" + org
	adaID, bobID := userID(t, ada), userID(t, bob)
	for _, r := range [][3]string{
		{"PATCH", "/members/" + adaID, `{"role": "admin"}`},
		{"DELETE", "/members/" + adaID, ""},
	} {
		var body any
		if r[2] != "" {
			json.Unmarshal([]byte(r[2]), &body)
		}
		before := orgLogCount(t, c, org)
		var e apiError
		if code := ada.Do(r[0], p+r[1], body, &e); code != 409 || e.Error.Code != "last_owner" {
			t.Errorf("the last owner: %s %s: %d %+v", r[0], r[1], code, e)
		}
		if orgLogCount(t, c, org) != before {
			t.Errorf("a refused change was logged")
		}
	}
	// Two owners demoting each other at once: one wins.
	for i := range 10 {
		join(t, c, org, bob, "owner")
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for j, r := range []struct {
			u      *coretest.User
			target string
		}{{ada, bobID}, {bob, adaID}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[j] = r.u.Do("PATCH", p+"/members/"+r.target, map[string]string{"role": "developer"}, nil)
			}()
		}
		wg.Wait()
		var owners int
		if err := c.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM organization_members WHERE org_id = $1 AND role = 'owner'`, org).Scan(&owners); err != nil {
			t.Fatal(err)
		}
		if owners != 1 {
			t.Fatalf("try %d: %d owners left (%v)", i, owners, codes)
		}
		// The other's request is either refused (last_owner) or, if it
		// came second, forbidden (a developer now).
		if (codes[0] == 200) == (codes[1] == 200) {
			t.Fatalf("try %d: both or neither succeeded: %v", i, codes)
		}
		// Back to ada as the owner.
		join(t, c, org, ada, "owner")
		join(t, c, org, bob, "developer")
	}
	// With two owners, one can leave; then the other can't.
	join(t, c, org, bob, "owner")
	if code := bob.Do("DELETE", p+"/members/"+bobID, nil, nil); code != 200 {
		t.Fatalf("an owner leaving: %d", code)
	}
	if code := ada.Do("DELETE", p+"/members/"+adaID, nil, nil); code != 409 {
		t.Fatalf("the last owner leaving: %d", code)
	}
}

// An organization is deleted only by an owner, and only without apps.
// Deleting it removes its members; its apps' pages are no one's.
func TestDeletingAnOrg(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	bob := c.SignIn("bob@example.com")
	org := newOrg(t, ada, "Acme")
	join(t, c, org, bob, "admin")
	p := "/api/v1/orgs/" + org
	if code := ada.Do("POST", "/api/v1/developer/apps", map[string]string{"org_id": org, "id": "acme-app", "name": "Acme"}, nil); code != 200 {
		t.Fatalf("app: %d", code)
	}
	if code := bob.Do("DELETE", p, nil, nil); code != 403 {
		t.Errorf("an admin deleting: %d", code)
	}
	var e apiError
	if code := ada.Do("DELETE", p, nil, &e); code != 409 || e.Error.Code != "has_apps" {
		t.Errorf("with an app: %d %+v", code, e)
	}
	if code := bob.Do("DELETE", "/api/v1/developer/apps/acme-app", nil, nil); code != 200 {
		t.Fatalf("an admin deleting the app: %d", code)
	}
	before := orgLogCount(t, c, org)
	if code := ada.Do("DELETE", p, nil, nil); code != 200 {
		t.Fatalf("deleting: %d", code)
	}
	if orgLogCount(t, c, org)-before != 1 {
		t.Error("deleting wasn't logged once")
	}
	for _, u := range []*coretest.User{ada, bob} {
		for _, path := range []string{p, p + "/members", "/api/v1/developer/apps/acme-app", "/api/v1/developer/apps/acme-app/log"} {
			if code := u.Do("GET", path, nil, nil); code != 404 {
				t.Errorf("after deleting: %s: %d", path, code)
			}
		}
	}
	// A personal organization can be deleted too; the console then lists
	// the others.
	var list struct{ Orgs []struct{ ID string } }
	ada.Do("GET", "/api/v1/orgs", nil, &list)
	if len(list.Orgs) != 1 {
		t.Fatalf("ada's organizations: %+v", list)
	}
	if code := ada.Do("DELETE", "/api/v1/orgs/"+list.Orgs[0].ID, nil, nil); code != 200 {
		t.Fatalf("deleting the personal organization: %d", code)
	}
	if code := ada.Do("GET", "/api/v1/orgs", nil, &list); code != 200 || len(list.Orgs) != 0 {
		t.Fatalf("after deleting both: %d %+v", code, list)
	}
	if code := ada.Do("POST", "/api/v1/developer/apps", map[string]string{"id": "orphan", "name": "Orphan"}, nil); code != 404 {
		t.Errorf("an app without an organization: %d", code)
	}
}

// Organizations' names are checked, and each user makes at most 5.
func TestCreatingOrgs(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	for _, name := range []string{"", "  ", strings.Repeat("x", 101), "<b>Acme</b>", "Two\nlines"} {
		if code := ada.Do("POST", "/api/v1/orgs", map[string]string{"name": name}, nil); code != 400 {
			t.Errorf("name %q: %d", name, code)
		}
	}
	for i := range 5 {
		newOrg(t, ada, "Org "+string(rune('A'+i)))
	}
	var e apiError
	if code := ada.Do("POST", "/api/v1/orgs", map[string]string{"name": "One more"}, &e); code != 400 || e.Error.Code != "too_many" {
		t.Errorf("a sixth: %d %+v", code, e)
	}
}

// The removal dialog lists the live keys and publish URLs a member made in
// the organization's apps.
func TestWhatAMemberMade(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	cy := c.SignIn("cy@example.com")
	org := newOrg(t, ada, "Acme")
	join(t, c, org, cy, "developer")
	if code := ada.Do("POST", "/api/v1/developer/apps", map[string]string{"org_id": org, "id": "acme-app", "name": "Acme"}, nil); code != 200 {
		t.Fatal(code)
	}
	var k madeApp
	cy.Do("POST", "/api/v1/developer/apps/acme-app/keys", map[string]string{"name": "cy's laptop"}, &k)
	noted(t, c, k.Key)
	if code := cy.Do("POST", "/api/v1/developer/apps/acme-app/publish-urls", map[string]any{"event": "note.one", "arguments": map[string]any{"a": "builds"}}, nil); code != 200 {
		t.Fatal(code)
	}
	var made struct {
		Keys        []struct{ AppID, KeyID, Name string } `json:"keys"`
		PublishURLs []struct{ AppID, Topic string }       `json:"publish_urls"`
	}
	if code := ada.Do("GET", "/api/v1/orgs/"+org+"/members/"+userID(t, cy)+"/made", nil, &made); code != 200 ||
		len(made.Keys) != 1 || made.Keys[0].Name != "cy's laptop" || len(made.PublishURLs) != 1 || made.PublishURLs[0].Topic != "note.one" {
		t.Fatalf("what cy made: %+v", made)
	}
}
