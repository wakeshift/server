package appapi

// Redeem by tool call ID, for Claude Code's plugin, behind
// API.RedeemByToolCall (WAKESHIFT_REDEEM_BY_TOOL_CALL, off by default).
//
// Claude Code tells an MCP server each call's ID (_meta
// ["claudecode/toolUseId"]). Its synchronous PreToolUse hook gets the same
// ID before the request goes out, and its PostToolUse hook gets it the
// moment the result is in, but not the result's _meta (the beacon), which
// it reads from the transcript once Claude Code writes it there. So:
//
//  1. Claim. For a call whose harness argument carries the plugin's mark,
//     the plugin's PreToolUse hook tells the plugin's issuer, before the
//     app's server sees the ID, that this conversation's key claims it:
//     POST /v1/connector/claim {"call", "thread", "app", "key", "sig",
//     "ts"}, signed by a key per Claude Code session that only the plugin
//     holds (the model carries nothing). Kept at platform level
//     (connector_call_claims), since no workspace is known yet. The first
//     claim keeps the call: a later one with another key (someone who saw
//     the ID on its way to the app) is ignored and counted.
//  2. Bind. When the call's result mints a beacon (mintBeacon), a claim
//     for the call binds the ticket to the claiming key (bindCall), unless
//     the claim is for another app, the conversation is another one than
//     the session's own (a fork), the conversation is bound to another key
//     (a resume without the key file, a teleport), or the ticket names no
//     tool (an old SDK). The outcome is kept on the ticket (binding).
//  3. Redeem. PostToolUse redeems the bound ticket by the call's ID,
//     signed by the same key: POST /v1/connector/redeem-call. Every check
//     runs before the ticket is used up, and a refusal answers as a missing
//     ticket would: the hook falls back to the beacon, which stays on every
//     result and is never used up by anything here. The first redeem by
//     call binds the conversation to its key (connector_conversation_keys).
//
// A key that isn't the claiming one is refused, counted on the ticket (the
// conversation's own redeem then says so: refused_attempts), and audited in
// the workspace. Proofs allow five minutes of clock skew either way. Every
// answer of redeem-call but a success takes at least refusalFloor, so its
// timing doesn't tell a prober which check refused it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dblimit"
	"github.com/wakeshift/server/internal/hookproof"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/sessions"
)

// ClaimTTL is how long a claim waits for its call's result to mint a
// beacon (the tool's own run time).
const ClaimTTL = 10 * time.Minute

// CallFallbackHeader is the header a hook's beacon redeem names, after a
// redeem by call it couldn't make, why (logRedeem).
const CallFallbackHeader = "X-Wakeshift-Call-Redeem"

// The binding outcomes (connector_tickets.binding).
const (
	bindingStored  = "stored"
	bindingNoClaim = "no_claim"
	bindingApp     = "app_mismatch"
	bindingNoTool  = "no_tool"
	bindingThread  = "thread_mismatch" // a fork: the session is another conversation's
	bindingKey     = "key_changed"     // the conversation is bound to another key
	bindingContest = "contested"       // two keys claimed the call
)

func sha(prefix, s string) []byte {
	h := sha256.Sum256([]byte(prefix + "\x00" + s))
	return h[:]
}

// callIDHash, threadHash: how a call's ID and a conversation ID are kept.
func callIDHash(call string) []byte    { return sha("wakeshift-call", call) }
func threadHash(thread string) []byte  { return sha("wakeshift-thread", thread) }
func keyHash(key []byte) []byte        { h := sha256.Sum256(key); return h[:] }
func keyFingerprint(key []byte) string { return hex.EncodeToString(keyHash(key)[:8]) }
func callFallback(r *http.Request) string {
	v := r.Header.Get(CallFallbackHeader)
	if len(v) > 40 {
		return ""
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c == '_') {
			return ""
		}
	}
	return v
}

