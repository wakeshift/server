package appapi

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/framing"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
	"github.com/wakeshift/server/internal/subscriptions"
)

// The Codex connector. A Codex thread can't hold
// a stream link itself, so an app puts a beacon in its tool results' _meta;
// the connector's hook on the user's machine redeems the beacon's one-time
// ticket here for the thread's own stream link, and holds it.

// mintBeacon stores a one-time ticket for the session and signs a beacon
// carrying it, naming the session's conversation if it has one, else the
// tool call it answers (call, for harnesses that say which; "" if not):
// the hook acts only on a beacon for its own conversation or call
// (beacon.VerifyBound). It names the app and the tool whose result carries
// it (tool, as the app said; "" if it didn't): the hook acts on it only
// from that app's server and that tool's result. lateOK: the ticket may
// be redeemed late (lateRedeemable): kept only for a beacon bound to a call
// for a session that doesn't know its conversation (Claude Code's), or
// bound to the conversation the session knows (Codex's thread).
func (a *API) mintBeacon(ctx context.Context, tx pgx.Tx, act actor, s sessions.Session, call, tool string, lateOK, marked bool) (string, time.Time, error) {
	if a.Sign == nil {
		return "", time.Time{}, errors.New("appapi: no beacon signer")
	}
	secret := make([]byte, 32)
	rand.Read(secret)
	ticket := "wst_" + act.tenant + "_" + base64.RawURLEncoding.EncodeToString(secret)
	now := a.now()
	exp := now.Add(beacon.TTL)
	tool = beaconTool(tool)
	// Redeem by call (callredeem.go): a call the plugin claimed is bound to the
	// claiming key, so its hook can redeem the ticket by the call's ID.
	var b callBinding
	if a.RedeemByToolCall && call != "" {
		var err error
		if b, err = bindCall(ctx, tx, act, s, call, tool, marked, now); err != nil {
			return "", time.Time{}, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO connector_tickets (tenant_id, token_hash, session_id, app_id, grant_id, expires_at, tool, late_ok,
		call_hash, key_hash, claim_thread, binding)
		VALUES (current_tenant(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`, ticketHash(ticket), s.ID, act.app, act.grant.ID, exp, tool,
		lateOK && (call != "" && s.Conversation == "" || call == "" && s.Conversation != ""), b.callHash, b.keyHash, b.threadHash, b.outcome); err != nil {
		return "", exp, err
	}
	if b.outcome == bindingStored {
		if err := indexCall(ctx, tx, b.callHash, act.app, exp); err != nil {
			return "", exp, err
		}
	}
	if s.Conversation != "" {
		call = ""
	}
	token, err := a.Sign(ctx, beacon.Type, beacon.Claims{Issuer: a.Issuer, Audience: beacon.Audience, Thread: s.Conversation, Call: call, App: act.app,
		Tool: tool, Redeem: strings.TrimSuffix(a.Issuer, "/") + "/v1/connector/redeem", Ticket: ticket, IssuedAt: now.Unix(), Expires: exp.Unix()})
	return token, exp, err
}

// unwatch stops a session being woken: it no longer wants to be, and its
// stream links are revoked (an open stream closes with 4001, so a Monitor or
// connector on it stops).
func (a *API) unwatch(w http.ResponseWriter, r *http.Request) {
	act := actorFrom(r.Context())
	var handle string
	err := a.tx(r, func(ctx context.Context, tx pgx.Tx) error {
		s, err := ownSession(ctx, tx, act, r.PathValue("handle"))
		if err != nil {
			return err
		}
		handle = s.Handle
		n, err := stopListening(ctx, tx, act.agent, s.ID)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Actor: "app:" + act.app, Action: audit.SessionUnwatched, AgentID: act.agent, AppID: act.app,
			GrantID: act.grant.ID, Detail: map[string]any{"session": s.Handle, "links_revoked": n}})
	})
	a.respond(w, map[string]string{"session": handle}, err)
}

// connectorApp is what a redeem tells the hooks about the ticket's app.
type connectorApp struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	PreapproveTools []string `json:"preapprove_tools,omitempty"`
}

var (
	ticketRE         = regexp.MustCompile(`^wst_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})_[A-Za-z0-9_-]{43}$`)
	errTicketGone    = errors.New("ticket expired or used")
	errThreadMissing = errors.New("ticket for another thread")
	// errTaken: the conversation is connected to a session of another
	// workspace or agent (takenElsewhere, takenHere).
	errTaken = errors.New("conversation connected elsewhere")
)

// LateRedeemWithin is how long after its expiry a ticket can still be
// redeemed late (redeemConnector's late): the window in which a waiter
// connects a conversation from the result that offered the install
// (connector.installedSince looks back as far).
const LateRedeemWithin = time.Hour

// lateRedeemable: an expired ticket may still be redeemed, because the
// hook says it found the beacon in the conversation's own record, minted
// before the hooks were installed, so the install needs no call of the
// app's tool (it may have side effects) to connect. late is the hook's
// word and the ticket's own (late_ok, set at mint for a beacon that went
// where the model never sees it: bound to a tool call, for a session that
// didn't know its conversation, Claude Code's _meta, whose transcript the
// waiter reads; or bound to the conversation the harness named in the
// call's _meta, Codex's, whose rollout the hook reads; a text beacon an
// agent may have seen stays a minute's). Only within LateRedeemWithin of
// its expiry; only for the conversation it's bound to: naming a call (the
// hook checked it) for a session that has no conversation yet, or the
// session's own conversation (the hook checked the beacon names it); and
// only while no connector has ever connected the session (after that,
// results carry fresh beacons and the hooks keep a link). Still single
// use.
func (a *API) lateRedeemable(ctx context.Context, tx pgx.Tx, late bool, call, thread, session string, expires time.Time) (bool, error) {
	if !late || thread == "" || !a.now().Before(expires.Add(LateRedeemWithin)) {
		return false, nil
	}
	var ok bool
	err := tx.QueryRow(ctx, `SELECT connector_seen_at IS NULL AND COALESCE(conversation = $3, $2 <> '') FROM sessions WHERE id = $1`,
		session, call, thread).Scan(&ok)
	return ok, err
}

// redeemConnector exchanges a beacon's ticket for its session's stream link. It needs
// no other credential: the ticket is single-use, expires within a minute and
// is bound to its thread, which the connector names (its hook's session_id).
// A session that doesn't know its conversation yet (the harness tells MCP
// servers only the call, or nothing; beacon.Binding) learns it here: the
// hook that redeemed was handed the result in that conversation. The
// session gets its own link from then on (runtime stream), so its queued
// deliveries move to it.
//
// supersedes is the stream link the hook held for the same conversation
// until now, if it held one: the conversation's earlier Wakeshift session
// (an OpenClaw session whose Codex thread changed after a restart, so its
// calls started a new one). That session is ended (supersede), so it
// doesn't keep matching publishes and holding what it can't deliver.
//
// held is the same link from hooks that keep their own link per
// (conversation, app) and replace it with the redeemed one: it's the
// takeover check's, and the session it delivers to is superseded as
// supersedes' is (only if the same app made it, so another app's
// session is left alone).
//
// A redeem doesn't take a conversation over: while the link the hook holds
// for it is live (not revoked, its session still watching that
// conversation), a ticket for a session in another of the same app's
// workspaces (another user's), or of another agent in the same workspace,
// is refused (409 conversation_taken). A beacon names a conversation
// anyone can learn the ID of, and hooks that see only a result's text can
// be handed one somebody else had the app mint for their own session; this
// keeps that from moving a connected conversation to them. Re-linking
// within the workspace and agent (a restart, a new session, watching
// again) is unaffected, and so is a conversation whose link was revoked
// (unwatch). Two moves across workspaces are kept: out of a conversation's
// own workspace (no user: "~conversation:…") of the same app, where a
// conversation that didn't pass its session key lands in a new one
// (OpenClaw after a restart); and to another app, which the hooks govern
// (a beacon comes only from its own app's server: connector.Pins).
func (a *API) redeemConnector(w http.ResponseWriter, r *http.Request) {
	var in redeemRequest
	if !httpx.Decode(w, r, &in) {
		return
	}
	// A body without a ticket or thread is the caller's mistake, said as
	// such, and before the ticket is looked up: a redeem without its thread
	// doesn't use the ticket up.
	for _, f := range []struct{ name, value string }{{"ticket", in.Ticket}, {"thread", in.Thread}} {
		if strings.TrimSpace(f.value) == "" {
			c, _ := connectorversion.Parse(r.Header.Get(connectorversion.Header))
			logRedeem(http.StatusBadRequest, "bad_request", f.name+"_missing", "", "", c)
			httpx.WriteError(w, http.StatusBadRequest, "bad_request", `send {"ticket": "…", "thread": "…"}: `+f.name+" is missing")
			return
		}
	}
	m := ticketRE.FindStringSubmatch(in.Ticket)
	if m == nil || in.Thread == "" || len(in.Thread) > 200 {
		c, _ := connectorversion.Parse(r.Header.Get(connectorversion.Header))
		logRedeem(http.StatusNotFound, "unknown_ticket", "malformed", "", "", c)
		httpx.WriteError(w, http.StatusNotFound, "unknown_ticket", "that ticket expired or was already used")
		return
	}
	tenant := m[1]
	a.redeemTicket(w, r, in, tenant, ticketHash(in.Ticket), nil)
}

// redeemRequest is a ticket redeem's body (redeemConnector; redeemByCall
// makes one from its own, callRequest).
type redeemRequest struct {
	Ticket     string `json:"ticket"`
	Thread     string `json:"thread"`
	Call       string `json:"call"`       // the tool call a call-bound beacon named (its hook checked it)
	Supersedes string `json:"supersedes"` // the stream link the hook held for this conversation before
	Held       string `json:"held"`       // the same, from hooks that keep their own link per (conversation, app)
	// Wait is Wakeshift's wait command on the hook's machine
	// (`/Users/<name>/.wakeshift/bin/wakeshift wait`), for the
	// harnesses whose agents start one: kept for the session if it's
	// one (harnesses.WaitCommandOK), so their instructions can give it.
	Wait string `json:"wait"`
	// Late: the hook found the beacon in the conversation's own record,
	// minted before the hooks were installed (connector.installedSince),
	// so its ticket may have expired: accepted within LateRedeemWithin
	// for a session that may be redeemed late (lateRedeemable).
	Late bool `json:"late"`
}

// redeemTicket redeems the ticket whose hash is hash, in tenant
// (redeemConnector). byCall: a redeem by call ID (redeemByCall), hash the
// call's, whose checks run before the ticket is used up, so a refused one
// leaves it for the beacon. One path for both on purpose (#1191's reviews):
// everything a redeem does once its ticket is accepted (the takeover check,
// the fold and merge, the minimum version, the link) must be the same
// however the ticket was presented; byCall changes only which ticket is
// found, the checks before it's used up, and what's recorded.
func (a *API) redeemTicket(w http.ResponseWriter, r *http.Request, in redeemRequest, tenant string, hash []byte, byCall *callProof) {
	// The earlier session's link: only one Wakeshift signed counts.
	var oldTenant, oldLink string
	if in.Supersedes != "" && len(in.Supersedes) <= 4096 {
		oldTenant, oldLink, _ = a.Streams.Held(in.Supersedes)
	}
	// The link the hook holds for the conversation, for the takeover check.
	heldTenant, heldLink := oldTenant, oldLink
	if heldLink == "" && in.Held != "" && len(in.Held) <= 4096 {
		heldTenant, heldLink, _ = a.Streams.Held(in.Held)
		// Hooks that keep a link per (conversation, app) replace it with
		// this one: the session it delivers to is the conversation's
		// earlier one of the same app, superseded as the plugins' is
		// (supersede leaves another app's alone). Left watching, it kept
		// the conversation (this redeem was refused) and every publish
		// reached it twice: once through the hooks on the old link, once
		// through the new session's Monitor.
		oldTenant, oldLink = heldTenant, heldLink
	}
	var elsewhere linkedIn
	if heldLink != "" && heldTenant != tenant {
		var err error
		if elsewhere, err = a.linkedElsewhere(r.Context(), heldTenant, heldLink, in.Thread); err != nil {
			slog.Error("appapi: checking the conversation's current link", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
			return
		}
	}
	var app string
	var client connectorversion.Client
	var out redeemed
	var refused error  // committed: the ticket is used up all the same
	var notHeld bool   // refused: the session's hooks are connected, and this redeem didn't show their link
	var listening bool // refused (notHeld): the session's own command holds it, nothing has connected it (commandHolds)
	var late bool      // redeemed after its expiry (in.Late)
	// fold: the ticket's session is a sibling of the one the hook holds a
	// link for, in another of the app's conversation workspaces (below);
	// folded is what it subscribed to, and foldFrom the session.
	var fold bool
	var folded []foldedSub
	var foldFrom sessions.Session
	err := a.DB.InTenant(r.Context(), tenant, func(tx pgx.Tx) error {
		ctx := r.Context()
		var id, sessionID, grantID string
		var expires time.Time
		var redeemed, lateOK bool
		var tool string
		q := `SELECT id::text, session_id::text, app_id, grant_id::text, expires_at, redeemed_at IS NOT NULL, late_ok, tool, refused_attempts
			FROM connector_tickets WHERE token_hash = $1 FOR UPDATE`
		if byCall != nil {
			q = `SELECT id::text, session_id::text, app_id, grant_id::text, expires_at, redeemed_at IS NOT NULL, late_ok, tool, refused_attempts
				FROM connector_tickets WHERE call_hash = $1 AND binding = 'stored' ORDER BY created_at DESC LIMIT 1 FOR UPDATE`
		}
		err := tx.QueryRow(ctx, q, hash).Scan(&id, &sessionID, &app, &grantID, &expires, &redeemed, &lateOK, &tool, &out.RefusedAttempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return errTicketGone
		}
		if err != nil {
			return err
		}
		if redeemed {
			return errTicketGone
		}
		if byCall != nil {
			// Before the ticket is used up: a refused redeem by call (another
			// key, another conversation) leaves it for the beacon.
			if a.now().After(expires) {
				return errTicketGone // never late
			}
			err := checkCallProof(ctx, tx, *byCall, id, hash, sessionID, app, tool, in.Thread)
			if errors.Is(err, errCallKey) {
				// Someone holds the call's ID and a key that isn't the one
				// that claimed it: counted on the ticket (the conversation's
				// own redeem says so), audited in the workspace (committed,
				// with nothing else changed), and the ticket kept for the
				// beacon.
				refused = err
				// Counted up to 100, and audited once per ticket: anyone
				// with the call's ID can do this, so it mustn't grow the
				// workspace's audit log without bound.
				if _, err := tx.Exec(ctx, `UPDATE connector_tickets SET refused_attempts = least(refused_attempts + 1, 100) WHERE id = $1`, id); err != nil {
					return err
				}
				if out.RefusedAttempts > 0 {
					return nil
				}
				return audit.Record(ctx, tx, audit.Entry{Actor: "connector", Action: audit.ConnectorCallKeyMismatch, AppID: app,
					Detail: map[string]any{"session": sessionID, "outcome": "key_mismatch", "key": keyFingerprint(byCall.key)}})
			}
			if err != nil {
				return err
			}
		}
		if a.now().After(expires) {
			ok, err := a.lateRedeemable(ctx, tx, in.Late && lateOK, in.Call, in.Thread, sessionID, expires)
			if err != nil || !ok {
				return cmp.Or(err, errTicketGone)
			}
			late = true
		}
		// FOR SHARE, before anything else the redeem changes: a revocation
		// in progress (which locks the grant, then the links and
		// subscriptions made under it) is waited for, so it can't miss the
		// link minted below, and superseding an earlier session (which ends
		// links and subscriptions made under the same grant) can't deadlock
		// with it.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM grants WHERE id = $1 FOR SHARE`, grantID); err != nil {
			return err
		}
		// Used up either way: a ticket offered for the wrong thread isn't
		// tried again.
		via := "beacon"
		switch {
		case byCall != nil:
			via = "call"
		case late:
			via = "late"
		}
		if _, err := tx.Exec(ctx, `UPDATE connector_tickets SET redeemed_at = now(), redeemed_via = $2 WHERE id = $1`, id, via); err != nil {
			return err
		}
		s, err := sessions.Get(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		// The connector redeeming (its X-Wakeshift-Connector header, else
		// version 0 of the one its harness uses): recorded on the session
		// once the ticket proves to be for this conversation, whether or not
		// it's served (below).
		var updated bool // its earlier connector was refused as out of date
		client, updated, err = a.redeemingClient(ctx, tx, r, s)
		if err != nil {
			return err
		}
		bound := false
		// From here, a refusal undoes what this redeem changed (but not the
		// ticket being used up): an earlier session is ended only by a
		// redeem that succeeds.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		refuse := func(why error) error {
			refused = why
			return sp.Rollback(ctx)
		}
		// The session's hooks are connected (a link minted by a redeem
		// still works): only they redeem for it again, showing that link
		// (held: theirs is the latest, or one rotated from it), since a
		// beacon for the session doesn't by itself show that the redeemer
		// is its hooks (#1152). A hook that lost its link (reinstalled)
		// connects again once that link expires (SessionLinkTTL unused).
		// (A ticket for another conversation is refused below, as such.)
		// Before the hooks connect it, a Cursor conversation's command
		// holds it the same way (commandHolds).
		command, err := claimedBearer(ctx, tx, tenant, s)
		if err != nil {
			return err
		}
		switch {
		case s.Conversation == in.Thread:
			var callKey []byte
			if byCall != nil {
				callKey = byCall.key
			}
			proven, err := redeemProven(ctx, tx, s, in.Thread, heldTenant == tenant, heldLink, callKey, command)
			if err != nil {
				return err
			}
			if !proven.ok {
				notHeld, listening = true, proven.listening
				return refuse(errTaken)
			}
		case s.Conversation == "" && command:
			// No connector link of the session's works without a
			// conversation (a redeem binds it), so only the command counts.
			if err := streams.LockRedeems(ctx, tx, s.ID); err != nil {
				return err
			}
			if listening, err = backgroundLive(ctx, tx, s); err != nil {
				return err
			}
			if listening {
				notHeld = true
				return refuse(errTaken)
			}
		}
		// Not a takeover: the conversation isn't connected to a session of
		// another of this app's workspaces, or of another agent here.
		if elsewhere.live && elsewhere.app == app && !strings.HasPrefix(elsewhere.external, conversationPrefix) {
			return refuse(errTaken)
		}
		if heldLink != "" && heldTenant == tenant {
			taken, err := takenHere(ctx, tx, heldLink, in.Thread, s.Agent)
			if err != nil {
				return err
			}
			if taken {
				return refuse(errTaken)
			}
		}
		// The hook holds a live link for the conversation to a session of
		// the same agent in another of this app's conversation workspaces
		// (without a user): the conversation's first calls came in
		// parallel, before either result gave it a session_key, so each
		// started a conversation of its own, and the hook connected the
		// other first. It stays connected where it is, and what this
		// session subscribed to joins it there (foldInto, after this
		// transaction). Not the other way: the session the hook holds is
		// the conversation's own, while this one's key may be anyone's who
		// had the app mint a beacon naming the conversation, so nothing
		// of the held session's ever moves to this one's workspace.
		if fold = elsewhere.live && elsewhere.app == app && strings.HasPrefix(elsewhere.external, conversationPrefix) &&
			elsewhere.agent == s.Agent && !a.Streams.Minimums().Outdated(client); fold {
			if s.Conversation != "" && s.Conversation != in.Thread {
				return refuse(errThreadMissing)
			}
			g, err := grants.Get(ctx, tx, grantID)
			if err != nil {
				return err
			}
			if g.RevokedAt != nil || !a.now().Before(g.ExpiresAt) {
				return refuse(errGrantGone)
			}
			if !s.Watching {
				return refuse(errTicketGone)
			}
			if folded, err = foldable(ctx, tx, s.ID); err != nil {
				return err
			}
			foldFrom = s
			return sp.Commit(ctx)
		}
		// A first redeem: the session that has the conversation now, if
		// another does, takes its redeems' lock here, before anything below
		// takes the agent's sessions lock (supersede, the bind): redeem
		// locks before the agent's, the one order (lockAgentSessions). This
		// read only picks the lock: what's decided below reads the
		// conversation's session again, under the agent's lock (which every
		// bind takes), and goes ahead only if it's this one.
		holder := ""
		if s.Conversation == "" {
			if o, err := sessions.ByConversation(ctx, tx, s.Agent, in.Thread); err == nil && o.ID != s.ID {
				if err := streams.LockRedeems(ctx, tx, o.ID); err != nil {
					return err
				}
				holder = o.ID
			} else if err != nil && !errors.Is(err, sessions.ErrNotFound) {
				return err
			}
		}
		if oldTenant == tenant && oldLink != "" && s.Watching && (s.Conversation == "" || s.Conversation == in.Thread) {
			// The earlier session is in this workspace (the app's user's):
			// ended first, it gives the conversation up to this one.
			if err := supersede(ctx, tx, app, oldLink, s.ID, in.Thread); err != nil {
				return err
			}
		}
		switch {
		case s.Conversation == "":
			// First redeem: the conversation is the hook's (the redeem lock
			// of the session that has it, if one does, was taken above).
			// Binds of the agent's sessions here wait for each other (its
			// hooks may redeem two of them at once).
			if err := lockAgentSessions(ctx, tx, s.Agent); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `UPDATE sessions SET conversation = $2 WHERE id = $1 AND conversation IS NULL
				AND NOT EXISTS (SELECT 1 FROM sessions o WHERE o.agent_id = sessions.agent_id AND o.conversation = $2)`, s.ID, in.Thread)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 1 {
				bound = true
				break
			}
			// Another session of the agent's here has the conversation. If
			// it's a sibling (the app's, watching: the conversation's first
			// calls came in parallel and each started one, and its hook
			// redeemed the other's beacon first, without a link to hand
			// back), this one merges into it, and the hook gets its link:
			// the hook's beacon for this session was the conversation's,
			// and both are in this workspace. Anything else is refused.
			o, err := sessions.ByConversation(ctx, tx, s.Agent, in.Thread)
			if errors.Is(err, sessions.ErrNotFound) || err == nil && (o.ID == s.ID || !o.Watching || !o.Joined(app) || !s.Watching) {
				return refuse(errThreadMissing)
			}
			if err != nil {
				return err
			}
			// Merging connects o, so it needs what a redeem of o's own
			// ticket needs (#1152): while hooks hold a link to o, only they
			// connect it, showing that link (or the call's key). A beacon
			// for a sibling session, which anyone who had the app mint one
			// naming the conversation holds, doesn't show that. (Hooks that
			// send o's link have already ended o into this session:
			// supersede, above.)
			//
			// o is read under the agent's lock, so it's current. If it
			// isn't the session whose redeem lock was taken above (another
			// redeem bound it in between), nothing here proved anything
			// about it, and taking its lock now could deadlock: refused, as
			// not shown (its hooks' next redeem shows its link).
			if o.ID != holder {
				notHeld = true
				return refuse(errTaken)
			}
			var callKey []byte
			if byCall != nil {
				callKey = byCall.key
			}
			ocommand, err := claimedBearer(ctx, tx, tenant, o)
			if err != nil {
				return err
			}
			proven, err := redeemProven(ctx, tx, o, in.Thread, heldTenant == tenant, heldLink, callKey, ocommand)
			if err != nil {
				return err
			}
			if !proven.ok {
				notHeld, listening = true, proven.listening
				return refuse(errTaken)
			}
			if err := mergeInto(ctx, tx, app, s, o.ID); err != nil {
				return err
			}
			s = o
		case s.Conversation != in.Thread:
			return refuse(errThreadMissing)
		}
		if a.Streams.Minimums().Outdated(client) {
			// Older than the core serves: no link, nothing bound or ended
			// (its messages stay held for the session's tool results, which
			// ask for the update), but its version is recorded, and the
			// ticket is used up.
			if err := refuse(&streams.UpdateRequired{Client: client}); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE sessions SET connector = $2 WHERE id = $1`, s.ID, client.String())
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET connector = $2 WHERE id = $1`, s.ID, client.String()); err != nil {
			return err
		}
		g, err := grants.Get(ctx, tx, grantID)
		if err != nil {
			return err
		}
		if g.RevokedAt != nil || !a.now().Before(g.ExpiresAt) {
			return refuse(errGrantGone)
		}
		// Only while it's watching: a ticket handed out before an unwatch
		// doesn't start the connector again.
		var wait *string // the connector's wait command, else none (an older connector, or an odd path)
		if harnesses.WaitCommandOK(in.Wait) {
			wait = &in.Wait
		}
		tag, err := tx.Exec(ctx, `UPDATE sessions SET runtime = 'stream', connector_seen_at = now(), last_active_at = now(), wait_command = $2 WHERE id = $1 AND watching`, s.ID, wait)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return refuse(errTicketGone)
		}
		// The hooks take over delivery from whatever holds the session's
		// stream now (an older wait or stream, whose client may be gone):
		// what it hasn't confirmed goes to them (streams.TakeOver).
		if err := streams.TakeOver(ctx, tx, s.Agent, s.ID); err != nil {
			return err
		}
		if !hooksLinked(s) || updated {
			// Connected for the first time (or again, updated): the next tool result (or the
			// waiter's last line) says so, for harnesses whose hook notes
			// reach the model late (connectedNote).
			if _, err := tx.Exec(ctx, `UPDATE sessions SET connected_note_due = true WHERE id = $1`, s.ID); err != nil {
				return err
			}
		}
		// Deliveries queued for the agent's own stream (deliver_to, while
		// the session was a connector session) are the session's own now.
		if err := deliveries.ToOwnStream(ctx, tx, s.Agent, s.ID); err != nil {
			return err
		}
		u, exp, err := a.Streams.Mint(ctx, tx, tenant, streams.NewLink{Agent: s.Agent, Target: s.ID, Ack: true,
			CreatedBy: "app:" + app, GrantID: grantID, TTL: streams.SessionLinkTTL, Connector: client.String()})
		if err != nil {
			return err
		}
		out.URL, out.ExpiresAt, out.Session = u, exp.UTC().Format(time.RFC3339), s.Handle
		if out.SessionKey, err = sessionKeyOf(ctx, tx, tenant, s); err != nil {
			return err
		}
		detail := map[string]any{"session": s.Handle, "bound_conversation": bound, "connector": client.String()}
		if byCall != nil {
			// Redeem by call: the conversation is bound to the key that
			// connected it by call (another key's claims for it bind
			// nothing), with the rest of the redeem.
			if err := bindConversationKey(ctx, tx, s.Agent, in.Thread, byCall.key); err != nil {
				return err
			}
			detail["via"] = "call"
			detail["key"] = keyFingerprint(byCall.key)
		}
		if err := sp.Commit(ctx); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Actor: "connector", Action: audit.ConnectorLinked, AgentID: s.Agent, AppID: app, GrantID: grantID,
			Detail: detail})
	})
	if err == nil {
		err = refused
	}
	if err == nil && fold {
		// The held session gets what the ticket's subscribed to, and the
		// hook a link for it; then the ticket's session ends in its own
		// workspace. Failing that, it's left as it was (its subscriptions
		// the held session got stay, once each there).
		if err = a.foldInto(r.Context(), app, client, heldTenant, elsewhere.session, heldLink, in.Thread, folded, &out); err == nil {
			if serr := a.DB.InTenant(r.Context(), tenant, func(tx pgx.Tx) error {
				return mergeInto(r.Context(), tx, app, foldFrom, "")
			}); serr != nil {
				slog.Warn("appapi: ending a folded session", "err", serr)
			}
		}
	}
	if err == nil && !fold && oldLink != "" && oldTenant != tenant {
		// In another workspace (a conversation's own, without a user): its
		// own transaction, after this one. Failing leaves it as it was,
		// which the redeem doesn't depend on. Only a conversation's own
		// workspace of the same app: the link shown may be revoked or
		// expired (streams.Held), which the takeover check above doesn't
		// count, so a user's workspace (another user's, say: their old
		// link from a log) is never ended from here.
		if serr := a.DB.InTenant(r.Context(), oldTenant, func(tx pgx.Tx) error {
			if ok, err := conversationWorkspaceOf(r.Context(), tx, oldTenant, app); err != nil || !ok {
				return err
			}
			return supersede(r.Context(), tx, app, oldLink, "", in.Thread)
		}); serr != nil {
			slog.Warn("appapi: ending a superseded session", "err", serr)
		}
	}
	if err == nil {
		// Platform-level, outside the workspace's transaction. Without it
		// (an error reading it) the hooks just don't offer its tools.
		if ap, aerr := a.Apps.Get(r.Context(), app); aerr == nil {
			out.App = &connectorApp{ID: ap.ID, Name: framing.Name(ap.Name), PreapproveTools: ap.Manifest.PreapproveTools}
		} else {
			slog.Warn("appapi: reading the redeemed ticket's app", "app", app, "err", aerr)
		}
	}
	if client.Name == "" { // refused before the session was read: what the header says
		client, _ = connectorversion.Parse(r.Header.Get(connectorversion.Header))
	}
	var outdated *streams.UpdateRequired
	var callRefused *callRefusal
	switch {
	case err == nil:
		why := ""
		if late {
			why = "late"
		}
		if byCall != nil {
			why = "by_call"
		} else if fb := callFallback(r); fb != "" {
			// Redeem by call: the hook tried by call first and says why it fell back.
			why = strings.TrimSpace(why + " fallback=" + fb)
		}
		logRedeem(http.StatusOK, "ok", why, tenant, app, client)
		httpx.WriteJSON(w, http.StatusOK, out)
	case errors.As(err, &callRefused):
		// The same answer as a ticket that isn't there, so a refusal says
		// nothing about whether the call has a binding; the log says which.
		logRedeem(http.StatusNotFound, callRefused.code, "by_call", tenant, app, client)
		httpx.WriteError(w, http.StatusNotFound, "unknown_ticket", "no ticket for that call, or it expired or was used")
	case errors.As(err, &outdated):
		logRedeem(http.StatusUpgradeRequired, "update_required", "", tenant, app, client)
		httpx.WriteError(w, http.StatusUpgradeRequired, "update_required", outdated.Message(a.base()))
	case errors.Is(err, errTicketGone):
		logRedeem(http.StatusNotFound, "unknown_ticket", "", tenant, app, client)
		httpx.WriteError(w, http.StatusNotFound, "unknown_ticket", "that ticket expired or was already used")
	case errors.Is(err, errThreadMissing):
		logRedeem(http.StatusForbidden, "wrong_thread", "", tenant, app, client)
		httpx.WriteError(w, http.StatusForbidden, "wrong_thread", "that ticket is for another conversation")
	case errors.Is(err, errTaken):
		why := ""
		switch {
		case listening:
			// Answered as link_not_held: the redeemer learns no more.
			why = "command_listening"
		case notHeld:
			why = "link_not_held"
		}
		logRedeem(http.StatusConflict, "conversation_taken", why, tenant, app, client)
		msg := "this conversation is connected to another workspace's session; it stays connected there. " +
			"To connect it here, stop it being woken there first (the app's unwatch tool), or start a new conversation"
		if notHeld {
			// The same code, which hooks already know: the conversation
			// stays connected where it is. Its reason, for the hooks' note
			// (connector.ErrLinkNotHeld): it tells the redeemer nothing the
			// message doesn't, and only about the ticket's own session.
			msg = "this conversation's session is connected to hooks that hold its link, and this redeem didn't send it; it stays connected to them. " +
				"Hooks that lost their link connect again once it has gone unused for 25 minutes"
			httpx.WriteJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{
				"code": "conversation_taken", "message": msg, "reason": "link_not_held"}})
			return
		}
		httpx.WriteError(w, http.StatusConflict, "conversation_taken", msg)
	case errors.Is(err, errGrantGone):
		logRedeem(http.StatusForbidden, "no_grant", "", tenant, app, client)
		httpx.WriteError(w, http.StatusForbidden, "no_grant", "the agent's consent for this app was revoked or has expired")
	default:
		slog.Error("appapi: redeeming a connector ticket", "err", err)
		logRedeem(http.StatusInternalServerError, "internal", "", tenant, app, client)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
	}
}

