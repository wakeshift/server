package orgs

import (
	"strings"
	"testing"
)

// Which roles may take each action, row by row.
func TestRolesCan(t *testing.T) {
	for a, want := range map[Action]string{
		AppRead: "owner admin developer", AppCreate: "owner admin developer", AppUpdate: "owner admin", AppDelete: "owner admin",
		AppKeys: "owner admin developer", AppLinkKey: "owner admin", AppEndSubscription: "owner admin developer", AppForgetUser: "owner admin",
		AppPublishURLs: "owner admin developer",
		OrgRead:        "owner admin developer", OrgRename: "owner admin", OrgDelete: "owner", OrgInvitesRead: "owner admin",
		OrgInvite: "owner admin", OrgRemove: "owner admin", OrgChangeRoles: "owner",
		"no.such.action": "",
	} {
		for _, r := range []Role{Owner, Admin, Developer, "", "superuser"} {
			if got, ok := r.Can(a), r != "" && strings.Contains(" "+want+" ", " "+string(r)+" "); got != ok {
				t.Errorf("%q can %s: %v, want %v", r, a, got, ok)
			}
		}
	}
}

// Owners manage and grant every role; admins only developers; developers
// nothing. No one can raise anyone above their own level.
func TestRolesManageAndGrant(t *testing.T) {
	for _, c := range []struct {
		actor, target Role
		ok            bool
	}{
		{Owner, Owner, true}, {Owner, Admin, true}, {Owner, Developer, true},
		{Admin, Owner, false}, {Admin, Admin, false}, {Admin, Developer, true},
		{Developer, Owner, false}, {Developer, Admin, false}, {Developer, Developer, false},
		{Owner, "superuser", false}, {"", Developer, false},
	} {
		if got := c.actor.CanManage(c.target); got != c.ok {
			t.Errorf("%s manages %s: %v", c.actor, c.target, got)
		}
		if got := c.actor.CanGrant(c.target); got != c.ok {
			t.Errorf("%s grants %s: %v", c.actor, c.target, got)
		}
	}
}
