// Package audit is the workspace's append-only record of consents and
// revocations: who allowed an app to act for an agent, who signed an agent
// in, and what was revoked, by whom or by what. Entries are written in the
// caller's transaction, so one exists exactly when its action committed.
package audit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// Actions.
const (
	GrantCreated       = "grant.created" // a user consented (the grant keeps what the screen showed)
	GrantRevoked       = "grant.revoked" // revoked, with what that undid
	GrantExpired       = "grant.expired" // revoked by the hub at its end date
	SignInAuthorized   = "signin.authorized"
	SignInRevoked      = "signin.revoked" // a sign-in (refresh token family) ended: by its client, or on reuse
	AppConnected       = "app.connected"  // the user sent to an app to finish connecting it to the workspace
	StreamLinksRevoked = "stream_links.revoked"
	// StreamLinkCreated: a user minted a stream link for an agent in the
	// console (detail: link, its ID; ack; expires_at). Never the URL.
	StreamLinkCreated = "stream_link.created"
	ConnectorLinked   = "connector.linked" // a connector redeemed a beacon's ticket for a session's stream link
	// ConnectorCallKeyMismatch (redeem by call): a redeem by a call's
	// ID was signed by another key than the one that claimed the call
	// (detail: session, outcome, the key's fingerprint; never the call ID).
	ConnectorCallKeyMismatch = "connector.call_key_mismatch"
	SessionUnwatched         = "session.unwatched"  // a conversation stopped watching: its links revoked
	SessionSuperseded        = "session.superseded" // a conversation's hook connected it to a newer session: the old one ended
	// StreamLinkRenewed: a session's stream link that expired while
	// nothing used it (a laptop asleep) was renewed within a day of its
	// expiry (streams.RenewGrace): its holder got a successor and the link
	// was revoked, so it renews once (detail: session, link, successor,
	// kind "connector" or "session", expired_seconds). IDs only, never a
	// link's URL or token.
	StreamLinkRenewed    = "stream_link.renewed"
	SubscriptionCreated  = "subscription.created"  // by a user in the console (apps' are inside their grant)
	SubscriptionCanceled = "subscription.canceled" // "subscription.cancelled" in entries from before (List reports them as this)
	// DeviceConnected and DeviceRevoked: a user approved a device
	// (an earlier desktop app) to act for them, and its revocation. Nothing
	// issues a device any more; the names stay so older entries still
	// read, and migration 122 recorded DeviceRevoked (actor Hub,
	// detail.reason "wakeshift_for_mac_removed") for each device it
	// revoked.
	DeviceConnected = "device.connected"
	DeviceRevoked   = "device.revoked"
	// AppDataDeleted: the app was deleted (detail.reason "app_deleted"; or,
	// in entries from before, "app_headless": it switched off account mode),
	// and what the workspace kept for it (its grants, subscriptions, events
	// and deliveries) was deleted by Wakeshift. The workspace's audit log keeps this entry and the earlier
	// ones about the app: they're the workspace's own record.
	AppDataDeleted = "app.data_deleted"
	// AccountCreated: the user signed up through an organization invite
	// (detail.via "org_invite", with the organization, the invite and who
	// sent it), in their new personal workspace.
	AccountCreated = "account.created"
	// KeyRegistered: the user confirmed, on the console's page from
	// @wakeshift/cli's link, a key made on their machine as one of an
	// app's keys (detail: org_id, key_id, via "cli", new_app whether it
	// made the app too), in their personal workspace: their own record of
	// approving a key the console didn't make.
	KeyRegistered = "app.key_registered"
	// AgentWebhookRegistered and AgentWebhookUnregistered: a conversation,
	// with the registration link its subscribing result gave it, registered
	// (or replaced: detail.replaced) the URL its harness wakes it on (Capy's
	// webhook watch; internal/agenthooks), or removed it (detail: session,
	// kind). Never the URL: it's the credential.
	AgentWebhookRegistered   = "agent_webhook.registered"
	AgentWebhookUnregistered = "agent_webhook.unregistered"
	// A Grok Bot's Automations webhook (kind "cursor-automations") is
	// registered by the user's save on the setup page, confirmed by the
	// bot's key (actor "user", detail via "setup_page", confirmed "bot_key",
	// replaced), or by a proof from another app's session joining the bot's
	// webhook (actor "agent", via "bot_key", joined true); removed with a
	// reason (detail.reason "unwatched", "grant_ended", "links_revoked",
	// "app_deleted"). Never the URL, the webhook's key, the bot
	// ID, a challenge or a token.
	//
	// BotKeyRegistered and BotKeyReplaced: a Grok Bot proved the signing key
	// it keeps in its folder (detail: session, fingerprint, the first 16
	// hex characters of the public key's SHA-256), or the user's setup
	// replaced it with a new one (detail: old, new fingerprints).
	BotKeyRegistered = "bot_key.registered"
	BotKeyReplaced   = "bot_key.replaced"
	// EventSubscribeChanged: the app's manifest changed who may subscribe
	// to one of its events (detail: event, from, to: "tools" or "any"), in
	// each workspace the app keeps. Closing it ("tools") ended the
	// workspace's subscriptions to it not made by the app's tools (detail
	// ended, each recorded too as event_subscription.policy_changed) and
	// kept its conversations' subscriptions from before Wakeshift recorded
	// how each was made (detail kept_without_via).
	EventSubscribeChanged = "event.subscribe_changed"
	// VoucherRedeemed: a native client redeemed a subscribe voucher one of
	// the app's tools handed out, making a webhook subscription (recorded
	// too as event_subscription.created): detail id, event, arguments,
	// tool, destination, workspace, and voucher "user" (bound to the user
	// it was handed to) or "anonymous". Never the token.
	VoucherRedeemed = "event_subscription.voucher_redeemed"
	// InstallElicit: the user answered the install offer an app's SDK showed
	// in their client's own dialog (appapi/clientrequests.go), as the app
	// reported it (actor app:<id>): detail harness, outcome ("setup": they'll
	// run the steps themselves, or "later": "Not now"), reported_by "app".
	// Nothing is installed or granted on it.
	InstallElicit = "install.elicit"
	// Otherwise, what happens to an app itself (registered, key rotated,
	// deleted) is in the app's own log (apps.Log), not a workspace's.
)