// RedeemLog is the message of the one line each connector redeem logs
// (logRedeem).
const RedeemLog = "appapi: connector redeem"

// logRedeem logs a redeem's outcome: the error code it answered ("ok" if
// none) and why, where the code alone doesn't say; the workspace and app
// (empty if the ticket named none); and the connector, as its header named
// it. Never the ticket, the thread or a link.
func logRedeem(status int, outcome, why, tenant, app string, c connectorversion.Client) {
	level := slog.LevelInfo
	if status != http.StatusOK {
		level = slog.LevelWarn
	}
	connector := ""
	if c.Name != "" {
		connector = c.Name + "/" + c.Version
	}
	slog.Log(context.Background(), level, RedeemLog, "outcome", outcome, "status", status, "why", why,
		"tenant", tenant, "app", app, "harness", c.Harness, "connector", connector)
}

// linkedIn is what a link a hook holds says about its conversation in the
// link's own workspace.
// sessionKeyOf is the session_key a conversation's tool results give it
// for session s in workspace tenant (passSession's handle): without a user,
// wsc_<token> naming the conversation's own workspace, whose one session's
// handle (<agent>@conversation) doesn't carry it; with one, handleOf (for
// a harness's conversation found by its conversation ID, the handle its
// results don't ask it to pass, but which finds it all the same).
func sessionKeyOf(ctx context.Context, tx pgx.Tx, tenant string, s sessions.Session) (string, error) {
	var external string
	if err := tx.QueryRow(ctx, `SELECT coalesce(external_id, '') FROM tenants WHERE id = $1::uuid`, tenant).Scan(&external); err != nil {
		return "", err
	}
	if tok, ok := strings.CutPrefix(external, conversationPrefix); ok && convTokenRE.MatchString(tok) {
		return sessionTokenPrefix + tok, nil
	}
	return handleOf(s), nil
}