// logCall is one counted event (log-based counters): claims,
// bindings at issue (with "marked" for a marked call that had no claim:
// a claim miss) and refused redeems by call.
func logCall(what, outcome, app string, extra ...any) {
	slog.Info("appapi: call "+what, append([]any{"outcome", outcome, "app", app}, extra...)...)
}

// signedBy is what every request signed by a session's key carries: the
// conversation (the hook's session_id), the key (base64url Ed25519) and
// its signature (hookproof.Message at TS, Unix seconds).
type signedBy struct {
	Thread string `json:"thread"`
	Key    string `json:"key"`
	Sig    string `json:"sig"`
	TS     int64  `json:"ts"`
}

func (s signedBy) proof() hookproof.Proof { return hookproof.Proof{Key: s.Key, Sig: s.Sig, TS: s.TS} }

// registerRequest is a registration's body.
type registerRequest struct {
	signedBy
}

// claimRequest is a claim's body: the call, and the app the hook pinned
// the server to ("" on the server's first sight).
type claimRequest struct {
	signedBy
	Call string `json:"call"`
	App  string `json:"app,omitempty"`
}

func (in claimRequest) ok() bool {
	return in.Call != "" && len(in.Call) <= 200 && in.Thread != "" && len(in.Thread) <= 200 && len(in.App) <= 200
}

// callRedeemRequest is a redeem by call's body: a claim's, the MCP tool the
// result came from, and, as a ticket redeem's, the link the hook holds and
// its wait command.
type callRedeemRequest struct {
	claimRequest
	Tool string `json:"tool,omitempty"`
	Held string `json:"held,omitempty"`
	Wait string `json:"wait,omitempty"`
}

// Redeem by call's limits per client address: each registration and claim
// stores a row for any key, so they're bounded. Far above a person's use (a
// session registers at each start and turn's end; a claim is a marked call).
var (
	registerLimit = dblimit.Limit{Name: "connector-register", Max: 600, Window: time.Hour}
	claimLimit    = dblimit.Limit{Name: "connector-claim", Max: 3000, Window: time.Hour}
)

// callLimited takes one of l for the request's client address, answering
// 429 if it's used up (the hook then falls back to the beacon).
func (a *API) callLimited(w http.ResponseWriter, r *http.Request, l dblimit.Limit) bool {
	if a.CallLimits == nil {
		return false
	}
	ok, retry, err := a.CallLimits.Take(r.Context(), a.DB.Pool, l, agentkit.ClientAddr(r, a.CallProxyHops), a.now())
	if err != nil {
		slog.Error("appapi: counting a redeem by call limit", "err", err)
		return false // fail open: the beacon is the bound anyway
	}
	if !ok {
		logCall(l.Name, "limited", "")
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		httpx.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; try again later")
		return true
	}
	return false
}

// registeredFor is how long a session's registration lasts after its last
// refresh (each start of its waiter refreshes it): the conversation can be
// resumed that long.
const registeredFor = 30 * 24 * time.Hour

