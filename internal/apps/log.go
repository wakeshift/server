package apps

// The app log (028_app_log.sql): an app's own history, which
// its organization's members read on the app's page. Apps are platform-level, so the log is
// too, outside any workspace; entries are written in the same transaction
// as the action they record, so one exists exactly when its action
// committed.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wakeshift/server/internal/orgs"
)

// Log kinds. Request logs and the delivery inspector add theirs later.
const KindAudit = "audit"

// Log actions.
const (
	LogCreated           = "app.created"            // by a member of its organization, in the console
	LogKeyCreated        = "app.key_created"        // a new key; the others keep working
	LogKeyRevoked        = "app.key_revoked"        // it stopped working at once
	LogKeyRotated        = "app.key_rotated"        // a new key in place of one, which stopped working at once
	LogKeyRenamed        = "app.key_renamed"        // its name changed: old_name and name
	LogUpdated           = "app.updated"            // its name or MCP URL changed
	LogKeyFirstUsed      = "app.key_first_used"     // once per key: the first request it signed
	LogManifestPublished = "app.manifest_published" // only when something changed, with what
	LogDeleted           = "app.deleted"            // its key stopped working
	LogGrantsRevoked     = "app.grants_revoked"     // after a delete, in the workspaces it kept
	LogUserForgotten     = "app.user_forgotten"     // its workspace deleted; the user as a hash, never their ID
	LogUsersDataDeleted  = "app.users_data_deleted" // by Wakeshift after a delete: what it kept for the app's users is gone
	// From the console (the app's own calls are in its users' workspaces'
	// audit logs).
	LogSubscriptionEnded = "app.subscription_ended" // by a member of its organization
	// Publish URLs are credentials: anyone with one can emit its event.
	// Logged however they're made (the SDK or the console).
	LogPublishURLCreated = "app.publish_url_created"
	LogPublishURLRevoked = "app.publish_url_revoked"
	// Its signing secret (Standard Webhooks): the detail names the URL and
	// the secret's fingerprint, never the secret.
	LogPublishURLSecretSet     = "app.publish_url_secret_set"
	LogPublishURLSecretRotated = "app.publish_url_secret_rotated" // the previous one still works for a day
	LogPublishURLSecretRemoved = "app.publish_url_secret_removed" // posts are no longer checked
)

// LogEntry is one thing that happened to an app.
type LogEntry struct {
	AppID  string
	Actor  string // user:<id>, app:<id> or hub
	Action string
	KeyID  string // the key it's about (a KeyID, never the key)
	Detail any    // marshaled to a JSON object; keep it small
}

// execer is a transaction or a pool.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Log appends e, in the caller's transaction.
func Log(ctx context.Context, tx execer, e LogEntry) error {
	detail := []byte("{}")
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return err
		}
		detail = b
	}
	_, err := tx.Exec(ctx, `INSERT INTO app_log (app_id, kind, actor, action, key_id, detail, org_id)
		VALUES ($1, $2, $3, $4, nullif($5, ''), $6, (SELECT org_id FROM apps WHERE id = $1))`,
		e.AppID, KindAudit, e.Actor, e.Action, e.KeyID, detail)
	return err
}

// LogItem is a logged entry, as the app's organization's members see it.
type LogItem struct {
	ID         int64           `json:"id"`
	At         time.Time       `json:"at"`
	Kind       string          `json:"kind"`
	Actor      string          `json:"actor"`
	ActorEmail string          `json:"actor_email,omitempty"` // for a user
	Action     string          `json:"action"`
	KeyID      string          `json:"key_id,omitempty"`
	Detail     json.RawMessage `json:"detail"`
}

// MaxLogLimit is the most entries ListLog returns at once.
const MaxLogLimit = 100