type linkedIn struct {
	live     bool   // the link isn't revoked, and its session still watches the conversation
	app      string // the workspace's app, if an app owns it
	external string // the app's ID for the workspace
	session  string // the link's session, if live
	agent    string // its agent
}

// linkedElsewhere reads, in the link's workspace (tenant), whether link
// still connects thread there, and whose workspace it is.
func (a *API) linkedElsewhere(ctx context.Context, tenant, link, thread string) (linkedIn, error) {
	var out linkedIn
	err := a.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT s.id::text, s.agent_id FROM stream_links l JOIN sessions s ON s.id::text = l.target
			WHERE l.id = $1::uuid AND l.revoked_at IS NULL AND s.watching AND s.conversation = $2`, link, thread).Scan(&out.session, &out.agent)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out.live = true
		return tx.QueryRow(ctx, `SELECT coalesce(app_id, ''), coalesce(external_id, '') FROM tenants WHERE id = $1::uuid`, tenant).Scan(&out.app, &out.external)
	})
	return out, err
}

// redeemProven: a redeem for session (thread's) may go ahead, which is
// the whole of the check that the session's hooks are the ones redeeming
// (#1152). Yes if no link a redeem minted for it (connector set) still
// works. Else only if the redeemer proves it's those hooks, one of two ways:
//   - held: it shows one of those links (the link's ID, held in the
//     transaction's workspace: here), one that still works (streams.LinkLive,
//     and unexpired), so a revoked or expired link, from an old log say, or
//     one whose grant or creator is gone, proves nothing;
//   - callKey: a redeem by call (callredeem.go) with the key thread is
//     already bound to, which only the plugin that connected it holds. A
//     key's first redeem by call proves nothing yet.
//
// It holds a lock on session until the transaction ends, so redeems for it
// take turns: two at once can't both find no link and both connect it; the
// second sees the first's link. A connector's stream ending with nothing left for the
// session takes the same lock to revoke the links connectors redeemed for
// it (streams.LockRedeems): the connector was told its link is revoked and
// forgets it, so it counts as held no longer, and its next redeem, with
// none, connects the session again.
//
// With command (claimedBearer), a session nothing has connected also isn't
// connected while its own command listens (commandHolds).
func redeemProven(ctx context.Context, tx pgx.Tx, s sessions.Session, thread string, here bool, held string, callKey []byte, command bool) (proof, error) {
	session := s.ID
	if err := streams.LockRedeems(ctx, tx, session); err != nil {
		return proof{}, err
	}
	connected, err := connectorLinked(ctx, tx, session)
	if err != nil {
		return proof{}, err
	}
	if !connected {
		if !command {
			return proof{ok: true}, nil
		}
		listening, err := backgroundLive(ctx, tx, s)
		return proof{ok: !listening, listening: listening}, err
	}
	if here && held != "" {
		// Live as the stream itself would take it: not revoked, its grant
		// not ended, its creator still a member.
		l, err := streams.LinkLive(ctx, tx, held)
		if err != nil && !errors.Is(err, streams.ErrLinkInvalid) {
			return proof{}, err
		}
		if err == nil && l.Target == session && l.Connector != "" && time.Now().Before(l.ExpiresAt) {
			return proof{ok: true}, nil
		}
	}
	var ok bool
	if callKey != nil {
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM connector_conversation_keys WHERE thread_hash = $1 AND key_hash = $2)`,
			threadHash(thread), keyHash(callKey)).Scan(&ok)
		return proof{ok: ok}, err
	}
	return proof{}, nil
}