// Hub is the actor for what Wakeshift does itself (expiry, reuse detection).
const Hub = "hub"

// User, App and Client name actors.
func User(id string) string   { return "user:" + id }
func App(id string) string    { return "app:" + id }
func Client(id string) string { return "client:" + id }

// Entry is one action.
type Entry struct {
	Actor   string
	Action  string
	AgentID string
	AppID   string
	GrantID string
	Detail  any // marshaled to a JSON object; keep it small
}

// Record appends e in tx (the workspace's transaction).
func Record(ctx context.Context, tx pgx.Tx, e Entry) error {
	detail := []byte("{}")
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return err
		}
		detail = b
	}
	_, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, actor, action, agent_id, app_id, grant_id, detail)
		VALUES (current_tenant(), $1, $2, nullif($3, ''), nullif($4, ''), nullif($5, '')::uuid, $6)`,
		e.Actor, e.Action, e.AgentID, e.AppID, e.GrantID, detail)
	return err
}

// Item is a recorded entry.
type Item struct {
	ID         int64           `json:"id"`
	At         time.Time       `json:"at"`
	Actor      string          `json:"actor"`
	ActorEmail string          `json:"actor_email,omitempty"` // for a user
	Action     string          `json:"action"`
	AgentID    string          `json:"agent_id,omitempty"`
	AppID      string          `json:"app_id,omitempty"`
	GrantID    string          `json:"grant_id,omitempty"`
	Detail     json.RawMessage `json:"detail"`
	// Shown is, for grant.created, what the consent screen said (from the
	// grant).
	Shown json.RawMessage `json:"shown,omitempty"`
}

// MaxLimit is the most entries List returns at once.
const MaxLimit = 100

// The log is append-only, so entries written before the American
// spellings keep the British ones: "subscription.cancelled", and a grant
// revocation's "subscriptions_cancelled" count. List reports them as
// written now (SubscriptionCanceled, "subscriptions_canceled").
const (
	americanAction = `CASE a.action WHEN 'subscription.cancelled' THEN '` + SubscriptionCanceled + `' ELSE a.action END`
	americanDetail = `CASE WHEN a.detail ? 'subscriptions_cancelled'
		THEN (a.detail - 'subscriptions_cancelled') || jsonb_build_object('subscriptions_canceled', a.detail -> 'subscriptions_cancelled')
		ELSE a.detail END`
)

// List returns up to limit entries, newest first, older than before (an
// entry ID; 0 for the newest).
func List(ctx context.Context, tx pgx.Tx, before int64, limit int) ([]Item, error) {
	if limit <= 0 || limit > MaxLimit {
		limit = MaxLimit
	}
	rows, err := tx.Query(ctx, `SELECT a.id, a.at, a.actor, coalesce(u.email, ''), `+americanAction+`, coalesce(a.agent_id, ''), coalesce(a.app_id, ''),
			coalesce(a.grant_id::text, ''), `+americanDetail+`, CASE WHEN a.action = 'grant.created' THEN g.shown END
		FROM audit_log a LEFT JOIN grants g ON g.tenant_id = a.tenant_id AND g.id = a.grant_id
			LEFT JOIN users u ON a.actor = 'user:' || u.id::text
		WHERE $1 = 0 OR a.id < $1 ORDER BY a.id DESC LIMIT $2`, before, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) {
		var it Item
		var shown []byte
		err := r.Scan(&it.ID, &it.At, &it.Actor, &it.ActorEmail, &it.Action, &it.AgentID, &it.AppID, &it.GrantID, &it.Detail, &shown)
		if shown != nil {
			it.Shown = shown
		}
		return it, err
	})
	if out == nil {
		out = []Item{} // the console lists them: never null
	}
	return out, err
}