// registerCallSession records a session's key for its conversation (step 0):
// POST /v1/connector/register {thread, key, sig, ts}, from the session's
// waiter when the session starts, in the background, so a claim later has
// a key the core already knows to check. The first registration keeps the
// conversation until it lapses; another key's is refused (registered:
// false). Not needed for a claim (a session whose waiter isn't up claims
// unregistered), but once there, claims for the conversation need its key.
// A lapsed registration (30 days without its session) can be replaced by
// any key: registering needs the conversation's ID, which only the user's
// computer has (Claude Code tells no server), and a conversation connected
// by call stays bound to its key apart from this (connector_conversation_keys).
func (a *API) registerCallSession(w http.ResponseWriter, r *http.Request) {
	if !a.RedeemByToolCall {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if a.callLimited(w, r, registerLimit) {
		return
	}
	var in registerRequest
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Thread == "" || len(in.Thread) > 200 {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", `send {"thread", "key", "sig", "ts"}`)
		return
	}
	key, ok := hookproof.Verify(in.proof(), hookproof.Register, a.Issuer, "", in.Thread, a.now())
	if !ok {
		logCall("register", "bad_proof", "")
		badProof(w, a.now())
		return
	}
	tag, err := a.DB.Pool.Exec(r.Context(), `INSERT INTO connector_sessions (thread_hash, key_hash, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (thread_hash) DO UPDATE SET key_hash = EXCLUDED.key_hash, expires_at = EXCLUDED.expires_at,
			created_at = CASE WHEN connector_sessions.key_hash = EXCLUDED.key_hash THEN connector_sessions.created_at ELSE now() END
		WHERE connector_sessions.key_hash = EXCLUDED.key_hash OR connector_sessions.expires_at <= now()`,
		threadHash(in.Thread), keyHash(key), a.now().Add(registeredFor))
	if err != nil {
		slog.Error("appapi: registering a session", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return
	}
	outcome := "stored"
	if tag.RowsAffected() == 0 {
		outcome = "conflict"
	}
	logCall("register", outcome, "")
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"registered": outcome == "stored"})
}

// registeredKey is the key registered for thread, nil if none (or lapsed).
func (a *API) registeredKey(ctx context.Context, thread string) ([]byte, error) {
	var kh []byte
	err := a.DB.Pool.QueryRow(ctx, `SELECT key_hash FROM connector_sessions WHERE thread_hash = $1 AND expires_at > $2`, threadHash(thread), a.now()).Scan(&kh)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return kh, err
}

