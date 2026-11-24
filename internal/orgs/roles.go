package orgs

import "errors"

// Role is a member's role in an organization.
type Role string

// Roles. Owners and admins manage people; owners alone manage owners and
// admins, and can delete the organization. Developers do the day-to-day
// work on apps, keys included, and read everything.
const (
	Owner     Role = "owner"
	Admin     Role = "admin"
	Developer Role = "developer"
)

// Valid says whether r is a role.
func (r Role) Valid() bool { return r == Owner || r == Admin || r == Developer }

// Action is something a member may or may not do.
type Action string

// Actions on an organization's apps (the developer API, /api/v1/developer/apps).
const (
	AppRead            Action = "app.read"             // every app page: overview, keys, log, subscriptions, activity, users, publish URLs
	AppCreate          Action = "app.create"           // register an app in the organization
	AppUpdate          Action = "app.update"           // Settings: name, MCP URL
	AppDelete          Action = "app.delete"           // irreversible: its users' data goes
	AppKeys            Action = "app.keys"             // make, rename, rotate and revoke keys
	AppEndSubscription Action = "app.end_subscription" // end one of its subscriptions
	AppForgetUser      Action = "app.forget_user"      // irreversible: the user's workspace goes
	AppPublishURLs     Action = "app.publish_urls"     // make and revoke publish URLs
)

// Actions on the organization itself (/api/v1/orgs). Changing members and
// invites depends on whose role is touched too: CanManage and CanGrant.
const (
	OrgRead        Action = "org.read"         // its settings, members and log
	OrgRename      Action = "org.rename"       // Settings: name
	OrgDelete      Action = "org.delete"       // Settings: danger zone
	OrgInvitesRead Action = "org.invites_read" // its pending invites
	OrgInvite      Action = "org.invite"       // send, resend and revoke invites (for roles it may grant)
	OrgRemove      Action = "org.remove"       // remove others (whose role it may manage)
	OrgChangeRoles Action = "org.change_roles" // change members' roles
)

// can is the table: which roles may do what. Everything not listed is
// refused.
var can = map[Action][]Role{
	AppRead:            {Owner, Admin, Developer},
	AppCreate:          {Owner, Admin, Developer},
	AppUpdate:          {Owner, Admin},
	AppDelete:          {Owner, Admin},
	AppKeys:            {Owner, Admin, Developer},
	AppEndSubscription: {Owner, Admin, Developer},
	AppForgetUser:      {Owner, Admin},
	AppPublishURLs:     {Owner, Admin, Developer},

	OrgRead:        {Owner, Admin, Developer},
	OrgRename:      {Owner, Admin},
	OrgDelete:      {Owner},
	OrgInvitesRead: {Owner, Admin},
	OrgInvite:      {Owner, Admin},
	OrgRemove:      {Owner, Admin},
	OrgChangeRoles: {Owner},
}

// Can says whether a member with role r may do a.
func (r Role) Can(a Action) bool {
	for _, ok := range can[a] {
		if ok == r {
			return true
		}
	}
	return false
}

// CanManage says whether r may remove a member whose role is target, or
// revoke or resend an invite for that role: owners manage everyone; admins
// only developers. (Leaving, removing yourself, isn't managing.)
func (r Role) CanManage(target Role) bool {
	switch r {
	case Owner:
		return target.Valid()
	case Admin:
		return target == Developer
	}
	return false
}

// CanGrant says whether r may give someone role g, by inviting them or
// changing their role: owners any role; admins only developer (by
// invite: admins can't change roles at all). So no one raises anyone,
// themselves included, above their own level.
func (r Role) CanGrant(g Role) bool { return r.CanManage(g) }

var (
	// ErrForbidden is a member whose role doesn't allow the action (403).
	ErrForbidden = errors.New("orgs: your role in this organization doesn't allow that")
	// ErrNotMember is an organization that doesn't exist, is deleted or
	// isn't one of yours (404, alike, so a guessed ID tells you nothing).
	ErrNotMember = errors.New("orgs: no such organization of yours")
)