// proof is redeemProven's answer: ok, the redeem may go ahead; listening,
// it may not because the session's command holds it (commandHolds).
type proof struct{ ok, listening bool }

// connectorLinked: a link a redeem minted for session (connector set)
// still works.
func connectorLinked(ctx context.Context, tx pgx.Tx, session string) (bool, error) {
	var connected bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_links WHERE target = $1 AND coalesce(connector, '') <> ''
		AND revoked_at IS NULL AND expires_at > now())`, session).Scan(&connected)
	return connected, err
}

// claimsLink: the harness's background command claims its link
// (linkclaim.go), so no result gives it.
func claimsLink(harness string) bool { return harness == "cursor" }

// claimedBearer: s is a session whose command claims its link, in a
// conversation's own workspace (tenant, no app user), where its
// session_key is the workspace's bearer and the agent passes it in its
// calls' arguments (#1152).
//
// commandHolds: for such a session, while nothing has connected it (no
// connector link works) and its command listens (backgroundLive), a redeem
// doesn't connect it and a result carries no beacon for it, so the stream
// stays the command's, as its claim keeps it (linkClaimFor). The
// conversation's own hooks connect from the first result after the command
// stops listening (a restart that loads them stops it).
func claimedBearer(ctx context.Context, tx pgx.Tx, tenant string, s sessions.Session) (bool, error) {
	if !claimsLink(s.Agent) {
		return false, nil
	}
	var external string
	if err := tx.QueryRow(ctx, `SELECT coalesce(external_id, '') FROM tenants WHERE id = $1::uuid`, tenant).Scan(&external); err != nil {
		return false, err
	}
	return strings.HasPrefix(external, conversationPrefix), nil
}

// commandHolds: see claimedBearer (anon: the workspace is a
// conversation's own).
func commandHolds(ctx context.Context, tx pgx.Tx, anon bool, s sessions.Session) (bool, error) {
	if !anon || !claimsLink(s.Agent) {
		return false, nil
	}
	if connected, err := connectorLinked(ctx, tx, s.ID); err != nil || connected {
		return false, err
	}
	return backgroundLive(ctx, tx, s)
}

// takenHere: link (in the transaction's workspace) still connects thread
// to a session of another agent than agent.
func takenHere(ctx context.Context, tx pgx.Tx, link, thread, agent string) (bool, error) {
	var taken bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stream_links l JOIN sessions s ON s.id::text = l.target
		WHERE l.id = $1::uuid AND l.revoked_at IS NULL AND s.watching AND s.conversation = $2 AND s.agent_id <> $3)`, link, thread, agent).Scan(&taken)
	return taken, err
}