// claimCall stores a claim (step 1).
//
// Any key can claim a call: the plugin has no credential the core could
// check, and needs none, because of when it claims. The plugin's
// PreToolUse hook claims before Claude Code sends the request, so the call
// ID exists only on the user's machine (and with the model's provider)
// until then; the app's server, a gateway or a log sees it only after the
// claim is stored, and the first claim keeps the call. Someone who could
// claim first must read the call ID on the user's machine before the
// request leaves it, and could read the session's key file there too. A
// claim the plugin didn't make in time (a timeout) leaves the call to
// whoever claims it, which can bind the ticket to their key: the same
// exposure as today's beacon, which anyone who sees the result can redeem.
// A claim binds nothing at all for a conversation already bound to another
// key (bindCall), and the beacon stays on every result.
func (a *API) claimCall(w http.ResponseWriter, r *http.Request) {
	if !a.RedeemByToolCall {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if a.callLimited(w, r, claimLimit) {
		return
	}
	var in claimRequest
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !in.ok() {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", `send {"call", "thread", "key", "sig", "ts"}`)
		return
	}
	key, ok := hookproof.Verify(in.proof(), hookproof.Claim, a.Issuer, in.Call, in.Thread, a.now())
	if !ok {
		logCall("claim", "bad_proof", in.App)
		badProof(w, a.now())
		return
	}
	// A conversation whose session registered a key takes claims only by
	// that key.
	if reg, err := a.registeredKey(r.Context(), in.Thread); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return
	} else if reg != nil && string(reg) != string(keyHash(key)) {
		logCall("claim", "unregistered_key", in.App)
		httpx.WriteJSON(w, http.StatusOK, map[string]bool{"claimed": false})
		return
	}
	if in.App != "" {
		// A claim names only an app there is (its hook's pin): one that
		// isn't is refused, rather than stored as naming none.
		var known bool
		if err := a.DB.Pool.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM apps WHERE id = $1)`, in.App).Scan(&known); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
			return
		}
		if !known {
			logCall("claim", "unknown_app", "")
			httpx.WriteJSON(w, http.StatusOK, map[string]bool{"claimed": false})
			return
		}
	}
	var outcome string
	err := pgx.BeginFunc(r.Context(), a.DB.Pool, func(tx pgx.Tx) error {
		var kh []byte
		var expires time.Time
		err := tx.QueryRow(r.Context(), `SELECT key_hash, expires_at FROM connector_call_claims WHERE call_hash = $1 FOR UPDATE`, callIDHash(in.Call)).Scan(&kh, &expires)
		switch {
		case errors.Is(err, pgx.ErrNoRows) || err == nil && !expires.After(a.now()):
			// Only over an expired claim: a claim another request stored
			// meanwhile keeps the call (first claim wins).
			tag, err := tx.Exec(r.Context(), `INSERT INTO connector_call_claims (call_hash, key_hash, thread_hash, app_id, expires_at)
				VALUES ($1, $2, $3, (SELECT id FROM apps WHERE id = $4), $5)
				ON CONFLICT (call_hash) DO UPDATE SET key_hash = EXCLUDED.key_hash, thread_hash = EXCLUDED.thread_hash, app_id = EXCLUDED.app_id,
				contested = false, created_at = now(), expires_at = EXCLUDED.expires_at WHERE connector_call_claims.expires_at <= now()`,
				callIDHash(in.Call), keyHash(key), threadHash(in.Thread), in.App, a.now().Add(ClaimTTL))
			if err != nil {
				return err
			}
			outcome = "stored"
			if tag.RowsAffected() == 0 {
				if err := tx.QueryRow(r.Context(), `SELECT key_hash FROM connector_call_claims WHERE call_hash = $1`, callIDHash(in.Call)).Scan(&kh); err != nil {
					return err
				}
				outcome = "conflict"
				if string(kh) == string(keyHash(key)) {
					outcome = "repeat"
				} else if _, err := tx.Exec(r.Context(), `UPDATE connector_call_claims SET contested = true WHERE call_hash = $1`, callIDHash(in.Call)); err != nil {
					return err
				}
			}
			return nil
		case err != nil:
			return err
		case string(kh) == string(keyHash(key)):
			outcome = "repeat"
			return nil
		default:
			// Another key claims a claimed call: contested, and it binds
			// nothing, so neither key gets the ticket by call (both fall
			// back to the beacon). The plugin's claim is async and races
			// the call, so the other key may be someone who saw the ID on
			// its way to the app and got there first.
			outcome = "conflict"
			_, err = tx.Exec(r.Context(), `UPDATE connector_call_claims SET contested = true WHERE call_hash = $1`, callIDHash(in.Call))
			return err
		}
	})
	if err != nil {
		slog.Error("appapi: storing a call claim", "err", err)
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return
	}
	logCall("claim", outcome, in.App)
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"claimed": outcome != "conflict"})
}

// badProof answers 401 with the core's time, so a hook can log its skew.
func badProof(w http.ResponseWriter, now time.Time) {
	w.Header().Set("X-Wakeshift-Time", strconv.FormatInt(now.Unix(), 10))
	httpx.WriteError(w, http.StatusUnauthorized, "bad_proof", "the key's signature doesn't check out (or the clock is more than 5 minutes off)")
}

// callBinding is what issue made of a call (step 2).
type callBinding struct {
	callHash, keyHash, threadHash []byte
	outcome                       string
}

// bindCall binds a call's ticket to its claim, if it has one that holds.
func bindCall(ctx context.Context, tx pgx.Tx, act actor, s sessions.Session, call, tool string, marked bool, now time.Time) (callBinding, error) {
	b := callBinding{callHash: callIDHash(call), outcome: bindingNoClaim}
	var app *string
	var contested bool
	var expires time.Time
	// FOR SHARE: a claim replacing an expired one waits for this binding.
	err := tx.QueryRow(ctx, `SELECT key_hash, thread_hash, app_id, contested, expires_at FROM connector_call_claims WHERE call_hash = $1 FOR SHARE`,
		b.callHash).Scan(&b.keyHash, &b.threadHash, &app, &contested, &expires)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return b, err
	case !expires.After(now):
	case contested:
		b.outcome = bindingContest
	case app != nil && *app != act.app:
		b.outcome = bindingApp
	case tool == "":
		b.outcome = bindingNoTool
	case s.Conversation != "" && string(threadHash(s.Conversation)) != string(b.threadHash):
		b.outcome = bindingThread
	default:
		var bound []byte
		// Any agent's binding of the conversation in the workspace counts: a
		// key bound for it under another agent binds nothing here.
		err := tx.QueryRow(ctx, `SELECT key_hash FROM connector_conversation_keys WHERE thread_hash = $1 ORDER BY (key_hash = $2) LIMIT 1`, b.threadHash, b.keyHash).Scan(&bound)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			b.outcome = bindingStored
		case err != nil:
			return b, err
		case string(bound) == string(b.keyHash):
			b.outcome = bindingStored
		default:
			b.outcome = bindingKey
		}
	}
	logCall("binding", b.outcome, act.app, "marked", marked)
	if b.outcome != bindingStored {
		b.keyHash, b.threadHash = nil, nil
	}
	return b, nil
}

// indexCall records which workspace (the transaction's) a bound ticket is
// in. One per call ID: an unexpired row keeps it (a call ID is the
// client's word, so another workspace's app can be sent the same one; its
// ticket isn't findable by it, and its hook falls back to the beacon).
func indexCall(ctx context.Context, tx pgx.Tx, hash []byte, app string, exp time.Time) error {
	// A live row of another workspace or app for the same call ID: neither
	// is redeemed by call (someone sent an app another call's ID).
	_, err := tx.Exec(ctx, `INSERT INTO connector_call_index (call_hash, tenant_id, app_id, expires_at) VALUES ($1, current_tenant(), $2, $3)
		ON CONFLICT (call_hash) DO UPDATE SET
			tenant_id = CASE WHEN connector_call_index.expires_at <= now() THEN current_tenant() ELSE connector_call_index.tenant_id END,
			app_id = CASE WHEN connector_call_index.expires_at <= now() THEN EXCLUDED.app_id ELSE connector_call_index.app_id END,
			ambiguous = CASE WHEN connector_call_index.expires_at <= now() THEN false
				ELSE connector_call_index.ambiguous OR connector_call_index.tenant_id <> current_tenant() OR connector_call_index.app_id <> EXCLUDED.app_id END,
			expires_at = CASE WHEN connector_call_index.expires_at <= now() THEN EXCLUDED.expires_at ELSE connector_call_index.expires_at END`, hash, app, exp)
	return err
}

// callProof is what a redeem by call proved: the key that signed it, and
// the tool and app the hook says the result came from.
type callProof struct {
	key  []byte
	tool string
	app  string
}

// callRefusal is a redeem by call refused before the ticket is used up.
type callRefusal struct{ code, msg string }

func (e *callRefusal) Error() string { return e.code }

var (
	errCallKey    = &callRefusal{"key_mismatch", "that call was claimed by another key"}
	errCallTool   = &callRefusal{"wrong_tool", "that call's ticket is for another tool"}
	errCallApp    = &callRefusal{"wrong_app", "that call's ticket is for another app"}
	errCallThread = &callRefusal{"wrong_thread", "that call's ticket is for another conversation"}
	errCallTwice  = &callRefusal{"ambiguous_call", "more than one open ticket names that call"}
	errCallConvo  = &callRefusal{"key_changed", "the conversation is bound to another key"}
	errCallLater  = &callRefusal{"contested", "another key claimed that call after its ticket was bound"}
)

// checkCallProof checks a redeem by call against its bound ticket (id, in
// its workspace's transaction), before it's used up. A refusal leaves the
// ticket to the result's beacon, as it always is: the beacon is a bearer
// credential (whoever holds the result can redeem it, by design and as
// before redeem by call), and redeem by call adds a path, not a gate.
func checkCallProof(ctx context.Context, tx pgx.Tx, p callProof, id string, hash []byte, session, app, tool, thread string) error {
	// First: two open bound tickets for one call ID (someone sent the app
	// another call's ID) are refused whichever was picked.
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM connector_tickets WHERE call_hash = $1 AND binding = 'stored' AND redeemed_at IS NULL AND expires_at > now()`,
		hash).Scan(&open); err != nil {
		return err
	}
	if open > 1 {
		return errCallTwice
	}
	var kh, th []byte
	if err := tx.QueryRow(ctx, `SELECT key_hash, claim_thread FROM connector_tickets WHERE id = $1`, id).Scan(&kh, &th); err != nil {
		return err
	}
	if string(kh) != string(keyHash(p.key)) {
		return errCallKey
	}
	// The call's claim, under its row's share lock in the redeem's
	// transaction (as bindCall reads it): a claim contesting it (UPDATE …
	// SET contested) waits for this redeem to commit, or this read waits
	// for that claim's. A claim by another key that came after the ticket
	// was bound (the plugin's own, late, after someone who saw the call's
	// ID claimed it first) contests the call as one before would, and a
	// claim row that no longer names the ticket's key isn't its claim:
	// either way nothing is redeemed by its ID, and the ticket stays for
	// the beacon. Checked after the key, so a stranger's attempt is still
	// counted and audited as a key mismatch. No claim row (pruned):
	// nothing contests it.
	var contested bool
	var claimKey []byte
	if err := tx.QueryRow(ctx, `SELECT contested, key_hash FROM connector_call_claims WHERE call_hash = $1 FOR SHARE`, hash).Scan(&contested, &claimKey); err == nil {
		if contested || string(claimKey) != string(kh) {
			return errCallLater
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if string(th) != string(threadHash(thread)) {
		return errCallThread
	}
	if p.app != "" && p.app != app { // "": the server's first sight; the hook pins the answer's app
		return errCallApp
	}
	if tool == "" || p.tool != tool {
		return errCallTool
	}
	var conv string
	if err := tx.QueryRow(ctx, `SELECT coalesce(conversation, '') FROM sessions WHERE id = $1`, session).Scan(&conv); err != nil {
		return err
	}
	if conv != "" && conv != thread {
		return errCallThread
	}
	// The conversation's binding in the workspace, whichever agent made it
	// (a row with another key sorts first).
	var bound []byte
	err := tx.QueryRow(ctx, `SELECT key_hash FROM connector_conversation_keys WHERE thread_hash = $1 ORDER BY (key_hash = $2) LIMIT 1`, threadHash(thread), keyHash(p.key)).Scan(&bound)
	if err == nil && string(bound) != string(keyHash(p.key)) {
		return errCallConvo
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

// bindConversationKey binds the conversation to the key that connected it
// by call (or records that it was used again).
// A redeem that raced another key's to bind the same conversation gets
// errCallConvo, which undoes the whole redeem (the ticket included).
func bindConversationKey(ctx context.Context, tx pgx.Tx, agent, thread string, key []byte) error {
	tag, err := tx.Exec(ctx, `INSERT INTO connector_conversation_keys (tenant_id, agent_id, thread_hash, key_hash) VALUES (current_tenant(), $1, $2, $3)
		ON CONFLICT (tenant_id, agent_id, thread_hash) DO UPDATE SET last_used_at = now() WHERE connector_conversation_keys.key_hash = EXCLUDED.key_hash`,
		agent, threadHash(thread), keyHash(key))
	if err == nil && tag.RowsAffected() == 0 {
		return errCallConvo
	}
	return err
}

// refusalFloor is the least time any answer of redeem-call but a success
// takes (paddedWriter): a refusal after a lookup in a workspace (a key
// mismatch, which also writes the audit entry) would otherwise take longer
// than one for a call nobody bound.
const refusalFloor = 75 * time.Millisecond

// paddedWriter holds an answer other than 200 until floor has passed since
// the request began.
type paddedWriter struct {
	http.ResponseWriter
	until time.Time
}

func (p *paddedWriter) WriteHeader(code int) {
	if code != http.StatusOK {
		if d := time.Until(p.until); d > 0 {
			time.Sleep(d)
		}
	}
	p.ResponseWriter.WriteHeader(code)
}

// redeemByCall redeems a call's bound ticket by the call's ID (step 3).
func (a *API) redeemByCall(w http.ResponseWriter, r *http.Request) {
	// Every answer but a success, from here on.
	w = &paddedWriter{ResponseWriter: w, until: time.Now().Add(refusalFloor)}
	if !a.RedeemByToolCall {
		httpx.WriteError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	var in callRedeemRequest
	if !httpx.Decode(w, r, &in) {
		return
	}
	c, _ := connectorversion.Parse(r.Header.Get(connectorversion.Header))
	if !in.ok() || len(in.Tool) > 200 {
		logRedeem(http.StatusBadRequest, "bad_request", "by_call", "", "", c)
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", `send {"call", "thread", "tool", "app", "key", "sig", "ts"}`)
		return
	}
	key, ok := hookproof.Verify(in.proof(), hookproof.Call, a.Issuer, in.Call, in.Thread, a.now())
	if !ok {
		logRedeem(http.StatusUnauthorized, "bad_proof", "by_call", "", "", c)
		badProof(w, a.now())
		return
	}
	hash := callIDHash(in.Call)
	var tenant, indexed string
	var ambiguous bool
	err := a.DB.Pool.QueryRow(r.Context(), `SELECT tenant_id::text, app_id, ambiguous FROM connector_call_index WHERE call_hash = $1 AND expires_at > $2`, hash, a.now()).Scan(&tenant, &indexed, &ambiguous)
	if err == nil && ambiguous {
		logCall("redeem", "ambiguous_call", indexed)
		err = pgx.ErrNoRows // two workspaces or apps minted it: the beacon's, not by call
	}
	if err == nil && in.App != "" && in.App != indexed {
		err = pgx.ErrNoRows // the hook's pinned app isn't the call's: as if there were none
	}
	if errors.Is(err, pgx.ErrNoRows) {
		logRedeem(http.StatusNotFound, "unknown_ticket", "by_call", "", "", c)
		httpx.WriteError(w, http.StatusNotFound, "unknown_ticket", "no ticket for that call, or it expired or was used")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal", "try again")
		return
	}
	a.redeemTicket(w, r, redeemRequest{Thread: in.Thread, Call: in.Call, Held: in.Held, Wait: in.Wait}, tenant, hash, &callProof{key: key, tool: in.Tool, app: in.App})
}

// PruneCallClaims deletes, at most limit of each at a time, redeem by
// call's platform rows an hour after they expire (claims and the call
// index) and lapsed session registrations. It
// returns how many it deleted.
func PruneCallClaims(ctx context.Context, d *db.DB, limit int) (int64, error) {
	var n int64
	if limit <= 0 {
		return 0, errors.New("appapi: PruneCallClaims needs a positive limit")
	}
	for _, q := range []string{
		// Each batch is picked once (MATERIALIZED), as CleanUp's.
		`WITH doomed AS MATERIALIZED (SELECT call_hash FROM connector_call_claims WHERE expires_at < now() - interval '1 hour' LIMIT $1)
			DELETE FROM connector_call_claims c USING doomed WHERE c.call_hash = doomed.call_hash`,
		`WITH doomed AS MATERIALIZED (SELECT call_hash FROM connector_call_index WHERE expires_at < now() - interval '1 hour' LIMIT $1)
			DELETE FROM connector_call_index c USING doomed WHERE c.call_hash = doomed.call_hash`,
		`WITH doomed AS MATERIALIZED (SELECT thread_hash FROM connector_sessions WHERE expires_at < now() LIMIT $1)
			DELETE FROM connector_sessions c USING doomed WHERE c.thread_hash = doomed.thread_hash`,
	} {
		tag, err := d.Pool.Exec(ctx, q, limit)
		if err != nil {
			return n, err
		}
		n += tag.RowsAffected()
	}
	return n, nil
}