// ListLog returns up to limit of the app's entries, newest first, older
// than before (an entry ID; 0 for the newest), whose action starts with
// action ("" for any; e.g. "app.key_" for its keys). It's ErrNotOwner
// unless user is a member of its organization; a deleted app's log stays
// readable by its organization's members.
func (r *Registry) ListLog(ctx context.Context, tx pgx.Tx, user, id string, before int64, limit int, action string) ([]LogItem, error) {
	if limit <= 0 || limit > MaxLogLimit {
		limit = MaxLogLimit
	}
	if _, err := r.access(ctx, tx, user, id, orgs.AppRead, false); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT l.id, l.at, l.kind, l.actor, coalesce(u.email, ''), l.action, coalesce(l.key_id, ''), l.detail
		FROM app_log l LEFT JOIN users u ON l.actor = 'user:' || u.id::text
		WHERE l.app_id = $1 AND ($2 = 0 OR l.id < $2) AND starts_with(l.action, $4) ORDER BY l.id DESC LIMIT $3`, id, before, limit, action)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LogItem, error) {
		var it LogItem
		err := row.Scan(&it.ID, &it.At, &it.Kind, &it.Actor, &it.ActorEmail, &it.Action, &it.KeyID, &it.Detail)
		return it, err
	})
	if out == nil {
		out = []LogItem{}
	}
	return out, err
}

// maxChanges is the most changes a manifest entry lists.
const maxChanges = 20

// namesOrNone is a list of names for a log line.
func namesOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

// manifestChanges says, in short lines, what changed from old to m: what
// the app's developers want to see when a publish changes what the app may do.
// Nothing changed is an empty list.
func manifestChanges(old, m Manifest) []string {
	out := []string{}
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if old.Description != m.Description {
		add("description changed")
	}
	if old.OthersCanWake != m.OthersCanWake {
		add("others can wake: %t -> %t", old.OthersCanWake, m.OthersCanWake)
	}
	switch oa, na := old.Agent, m.Agent; {
	case oa == nil && na != nil:
		add("agent scope added")
		agentChanges(add, &AgentScope{}, na)
	case oa != nil && na == nil:
		add("agent scope removed")
	case oa != nil:
		agentChanges(add, oa, na)
	}
	eventTypeChanges(add, old.Events, m.Events)
	if !slices.Equal(old.PreapproveTools, m.PreapproveTools) {
		add("preapprove tools: %s -> %s", namesOrNone(old.PreapproveTools), namesOrNone(m.PreapproveTools))
	}
	if len(out) > maxChanges {
		out = append(out[:maxChanges], fmt.Sprintf("and %d more", len(out)-maxChanges))
	}
	return out
}

func agentChanges(add func(string, ...any), o, n *AgentScope) {
	eventChanges(add, "wake", o.Wake, n.Wake)
	eventChanges(add, "post", o.Post, n.Post)
	if !slices.Equal(o.WakeLimits, n.WakeLimits) {
		add("wake limits changed")
	}
	if o.TTLDays != n.TTLDays {
		add("agent ttl_days: %d -> %d", o.TTLDays, n.TTLDays)
	}
	if o.StartPrompt != n.StartPrompt {
		add("start prompt changed")
	}
}

// eventChanges lists patterns added (+), removed (-) and re-described (~).
func eventChanges(add func(string, ...any), what string, o, n []ScopeEvent) {
	before := map[string]string{}
	for _, e := range o {
		before[e.Pattern] = e.Description
	}
	after := map[string]bool{}
	for _, e := range n {
		after[e.Pattern] = true
		d, ok := before[e.Pattern]
		switch {
		case !ok:
			add("%s + %s", what, e.Pattern)
		case d != e.Description:
			add("%s ~ %s (description)", what, e.Pattern)
		}
	}
	for _, e := range o {
		if !after[e.Pattern] {
			add("%s - %s", what, e.Pattern)
		}
	}
}

func eventTypeChanges(add func(string, ...any), o, n []EventType) {
	before := map[string]EventType{}
	for _, e := range o {
		before[e.Name] = e
	}
	for _, e := range n {
		was, ok := before[e.Name]
		delete(before, e.Name)
		switch {
		case !ok:
			add("event %s added", e.Name)
		case was.Topic != e.Topic || !sameJSON(was.Arguments, e.Arguments) || !sameJSON(was.Payload, e.Payload) || !maps.Equal(was.Filter, e.Filter):
			add("event %s changed", e.Name)
		case was.DeleteOnDelivery != e.DeleteOnDelivery && e.DeleteOnDelivery:
			add("event %s: content now deleted once delivered", e.Name)
		case was.DeleteOnDelivery != e.DeleteOnDelivery:
			add("event %s: content now kept after delivery", e.Name)
		case was.Description != e.Description:
			add("event %s: description changed", e.Name)
		}
		if ok && effectivePolicy(was.SubscribePolicy) != effectivePolicy(e.SubscribePolicy) {
			add("event %s: subscribe %s -> %s", e.Name, effectivePolicy(was.SubscribePolicy), effectivePolicy(e.SubscribePolicy))
		}
	}
	for _, e := range o {
		if _, ok := before[e.Name]; ok {
			add("event %s removed", e.Name)
		}
	}
}

// sameJSON says a and b are the same JSON value. The stored manifest's
// schemas come back from jsonb with their keys and spacing changed, so an
// unchanged event compares by value, not by its bytes.
func sameJSON(a, b json.RawMessage) bool {
	if string(a) == string(b) {
		return true
	}
	ca, errA := Canonical(a)
	cb, errB := Canonical(b)
	return errA == nil && errB == nil && string(ca) == string(cb)
}