// endSuperseded is why a topic subscription ended: its conversation's hook
// connected the conversation to a newer session (redeemConnector).
const endSuperseded = "superseded"

// supersede ends the session the stream link link (in the transaction's
// workspace) delivers to, if it's the conversation's (bound to thread) and
// app made it, and it isn't current (the session the hook connects now):
// with current, mergeInto; without (current is in another workspace: a
// conversation's own, without a user), its topic subscriptions end and it
// stops watching, its links revoked. Anything else is left alone.
func supersede(ctx context.Context, tx pgx.Tx, app, link, current, thread string) error {
	l, err := streams.LinkByID(ctx, tx, link)
	if errors.Is(err, streams.ErrNoLink) {
		return nil
	}
	if err != nil {
		return err
	}
	if l.CreatedBy != "app:"+app || l.Target == current {
		return nil
	}
	s, err := sessions.Get(ctx, tx, l.Target)
	if errors.Is(err, sessions.ErrNotFound) {
		return nil
	}
	if err != nil || s.Conversation != thread {
		return err
	}
	return mergeInto(ctx, tx, app, s, current)
}

// conversationWorkspaceOf: tenant is a conversation's own workspace (no
// user: "~conversation:…") of app.
func conversationWorkspaceOf(ctx context.Context, tx pgx.Tx, tenant, app string) (bool, error) {
	var owner, external string
	err := tx.QueryRow(ctx, `SELECT coalesce(app_id, ''), coalesce(external_id, '') FROM tenants WHERE id = $1::uuid`, tenant).Scan(&owner, &external)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && owner == app && strings.HasPrefix(external, conversationPrefix), err
}

// lockAgentSessions serializes, in the transaction's workspace, what binds
// or merges agent's sessions (as sessions.Register numbers them): two
// redeems of one conversation's sibling sessions at once (its hooks run
// in parallel) each see what the other did.
//
// Lock order: a transaction takes any session's redeem lock
// (streams.LockRedeems: redeemProven, the command check and a first
// redeem's holder, all in redeemTicket) before this one, never after: once
// it holds this lock it takes no redeem lock it doesn't already hold. A
// first redeem holds at most two redeem locks (its own session's, for a
// command, then the conversation's session's), and the conversation's
// session is never a first redeem's own, so those can't cycle either.
// streams takes a redeem lock without this one.
func lockAgentSessions(ctx context.Context, tx pgx.Tx, agent string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('sessions:' || current_tenant()::text || ':' || $1))`, agent)
	return err
}

// mergeInto gives up old (a session of the conversation's that its hook no
// longer delivers to) for current (the same agent's session, in the same
// workspace, the hook connects now; "" for one in another workspace). A
// conversation ends up with two when its first calls come in parallel,
// before either result gave it a session_key (each starts one), or when
// its calls start a new one (OpenClaw after a restart): each is the same
// conversation's, and what it subscribed to is its own. So old's live
// subscriptions that current doesn't have move to current, with what they
// were still owed; the ones current has too end (what they were owed goes
// to current's, handOn), as do all of them without current. old stops
// watching, its links are revoked (its stream closes), it gives up the
// conversation, and its session_key finds current from now on
// (handleSession follows merged_into).
func mergeInto(ctx context.Context, tx pgx.Tx, app string, old sessions.Session, current string) error {
	if err := lockAgentSessions(ctx, tx, old.Agent); err != nil {
		return err
	}
	// The session's row before its subscriptions', as a subscribe or
	// unsubscribe in it takes them (lockWatching, then the routing): the
	// other way round, each waits on the other.
	if err := lockWatching(ctx, tx, old.ID); err != nil {
		return err
	}
	list, err := liveTopicSubs(ctx, tx, `ts.session_id = $1`, old.ID)
	if err != nil {
		return err
	}
	if err := lockEnding(ctx, tx, topicSubIDs(list)); err != nil {
		return err
	}
	ending, moved := list, []string{}
	handed := 0
	if current != "" {
		if ending, moved, err = moveSubs(ctx, tx, list, current); err != nil {
			return err
		}
		// What was published to old's subscriptions that the pipeline
		// hasn't made deliveries of yet (it makes them after the publish
		// returns): made now, so handOn sees them (a moved one's go to
		// current, its session now). Ended without it, they were lost,
		// since current's subscription starts after them. Their cursors are
		// locked (lockEnding), so no pipeline step is mid-way on them. A
		// lapsed one (past its ttl) is ended, not caught up, as a publish's
		// copy treats it.
		for _, ts := range list {
			if lapsed(ts) {
				continue
			}
			if err := catchUp(ctx, tx, ts.ID); err != nil {
				return err
			}
		}
		if handed, err = handOn(ctx, tx, old.Agent, old.ID, current); err != nil {
			return err
		}
		// What old was owed on the moved ones went to current (handOn).
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'cancelled', finished_at = now(), lease_until = NULL, lease_holder = NULL
			WHERE target = $1 AND subscription_id = ANY($2::uuid[]) AND state = 'pending'`, old.ID, moved); err != nil {
			return err
		}
	}
	for _, ts := range ending {
		if err := endTopicSub(ctx, tx, app, ts, endSuperseded, false); err != nil {
			return err
		}
	}
	n, err := stopListening(ctx, tx, old.Agent, old.ID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET conversation = NULL, merged_into = nullif($2, '')::uuid WHERE id = $1`, old.ID, current); err != nil {
		return err
	}
	if current != "" {
		// Keys that found old find current; current is no one's merged
		// session any more (it may have been old's, the other way round).
		if _, err := tx.Exec(ctx, `UPDATE sessions SET merged_into = $2 WHERE merged_into = $1`, old.ID, current); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET merged_into = NULL WHERE id = $1`, current); err != nil {
			return err
		}
	}
	return audit.Record(ctx, tx, audit.Entry{Actor: "connector", Action: audit.SessionSuperseded, AgentID: old.Agent, AppID: app,
		Detail: map[string]any{"session": old.Handle, "subscriptions_ended": len(ending), "subscriptions_moved": len(moved),
			"links_revoked": n, "deliveries_handed_on": handed}})
}

// moveSubs moves the live topic subscriptions in list (old's, locked) that
// current has no equal of to current, with their event subscriptions:
// what's left is to end (lapsed, or current's already). An equal: current's
// live one on the same topic with the same filters, or its event
// subscription to the same event with the same arguments.
func moveSubs(ctx context.Context, tx pgx.Tx, list []liveTopicSub, current string) (ending []liveTopicSub, moved []string, err error) {
	heirs, err := liveTopicSubs(ctx, tx, `ts.session_id = $1`, current)
	if err != nil {
		return nil, nil, err
	}
	moved = []string{}
	for _, ts := range list {
		dup := lapsed(ts)
		for _, h := range heirs {
			dup = dup || !lapsed(h) && h.Topic == ts.Topic && maps.Equal(h.Filters, ts.Filters)
		}
		if !dup {
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM event_subscriptions o JOIN event_subscriptions n
				ON n.source_kind = o.source_kind AND n.source_id = o.source_id AND n.event = o.event AND n.arguments_key = o.arguments_key
				WHERE o.subscription_id = $1 AND o.ended_at IS NULL AND n.session_id = $2 AND n.ended_at IS NULL AND n.destination = o.destination)`,
				ts.ID, current).Scan(&dup); err != nil {
				return nil, nil, err
			}
		}
		if dup {
			ending = append(ending, ts)
			continue
		}
		for _, q := range []string{
			`UPDATE subscriptions SET session_id = $2 WHERE id = $1`,
			`UPDATE topic_subscriptions SET session_id = $2 WHERE subscription_id = $1 AND ended_at IS NULL`,
			`UPDATE event_subscriptions SET session_id = $2 WHERE subscription_id = $1 AND ended_at IS NULL`,
		} {
			if _, err := tx.Exec(ctx, q, ts.ID, current); err != nil {
				return nil, nil, err
			}
		}
		moved = append(moved, ts.ID)
	}
	return ending, moved, nil
}

// redeemed is a redeem's answer.
type redeemed struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
	Session   string `json:"session"`
	// SessionKey: the session_key the conversation passes back to the
	// app's tools, the one its tool results give it (sessionKeyOf), so
	// a plugin can pass it for the model (Hermes's, plugins/hermes):
	// the redeem is for this conversation only, and the result whose
	// beacon it redeemed gave the same key.
	SessionKey string `json:"session_key,omitempty"`
	// App: the app the ticket is for, its name and the tools its
	// manifest says can be pre-approved (apps.Manifest.PreapproveTools):
	// the hooks allow them only if the user agreed to exactly those
	// (internal/connector, toolrules.go).
	App *connectorApp `json:"app,omitempty"`
	// RefusedAttempts: redeems by call of this ticket with another key than
	// the one that claimed it (callredeem.go), refused before this one, so
	// the hook can tell the conversation someone else had its call's ID.
	RefusedAttempts int `json:"refused_attempts,omitempty"`
}

// foldedSub is a live event subscription of a conversation's session,
// as foldInto makes it again for the session the conversation's hook
// holds.
type foldedSub struct {
	Event, Arguments, Key, Via, Reason, Topic string
	Filters                                   map[string]string
	Once                                      bool
	TTLSeconds                                int
	CreatedAt                                 time.Time
	ExpiresAt                                 time.Time
}

// foldable is the session's live event subscriptions to its conversation,
// oldest first.
func foldable(ctx context.Context, tx pgx.Tx, session string) ([]foldedSub, error) {
	rows, err := tx.Query(ctx, `SELECT es.event, es.arguments, es.arguments_key, coalesce(es.via, ''), coalesce(es.reason, ''), ts.topic, su.filters,
		ts.once, ts.ttl_seconds, ts.created_at, ts.expires_at
		FROM event_subscriptions es
		JOIN topic_subscriptions ts ON ts.tenant_id = es.tenant_id AND ts.subscription_id = es.subscription_id
		JOIN subscriptions su ON su.tenant_id = ts.tenant_id AND su.id = ts.subscription_id
		WHERE es.session_id = $1 AND es.destination = 'conversation' AND es.ended_at IS NULL
		AND ts.ended_at IS NULL AND ts.expires_at > now() AND su.status = 'active'
		ORDER BY ts.created_at`, session)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[foldedSub])
}

// foldInto makes list (a sibling session's subscriptions, in another of the
// app's conversation workspaces) again for session (the one the
// conversation's hook holds a link for, in workspace tenant), each one it
// doesn't have already, as the conversation's own subscribe would, and
// replays what was published to it since the sibling subscribed (the
// latest that passes its filter). out is a redeem's answer for session: a
// new link for the hook, which replaces the one it handed back (held,
// revoked here, as a redeem's supersede revokes it), and its session_key.
func (a *API) foldInto(ctx context.Context, app string, client connectorversion.Client, tenant, session, held, thread string, list []foldedSub, out *redeemed) error {
	ap, err := a.Apps.Get(ctx, app)
	if err != nil {
		return err
	}
	slices.SortStableFunc(list, func(x, y foldedSub) int { return strings.Compare(x.Topic, y.Topic) })
	return a.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		o, err := sessions.Get(ctx, tx, session)
		if errors.Is(err, sessions.ErrNotFound) {
			return errTicketGone
		}
		if err != nil {
			return err
		}
		// Locks in a subscribe's order (eventSubscribe): the policy, the
		// topics (in order), the grant, the session.
		for _, f := range list {
			if err := holdPolicy(ctx, tx, app, f.Event, f.Via); err != nil {
				return err
			}
		}
		locked := map[string]time.Time{}
		var topics []string
		for _, f := range list {
			if _, ok := locked[f.Topic]; ok {
				continue
			}
			if locked[f.Topic], err = lockTopic(ctx, tx, app, f.Topic); err != nil {
				return err
			}
			topics = append(topics, f.Topic)
		}
		for _, topic := range topics { // personal topics' (none without a user), after every topic's, as a subscribe takes its one
			if err := lockStripe(ctx, tx, app, topic); err != nil {
				return err
			}
		}
		g, err := grants.Live(ctx, tx, app, o.Agent)
		if errors.Is(err, grants.ErrNotFound) {
			return errGrantGone
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM grants WHERE id = $1 FOR SHARE`, g.ID); err != nil {
			return err
		}
		if err := lockWatching(ctx, tx, o.ID); err != nil {
			return err
		}
		if o, err = sessions.Get(ctx, tx, o.ID); err != nil {
			return err
		}
		if !o.Watching || o.Conversation != thread {
			return errTicketGone // it stopped watching (or gave the conversation up) meanwhile
		}
		cv := &conversation{app: ap, session: o, act: actor{app: app, tenant: tenant, agent: o.Agent, grant: g}}
		for _, f := range list {
			sub := apps.Subscription{Event: f.Event, Arguments: []byte(f.Arguments), Key: f.Key, Topic: f.Topic}
			if err := serialize(ctx, tx, "event:"+o.ID+":"+f.Event+":"+f.Key); err != nil {
				return err
			}
			_, _, err := liveConversationSub(ctx, tx, app, sub, o.ID)
			if err == nil {
				continue // it has it
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			subID, err := insertTopicSub(ctx, tx, cv, f.Topic, f.Filters, f.Once, time.Duration(f.TTLSeconds)*time.Second)
			if err != nil {
				return err
			}
			// Its ttl runs on from where it was, not afresh.
			if _, err := tx.Exec(ctx, `UPDATE topic_subscriptions SET expires_at = $2 WHERE subscription_id = $1`, subID, f.ExpiresAt); err != nil {
				return err
			}
			id, err := recordEventSub(ctx, tx, ap, sub, cv, subID, toConversation, f.Via, "", "")
			if err != nil {
				return err
			}
			if f.Reason != "" {
				if _, err := tx.Exec(ctx, `UPDATE event_subscriptions SET reason = $2 WHERE id = $1::uuid`, id, f.Reason); err != nil {
					return err
				}
			}
			if window := min(time.Since(f.CreatedAt), maxReplay); window > 0 {
				if err := a.replay(ctx, tx, ap, f.Topic, subID, f.Filters, locked[f.Topic], window); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET connector = $2, connector_seen_at = now(), last_active_at = now() WHERE id = $1`, o.ID, client.String()); err != nil {
			return err
		}
		// The hook replaces the link it handed back with this one: that one
		// ends, and what its listener hasn't confirmed goes to the new one.
		if _, err := tx.Exec(ctx, `UPDATE stream_links SET revoked_at = now() WHERE id = $1::uuid AND target = $2 AND revoked_at IS NULL`, held, o.ID); err != nil {
			return err
		}
		if err := streams.TakeOver(ctx, tx, o.Agent, o.ID); err != nil {
			return err
		}
		u, exp, err := a.Streams.Mint(ctx, tx, tenant, streams.NewLink{Agent: o.Agent, Target: o.ID, Ack: true,
			CreatedBy: "app:" + app, GrantID: g.ID, TTL: streams.SessionLinkTTL, Connector: client.String()})
		if err != nil {
			return err
		}
		out.URL, out.ExpiresAt, out.Session = u, exp.UTC().Format(time.RFC3339), o.Handle
		if out.SessionKey, err = sessionKeyOf(ctx, tx, tenant, o); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{Actor: "connector", Action: audit.ConnectorLinked, AgentID: o.Agent, AppID: app, GrantID: g.ID,
			Detail: map[string]any{"session": o.Handle, "connector": client.String(), "subscriptions_folded": len(list)}})
	})
}

// handOn gives the superseding session (current, the same agent's) what
// the superseded one (old) was still owed and hadn't been handed: a
// publish made before current subscribed reached only old, and ending
// old's subscriptions would drop it. Each goes to current's subscription
// on the same topic, if its filters take the event (from before it
// subscribed, and an app's send to old's subscription by its ID, since
// it's the same conversation). One current already has isn't added again
// (deliveries.Insert), and the attempts so far go with it, so one handed
// out before comes as a redelivery. What's in flight (a listener holds it
// and may have shown it) is left to end with old's subscriptions.
func handOn(ctx context.Context, tx pgx.Tx, agent, old, current string) (int, error) {
	var to string
	err := tx.QueryRow(ctx, `SELECT agent_id FROM sessions WHERE id = $1`, current).Scan(&to)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && to != agent {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT d.event_id::text, d.subscription_id::text, d.attempts, ts.topic FROM deliveries d
		JOIN topic_subscriptions ts ON ts.tenant_id = d.tenant_id AND ts.subscription_id = d.subscription_id
		WHERE d.agent_id = $1 AND d.target = $2 AND d.state = 'pending' AND d.deliver_to IS NULL ORDER BY d.event_seq`, agent, old)
	if err != nil {
		return 0, err
	}
	type owed struct {
		Event, Sub string
		Attempts   int
		Topic      string
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[owed])
	if err != nil || len(list) == 0 {
		return 0, err
	}
	heirs, err := liveTopicSubs(ctx, tx, `ts.session_id = $1`, current)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, d := range list {
		e, err := events.Get(ctx, tx, d.Event)
		if err != nil {
			return moved, err
		}
		for _, h := range heirs {
			if h.Topic != d.Topic || lapsed(h) {
				continue
			}
			sub, err := subscriptions.Get(ctx, tx, h.ID)
			if err != nil {
				return moved, err
			}
			as := sub
			as.StartSeq = 0
			if e.Attrs[subscriptions.ForAttr] == d.Sub {
				as.ID = d.Sub
			}
			if !as.Matches(e) {
				continue
			}
			ok, err := deliveries.Insert(ctx, tx, deliveries.New{AgentID: agent, Target: current, SubscriptionID: sub.ID, Event: e})
			if err != nil {
				return moved, err
			}
			if ok && d.Attempts > 0 {
				if _, err := tx.Exec(ctx, `UPDATE deliveries SET attempts = $2 WHERE id = $1`, deliveries.ID(agent, e.ID, current), d.Attempts); err != nil {
					return moved, err
				}
			}
			if ok {
				moved++
			}
			break
		}
	}
	if moved > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, current_tenant()::text || ':' || $2)`, deliveries.NotifyChannel, agent); err != nil {
			return moved, err
		}
	}
	return moved, nil
}

// redeemingClient is the connector redeeming a ticket for the session
// (r's connectorversion.Header, else version 0 of the connector its
// harness uses). updated: the session's latest before it was out of date
// (and this one may be the update).
func (a *API) redeemingClient(ctx context.Context, tx pgx.Tx, r *http.Request, s sessions.Session) (c connectorversion.Client, updated bool, err error) {
	var harness string
	if err := tx.QueryRow(ctx, `SELECT coalesce(harness, agent_id) FROM sessions WHERE id = $1`, s.ID).Scan(&harness); err != nil {
		return c, false, err
	}
	_, updated = a.outdatedConnector(s)
	return connectorversion.Identify(r.Header.Get(connectorversion.Header), harness), updated, nil
}

func ticketHash(t string) []byte {
	sum := sha256.Sum256([]byte(t))
	return sum[:]
}

// StrandedAfter is how long a delivery for a watching Codex thread may wait
// with no connector holding the thread's stream before StrandedCodex counts
// the thread.
const StrandedAfter = 2 * time.Minute

// StrandedLog is what the expiry pass logs, with the workspace and count,
// for each workspace with stranded Codex threads (the log metric
// wakeshift_codex_stranded matches it).
const StrandedLog = "codex: watching threads with deliveries waiting and no connector"

// StrandedCodex counts the transaction's workspace's Codex threads that are
// stranded: watching, connected through Wakeshift's connector before, with a
// delivery that could have gone out more than after ago still waiting, and
// no connector holding the thread's stream (none renewed in the last
// minute). Its connector stopped and nothing on the user's machine started
// it again: the core can't start a process there, and the connector's hook
// starts it only at the thread's next tool call, prompt or opening. A thread whose window really closed is
// counted too, so this is a measure, not an alarm: the expiry pass logs it
// for a metric.
func StrandedCodex(ctx context.Context, tx pgx.Tx, after time.Duration) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, strandedCodexSQL, after.String()).Scan(&n)
	return n, err
}

// strandedCodexSQL counts StrandedCodex's threads. Each session's ID is
// cast to text once, in s (OFFSET 0 keeps the planner from folding the
// cast back in), so its deliveries and leases are read by target: under
// row-level security, Postgres 17.12 and later won't use an I/O cast of an
// outer column (not leakproof) as an index or join condition
// (TestStrandedCodexReadsByIndex).
var strandedCodexSQL = `SELECT count(*) FROM (SELECT tenant_id, agent_id, id::text AS target FROM sessions
			WHERE runtime = 'stream' AND conversation IS NOT NULL AND watching AND connector_seen_at IS NOT NULL
			AND connector LIKE '% codex' OFFSET 0) s
		WHERE EXISTS (SELECT 1 FROM deliveries d WHERE d.tenant_id = s.tenant_id AND d.agent_id = s.agent_id
			AND d.target = s.target AND d.state = 'pending' AND d.available_at < now() - $1::interval)
		AND NOT EXISTS (SELECT 1 FROM stream_leases l WHERE l.tenant_id = s.tenant_id AND l.agent_id = s.agent_id
			AND l.target = s.target AND l.renewed_at > now() - interval '1 minute' AND l.holder NOT LIKE 'poll\_%')`
