package appapi

// Grok Bot's Automations webhooks, on with the core flag
// WAKESHIFT_GROK_WEBHOOKS (API.Grok). A Grok Bot waits in the
// foreground (foregroundResult) until its own webhook is set up, once per
// bot:
//
//  1. Its first subscribing result (grokResult) asks it to make an Ed25519
//     key in a protected file in its folder and prove it on a register
//     challenge, with curl, straight to Wakeshift (grokProof): the app never
//     sees the proof. The proof's answer asks it to create a routine with a
//     webhook trigger and to give the user a one-time setup link.
//  2. On the page (web/src/pages/WebhookSetup.tsx) the user pastes the
//     webhook's URL and key (grokSetupSave). Wakeshift POSTs a fresh setup
//     challenge through the webhook; the run it starts, in the bot, signs it
//     with the bot's key, and only then is the webhook bound (grokProof,
//     grokBind). A leaked link alone binds nothing.
//  3. Another app's session in the same bot proves the same key on its own
//     register challenge and joins the bot's webhook (grokJoin), announced
//     through the webhook.
//
// Every refusal of a token or a challenge answers the same, byte for byte.
// Tokens, challenges, the webhook's URL and key, and proof bodies are never
// logged; what's logged is a category.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/botkeys"
	"github.com/wakeshift/server/internal/dblimit"
	"github.com/wakeshift/server/internal/framing"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/httpx"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

// GrokHooks is Grok Bot's webhook wake's configuration. Zero fields take
// the defaults (WAKESHIFT_GROK_* settings set them).
type GrokHooks struct {
	// Links makes and reads the setup page's tokens; Proofs, challenges.
	Links  *agenthooks.SetupLinks
	Proofs *botkeys.Codec
	// LinkTTL is how long a setup link works (default 24h).
	LinkTTL time.Duration
	// MaxAttempts is how many failed attempts end a setup link (default 5).
	MaxAttempts int
	// Counts counts Limits in Postgres, so they hold across instances;
	// ProxyHops is how many trusted proxies are in front, for the
	// client address (agentkit.ClientAddr).
	Counts    *dblimit.Counter
	ProxyHops int
	// RateLimits turns the request limits on (WAKESHIFT_GROK_RATE_LIMITS;
	// off by default until there's data): the per-address limits on saves,
	// describes and the page's failed checks, the per-link save limit and
	// the per-user bind cap. Off, no client address is read or stored. The
	// proof and challenge routes have no limit per address either way. The tokens' own
	// limits (5 failed attempts per link, 3 failed proofs per challenge,
	// single use, expiry, the body cap) and the identical refusals hold
	// either way, and so do delivery pacing and the failed-proofs alert.
	RateLimits bool
	Limits     GrokLimits
	// Now is the limits' clock (default time.Now).
	Now func() time.Time
	// Client POSTs the setup challenge (default agenthooks.Client()).
	Client *http.Client

	once sync.Once
}

// GrokLimits are the setup and proof routes' limits. A zero field takes
// its default; one with only Max set, the default's name and window
// (tests raise Max).
type GrokLimits struct {
	// SavePerAddress and SavePerLink: saves that send a test POST to
	// Cursor (5 per address and 3 per link an hour).
	SavePerAddress, SavePerLink dblimit.Limit
	// DescribePerAddress: the page's describe, which it polls (120 per
	// address per 10 minutes).
	DescribePerAddress dblimit.Limit
	// FailuresPerAddress: failed token checks on describe and save (20 per
	// address per 10 minutes); past it every token from the address is
	// refused as an invalid one would be. Proofs don't count and aren't
	// refused by it: bots' VMs may share outbound addresses, and a
	// challenge's 128-bit nonce and its 3 failed proofs already end
	// guessing.
	FailuresPerAddress dblimit.Limit
	// BindsPerUser: webhooks bound through the page per user (10 a day).
	BindsPerUser dblimit.Limit
	// FailedProofs: failed proofs across Wakeshift (30 an hour) that raise
	// an internal alert: signatures that don't verify on real, open
	// challenges, which only a holder of a challenge can send. Malformed or
	// unknown challenges, which anyone can send, don't count. It never
	// refuses anything: bots' VMs may share outbound addresses, so proofs
	// have no limit per address.
	FailedProofs dblimit.Limit
}

// Defaults.
const (
	DefaultGrokLinkTTL     = 24 * time.Hour
	DefaultGrokMaxAttempts = 5
)

// DefaultGrokLimits are the limits a zero GrokLimits field takes.
var DefaultGrokLimits = GrokLimits{
	SavePerAddress:     dblimit.Limit{Name: "grok-save-address", Max: 5, Window: time.Hour},
	SavePerLink:        dblimit.Limit{Name: "grok-save-link", Max: 3, Window: time.Hour},
	DescribePerAddress: dblimit.Limit{Name: "grok-describe-address", Max: 120, Window: 10 * time.Minute},
	FailuresPerAddress: dblimit.Limit{Name: "grok-failures-address", Max: 20, Window: 10 * time.Minute},
	BindsPerUser:       dblimit.Limit{Name: "grok-binds-user", Max: 10, Window: 24 * time.Hour},
	FailedProofs:       dblimit.Limit{Name: "grok-failed-proofs", Max: 30, Window: time.Hour},
}

func orLimit(l, d dblimit.Limit) dblimit.Limit {
	if l.Name == "" {
		l.Name = d.Name
	}
	if l.Max <= 0 {
		l.Max = d.Max
	}
	if l.Window <= 0 {
		l.Window = d.Window
	}
	return l
}

func (g *GrokHooks) init() {
	g.once.Do(func() {
		if g.LinkTTL <= 0 {
			g.LinkTTL = DefaultGrokLinkTTL
		}
		if g.MaxAttempts <= 0 {
			g.MaxAttempts = DefaultGrokMaxAttempts
		}
		d, l := DefaultGrokLimits, &g.Limits
		l.SavePerAddress = orLimit(l.SavePerAddress, d.SavePerAddress)
		l.SavePerLink = orLimit(l.SavePerLink, d.SavePerLink)
		l.DescribePerAddress = orLimit(l.DescribePerAddress, d.DescribePerAddress)
		l.FailuresPerAddress = orLimit(l.FailuresPerAddress, d.FailuresPerAddress)
		l.BindsPerUser = orLimit(l.BindsPerUser, d.BindsPerUser)
		l.FailedProofs = orLimit(l.FailedProofs, d.FailedProofs)
		if g.Now == nil {
			g.Now = time.Now
		}
		if g.Client == nil {
			g.Client = agenthooks.Client()
		}
	})
}

// grokOn: Grok Bot's webhook wake is on here.
func (a *API) grokOn() bool {
	if a.Grok == nil || a.HookSeal == nil || a.Grok.Links == nil || a.Grok.Proofs == nil || a.Grok.Counts == nil {
		return false
	}
	a.Grok.init()
	return true
}

// grokInstall is how a Grok Bot's VM gets the connector from here, in
// app's results (its register challenges' prove commands pin it).
func (a *API) grokInstall(app string) harnesses.GrokInstall {
	in := harnesses.GrokInstall{Base: a.base(), App: app}
	if a.Downloads != nil {
		in.Command = a.Downloads.GrokInstallCommand()
	}
	return in
}

// challengeID is a challenge's row ID, for the prove command.
func challengeID(ch string) string {
	c, err := botkeys.Parse(ch)
	if err != nil {
		return ""
	}
	return c.ID
}

// grokHarness is Grok Bot's entry.
func grokHarness() harnesses.Harness {
	h, _ := harnesses.Get("grok-bot")
	return h
}

// grokResult: a Grok Bot (Wake Foreground) with the webhook wake on.
// Subscribing says where the bot's webhook stands for this app (bound;
// key missing; dead; a setup link open; or the key step), always with the
// foreground command as the fallback. Other results add nothing, except,
// once, a line saying the webhook stopped working (GrokStoppedLine).
func (a *API) grokResult(ctx context.Context, tx pgx.Tx, cv *conversation, h harnesses.Harness, why resultFor) (toolResult, error) {
	s := cv.session
	var out toolResult
	app := appName(cv)
	d, registered, err := agenthooks.Get(ctx, tx, s.ID)
	if err != nil {
		return out, err
	}
	cursor := registered && d.Kind == agenthooks.KindCursor
	if why == forAnnotate {
		if !cursor || d.State != agenthooks.Dead || d.DeadTold {
			return out, nil
		}
		tag, err := tx.Exec(ctx, `UPDATE agent_webhooks SET dead_told_at = now() WHERE session_id = $1 AND state = 'dead' AND dead_told_at IS NULL`, s.ID)
		if err == nil && tag.RowsAffected() == 1 {
			out.Text = harnesses.GrokStoppedLine(app, d.DeadReason)
		}
		return out, err
	}
	// No bot ID yet (or one that can't be signed): the foreground text,
	// which asks for it. The key step comes with the next subscribing
	// result that has it.
	bot := s.Conversation
	if !botkeys.BotIDOK(bot) || !a.grokInstall("").OK() {
		// No bot ID yet, or the connector isn't built here for a Grok
		// Bot's VM: foreground waiting.
		return a.foregroundResult(ctx, tx, cv, h, why)
	}
	url, err := a.backgroundURL(ctx, tx, cv, h)
	if err != nil {
		return out, err
	}
	fg := harnesses.ForegroundNote(h, app, url, false)
	if cursor && d.State == agenthooks.Live && !d.NeedsSetup {
		out.Text = join(harnesses.GrokRegisteredNote(app), passSession(cv))
		return out, nil
	}
	key, hasKey, err := botkeys.GetKey(ctx, tx, s.ID)
	if err != nil {
		return out, err
	}
	missing := hasKey && key.Missing || cursor && d.NeedsSetup
	if !missing && !hasKey {
		// A bot that never registered a key here, and reported it missing.
		if missing, err = reportedMissing(ctx, tx, s.ID); err != nil {
			return out, err
		}
	}
	if !missing {
		if missing, err = setupKeyMissing(ctx, tx, s.ID, ""); err != nil {
			return out, err
		}
	}
	// A setup link open for this session (for a new key, after a missing
	// one), shown again rather than replaced.
	// A register challenge for the key step a lost key needs (the
	// routine's run can't fetch the setup check without the key): shown
	// again while open, so a status tool that subscribes doesn't mint one
	// on every poll.
	register := func() (string, error) {
		ch, err := a.Grok.Proofs.OpenRegister(ctx, tx, cv.tenant, s.ID, bot, cv.app.ID, cv.act.grant.ID)
		return challengeID(ch), err
	}
	if su, ok, err := openSetup(ctx, tx, s.ID); err != nil {
		return out, err
	} else if ok && (!missing || su.Rekey) {
		link := a.Grok.Links.URL(a.base(), agenthooks.Setup{Tenant: cv.tenant, ID: su.ID})
		reg, err := register()
		if err != nil {
			return out, err
		}
		out.Text = join(harnesses.GrokPendingNote(app, link, su.ExpiresAt, a.grokInstall(cv.app.ID), bot, reg, fg), passSession(cv))
		return out, nil
	}
	if missing {
		ch, err := a.Grok.Proofs.OpenRegister(ctx, tx, cv.tenant, s.ID, bot, cv.app.ID, cv.act.grant.ID)
		if err != nil {
			return out, err
		}
		out.Text = join(harnesses.GrokKeyMissingNote(app, a.grokInstall(cv.app.ID), bot, challengeID(ch), fg), passSession(cv))
		return out, nil
	}
	if hasKey && key.Bot == bot {
		dead := cursor && d.State == agenthooks.Dead
		had, err := hadSetup(ctx, tx, s.ID)
		if err != nil {
			return out, err
		}
		// Joined once, then stopped, has had no setup: the key step again
		// below (a proof joins again if the bot's webhook is live
		// elsewhere).
		if dead || had {
			link, su, err := a.newSetup(ctx, tx, cv.tenant, s, cv.app.ID, cv.act.grant.ID, key.Public, false)
			if err != nil {
				return out, err
			}
			reg, err := register()
			if err != nil {
				return out, err
			}
			if dead {
				out.Text = join(harnesses.GrokDeadNote(app, d.DeadReason, a.grokInstall(cv.app.ID), bot, reg, link, a.Grok.LinkTTL, fg), passSession(cv))
			} else {
				// Asked before: the brief text, with a new link for the same key.
				out.Text = join(harnesses.GrokPendingNote(app, link, su.ExpiresAt, a.grokInstall(cv.app.ID), bot, reg, fg), passSession(cv))
			}
			return out, nil
		}
	}
	ch, err := a.Grok.Proofs.OpenRegister(ctx, tx, cv.tenant, s.ID, bot, cv.app.ID, cv.act.grant.ID)
	if err != nil {
		return out, err
	}
	out.Text = join(harnesses.GrokKeyNote(app, a.grokInstall(cv.app.ID), bot, challengeID(ch), fg), passSession(cv))
	return out, nil
}

// reportedMissing: the session's last used challenge was used up by the
// bot's report that its key is gone.
func reportedMissing(ctx context.Context, tx pgx.Tx, session string) (bool, error) {
	var missing bool
	err := tx.QueryRow(ctx, `SELECT missing FROM bot_challenges WHERE session_id = $1 AND used_at IS NOT NULL ORDER BY used_at DESC LIMIT 1`, session).Scan(&missing)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return missing, err
}

// setupKeyMissing: the session's latest answered challenge (other than
// except) reported the bot's key missing, as a setup run's does without
// marking the session: its key is gone from the VM until a new one is
// proved.
func setupKeyMissing(ctx context.Context, tx pgx.Tx, session, except string) (bool, error) {
	var missing bool
	err := tx.QueryRow(ctx, `SELECT missing FROM bot_challenges WHERE session_id = $1 AND used_at IS NOT NULL AND id::text <> $2
		ORDER BY used_at DESC LIMIT 1`, session, except).Scan(&missing)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return missing, err
}

// setupRow is a webhook setup.
type setupRow struct {
	ID, Session, Agent, App, Grant, Link, Bot string
	Public                                    []byte
	Fingerprint                               []byte
	Rekey                                     bool
	ExpiresAt                                 time.Time
	ClaimID                                   string
	Claimed                                   bool // a save is POSTing its challenge
	Failures                                  int
	LastError                                 string
	Sealed                                    []byte
	Challenge                                 string
	Awaiting                                  bool // sealed, its proof still due
	Used, Expired                             bool
}

const setupColumns = `id::text, session_id::text, agent_id, app_id, coalesce(grant_id::text, ''), link_id, bot_id, public_key, key_fingerprint, rekey, expires_at,
	coalesce(claim_id, ''), coalesce(claimed_until > now(), false), failures, coalesce(last_error, ''), sealed, coalesce(challenge_id::text, ''),
	sealed IS NOT NULL AND coalesce(proof_due > now(), false), used_at IS NOT NULL, expires_at <= now()`

func scanSetup(row pgx.Row) (setupRow, error) {
	var s setupRow
	err := row.Scan(&s.ID, &s.Session, &s.Agent, &s.App, &s.Grant, &s.Link, &s.Bot, &s.Public, &s.Fingerprint, &s.Rekey, &s.ExpiresAt,
		&s.ClaimID, &s.Claimed, &s.Failures, &s.LastError, &s.Sealed, &s.Challenge, &s.Awaiting, &s.Used, &s.Expired)
	return s, err
}

// openSetup is the session's newest setup that still works: unused,
// unexpired, under its attempts.
func openSetup(ctx context.Context, tx pgx.Tx, session string) (setupRow, bool, error) {
	s, err := scanSetup(tx.QueryRow(ctx, `SELECT `+setupColumns+` FROM agent_webhook_setups
		WHERE session_id = $1 AND used_at IS NULL AND expires_at > now() + interval '5 minutes' AND failures < $2
		ORDER BY created_at DESC LIMIT 1`, session, DefaultGrokMaxAttempts))
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	// A link works only while the stream link it was minted under does: an
	// unwatch (or the grant's end) revoked that one, so this link is dead,
	// and showing it again would leave the user stuck with a link that
	// can't be used. The caller mints a new one under the session's
	// current stream link instead.
	if _, err := streams.LinkLive(ctx, tx, s.Link); errors.Is(err, streams.ErrLinkInvalid) {
		return s, false, nil
	} else if err != nil {
		return s, false, err
	}
	return s, true, nil
}

// hadSetup: the session had a setup link before.
func hadSetup(ctx context.Context, tx pgx.Tx, session string) (bool, error) {
	var had bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_webhook_setups WHERE session_id = $1)`, session).Scan(&had)
	return had, err
}

// grokLink is the session's stream link (its URL and ID): a setup works
// only while it does.
func (a *API) grokLink(ctx context.Context, tx pgx.Tx, tenant string, s sessions.Session, app, grant string) (url, id string, err error) {
	createdBy := "app:" + app
	link, err := a.Streams.SessionLink(ctx, tx, tenant, s.Agent, s.ID, createdBy, grant)
	if err == nil && link == "" {
		link, _, err = a.Streams.Mint(ctx, tx, tenant, streams.NewLink{Agent: s.Agent, Target: s.ID, CreatedBy: createdBy,
			GrantID: grant, TTL: streams.SessionLinkTTL, Slide: streams.SessionLinkMax})
	}
	if err != nil {
		return "", "", err
	}
	t, id, ok := a.Streams.Held(link)
	if !ok || t != tenant {
		return "", "", errors.New("appapi: the session's link isn't one this server made")
	}
	return link, id, nil
}

// newSetup mints a setup link for the session's bot and key pub (rekey:
// the bind replaces the bot's key), working for LinkTTL.
func (a *API) newSetup(ctx context.Context, tx pgx.Tx, tenant string, s sessions.Session, app, grant string, pub ed25519.PublicKey, rekey bool) (string, setupRow, error) {
	var su setupRow
	_, linkID, err := a.grokLink(ctx, tx, tenant, s, app, grant)
	if err != nil {
		return "", su, err
	}
	su, err = scanSetup(tx.QueryRow(ctx, `INSERT INTO agent_webhook_setups (tenant_id, id, session_id, agent_id, app_id, grant_id, link_id, bot_id, public_key, key_fingerprint, rekey, expires_at)
		VALUES (current_tenant(), gen_random_uuid(), $1, $2, $3, nullif($4, '')::uuid, $5, $6, $7, $8, $9, now() + $10::interval)
		RETURNING `+setupColumns, s.ID, s.Agent, app, grant, linkID, s.Conversation, []byte(pub), botkeys.Fingerprint(pub), rekey, a.Grok.LinkTTL.String()))
	if err != nil {
		return "", su, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO bot_setup_targets (tenant_id) VALUES (current_tenant()) ON CONFLICT DO NOTHING`); err != nil {
		return "", su, err
	}
	return a.Grok.Links.URL(a.base(), agenthooks.Setup{Tenant: tenant, ID: su.ID}), su, nil
}

// grokForeground is ForegroundNote's text for a session, outside a tool
// call (the proof route): its wait URL on its stream link.
func (a *API) grokForeground(ctx context.Context, tx pgx.Tx, tenant string, s sessions.Session, app, appName, grant string) (string, error) {
	link, _, err := a.grokLink(ctx, tx, tenant, s, app, grant)
	if err != nil {
		return "", err
	}
	return harnesses.ForegroundNote(grokHarness(), appName, streams.WaitURL(link), false), nil
}

// appNameTx is an app's name as Wakeshift's sentences use it, read in
// the transaction (apps is platform-level): never another connection while
// a row is locked.
func appNameTx(ctx context.Context, tx pgx.Tx, id string) string {
	var name string
	if err := tx.QueryRow(ctx, `SELECT name FROM apps WHERE id = $1`, id).Scan(&name); err != nil || name == "" {
		name = id
	}
	return framing.Name(name)
}

// Routes. Under /v1/stream, whose query strings no request log keeps;
// tokens and challenges are in the body anyway.
const (
	GrokProofPath    = "/v1/stream/proof"
	GrokFetchPath    = "/v1/stream/proof/challenge"
	GrokDescribePath = "/v1/stream/webhook-setup/describe"
	GrokSavePath     = "/v1/stream/webhook-setup"
)

// grokMaxBody caps the setup and proof routes' bodies.
const grokMaxBody = 4 << 10

// errGrokRefused is every refusal of a challenge or token: one answer.
var errGrokRefused = errors.New("refused")

// grokOff answers 404 with the webhook wake off.
func grokOff(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusNotFound, "not_found", "not found")
}

// grokText answers the agent in plain text.
func grokText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	io.WriteString(w, text+"\n")
}

// grokAnswer is what a proof's transaction decided.
type grokAnswer struct {
	status int
	text   string
	// after the commit: join the bot's webhook (peers), bind's follow-ups.
	joinPeers  []agenthooks.Peer
	join       *joinTry
	bound      *boundFollowUp
	logOutcome string
	// app is the challenge's app, once it's found; failed, a signature
	// that didn't verify (for FailedProofs).
	app    string
	failed bool
	// proofMs: a bind's time from the setup challenge to its proof.
	proofMs int64
}

type joinTry struct {
	tenant, app, appName, grant, bot string
	session                          sessions.Session
	pub                              ed25519.PublicKey
	fg                               string
}

type boundFollowUp struct {
	tenant, session string
	routine         []byte
	agent, bot      string
	hook            agenthooks.CursorHook
	pub             ed25519.PublicKey
	binding         []byte
	// peers: the other destinations on the bind's own key, when the bind
	// was a re-setup by that key (grokBind).
	peers []agenthooks.Peer
}

// grokProof is POST /v1/stream/proof (form fields, as curl -F sends them):
// challenge, public_key and signature, a bot's proof of its key over a
// challenge Wakeshift issued; or challenge and missing=1, the bot's report
// that its key file is gone. It answers in plain text for the agent.
func (a *API) grokProof(w http.ResponseWriter, r *http.Request) {
	if !a.grokOn() {
		grokOff(w)
		return
	}
	// No limit per address, not even on failed checks: bots' VMs may share
	// outbound addresses, so one bot's failures (or someone's garbage)
	// must not lock out another's proofs. A challenge's 128-bit nonce, and
	// its end after 3 failed proofs, are what stop guessing.
	ctx := r.Context()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, grokMaxBody))
	if err != nil {
		grokText(w, http.StatusRequestEntityTooLarge, "That request was too large. Run the command exactly as given.")
		return
	}
	// A private key is refused before anything else reads the body, and
	// neither it nor the body is logged.
	if botkeys.HoldsPrivateKey(body) {
		slog.Warn("grok webhooks: proof refused", "reason", "private_key")
		grokText(w, http.StatusBadRequest, harnesses.GrokPrivateKeyNote)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err := r.ParseMultipartForm(grokMaxBody); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		grokText(w, http.StatusBadRequest, "That request wasn't the form the command sends. Run the command exactly as given.")
		return
	}
	ch, err := botkeys.Parse(r.PostFormValue("challenge"))
	if err != nil {
		slog.Info("grok webhooks: proof refused", "reason", "malformed")
		grokText(w, http.StatusUnauthorized, harnesses.GrokChallengeGone)
		return
	}
	// A nonce Wakeshift didn't derive (a made-up challenge) is refused
	// before the database: guessing costs no lookup.
	if !a.Grok.Proofs.Authentic(ch) {
		slog.Info("grok webhooks: proof refused", "reason", "forged")
		grokText(w, http.StatusUnauthorized, harnesses.GrokChallengeGone)
		return
	}
	missing := strings.TrimSpace(r.PostFormValue("missing")) == "1"
	// v=2: a connector that was shown the register challenge's workspace
	// and app (grokFetch) signs them too (botkeys.RegisterMessage).
	v2 := strings.TrimSpace(r.PostFormValue("v")) == "2"
	pub, pubErr := botkeys.ParsePublicKey(r.PostFormValue("public_key"))
	sig, sigOK := botkeys.ParseSignature(r.PostFormValue("signature"))
	var ans grokAnswer
	err = a.DB.InTenant(ctx, ch.Tenant, func(tx pgx.Tx) error {
		var err error
		ans, err = a.proveTx(ctx, tx, ch, missing, v2, pub, pubErr == nil, sig, sigOK)
		return err
	})
	switch {
	case errors.Is(err, errGrokRefused):
		slog.Info("grok webhooks: proof refused", "reason", cmpOr(ans.logOutcome, "challenge_refused"), "app", ans.app)
		grokText(w, http.StatusUnauthorized, harnesses.GrokChallengeGone)
		return
	case err != nil:
		slog.Error("grok webhooks: a proof failed", "err", err) // pgx errors don't carry arguments
		grokText(w, http.StatusServiceUnavailable, "Something went wrong at Wakeshift. Run the command again in a minute.")
		return
	}
	if ans.failed {
		// A signature that doesn't verify on a real challenge: limited per
		// challenge and per link (botkeys.MaxFailures, MaxAttempts), not
		// per address, and the only failure the alert counts.
		a.grokProofFailed(ctx, ans.app)
	}
	if ans.join != nil {
		joined, err := a.grokJoin(ctx, *ans.join, ans.joinPeers)
		if err != nil {
			slog.Error("grok webhooks: a join failed", "err", err)
		}
		if joined {
			ans.text, ans.logOutcome = harnesses.GrokJoinedNote(ans.join.appName), "joined"
		} else {
			// Nothing to join: set up the webhook.
			err = a.DB.InTenant(ctx, ans.join.tenant, func(tx pgx.Tx) error {
				link, _, err := a.newSetup(ctx, tx, ans.join.tenant, ans.join.session, ans.join.app, ans.join.grant, ans.join.pub, false)
				if err == nil {
					ans.text = harnesses.GrokWebhookNote(ans.join.appName, a.base(), ans.join.bot, link, a.Grok.LinkTTL, ans.join.fg)
				}
				return err
			})
			if err != nil {
				slog.Error("grok webhooks: minting a setup link failed", "err", err)
				grokText(w, http.StatusServiceUnavailable, "Something went wrong at Wakeshift. Run the command again in a minute.")
				return
			}
		}
	}
	if b := ans.bound; b != nil {
		if a.HookWake != nil {
			a.HookWake(b.tenant, b.agent) // what waited goes at once
		}
		a.grokPropagate(ctx, *b)
	}
	// Metrics (the hosted deployment's grok_proofs and grok_proof_ms):
	// the outcome, the app's ID and, for a bind, the time from the setup
	// challenge to its proof. Nothing about the bot or the user.
	slog.Info("grok webhooks: proof", "outcome", ans.logOutcome, "app", ans.app, "proof_ms", ans.proofMs)
	grokText(w, cmp.Or(ans.status, http.StatusOK), ans.text)
}

// grokNoChallenge is the one answer the fetch route gives when it has no
// challenge to give: none open, an unknown bot, a malformed request.
const grokNoChallenge = "No Wakeshift check is waiting for this bot."

// grokFetch is POST /v1/stream/proof/challenge (form fields bot and, for a
// register challenge, challenge: its ID), what `wakeshift grok prove`
// calls: the open challenge to sign, as plain text, so the challenge never
// passes through a webhook's body or a tool's result into a command. A
// challenge isn't a secret (only the bot's key signs it, once, within its
// lifetime), but the route says nothing else: it answers 404 with the same
// text when it has none to give, whatever the reason. Without an ID it
// gives the bot's open setup challenge only when there's exactly one (a
// save waits while another is open for the bot: grokSetupSave), so the
// run a webhook started signs the check that webhook carried.
func (a *API) grokFetch(w http.ResponseWriter, r *http.Request) {
	if !a.grokOn() {
		grokOff(w)
		return
	}
	none := func() { grokText(w, http.StatusNotFound, grokNoChallenge) }
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, grokMaxBody))
	if err != nil {
		none()
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err := r.ParseMultipartForm(grokMaxBody); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		none()
		return
	}
	bot, id := r.PostFormValue("bot"), r.PostFormValue("challenge")
	purpose := botkeys.Setup
	if id != "" {
		purpose = botkeys.Register
	}
	if !botkeys.BotIDOK(bot) || (id != "" && !uuidRE.MatchString(id)) {
		none()
		return
	}
	// A setup challenge goes only to the key its setup was minted for: the
	// fetch is signed (botkeys.FetchMessage), so knowing the bot's ID isn't
	// enough to read the check and use it up. A register challenge is
	// named by its ID, which only the subscribing result carries.
	var fp []byte
	if purpose == botkeys.Setup {
		pub, err := botkeys.ParsePublicKey(r.PostFormValue("public_key"))
		sig, sigOK := botkeys.ParseSignature(r.PostFormValue("signature"))
		if err != nil || !sigOK || !botkeys.VerifyFetch(pub, bot, r.PostFormValue("time"), sig, time.Now()) {
			slog.Info("grok webhooks: challenge fetched", "outcome", "unsigned", "purpose", purpose)
			none()
			return
		}
		fp = botkeys.Fingerprint(pub)
	}
	ctx := r.Context()
	found, ok, err := botkeys.FindOpen(ctx, a.DB.Pool, a.DB.InTenant, bot, purpose, id, fp)
	if err != nil {
		slog.Error("grok webhooks: fetching a challenge failed", "err", err)
		grokText(w, http.StatusServiceUnavailable, "Something went wrong at Wakeshift. Run the command again in a minute.")
		return
	}
	if !ok {
		slog.Info("grok webhooks: challenge fetched", "outcome", "none", "purpose", purpose)
		none()
		return
	}
	text := a.Grok.Proofs.Challenge(purpose, found.Tenant, found.ID)
	if purpose == botkeys.Register && strings.TrimSpace(r.PostFormValue("v")) == "2" {
		// A v2 connector shows which app, in which workspace, the proof
		// goes to, refuses another app than its command pins (--app), and
		// signs both (botkeys.RegisterMessage).
		var about string
		err := a.DB.InTenant(ctx, found.Tenant, func(tx pgx.Tx) error {
			var err error
			about, err = registerAbout(ctx, tx, found.Tenant, found.ID)
			return err
		})
		if errors.Is(err, pgx.ErrNoRows) {
			slog.Info("grok webhooks: challenge fetched", "outcome", "none", "purpose", purpose)
			none()
			return
		}
		if err != nil {
			slog.Error("grok webhooks: fetching a challenge failed", "err", err)
			grokText(w, http.StatusServiceUnavailable, "Something went wrong at Wakeshift. Run the command again in a minute.")
			return
		}
		text += "\n" + about
	}
	slog.Info("grok webhooks: challenge fetched", "outcome", "given", "purpose", purpose)
	grokText(w, http.StatusOK, text)
}

// registerAbout is what a v2 fetch of a register challenge adds, one
// "key=value" line each: the app's ID and name, and the workspace's ID,
// name and, for a workspace an app owns, that app's ID. Names are one line
// (framing.Name).
func registerAbout(ctx context.Context, tx pgx.Tx, tenant, id string) (string, error) {
	var app string
	if err := tx.QueryRow(ctx, `SELECT app_id FROM bot_challenges WHERE id = $1`, id).Scan(&app); err != nil {
		return "", err
	}
	var name, owner string
	if err := tx.QueryRow(ctx, `SELECT name, coalesce(app_id, '') FROM tenants WHERE id = $1`, tenant).Scan(&name, &owner); err != nil {
		return "", err
	}
	ws := framing.Name(name)
	if strings.TrimSpace(name) == "" {
		ws = tenant
	}
	lines := []string{"app_id=" + app, "app=" + appNameTx(ctx, tx, app), "workspace_id=" + tenant, "workspace=" + ws}
	if owner != "" {
		lines = append(lines, "workspace_app="+owner)
	}
	return strings.Join(lines, "\n"), nil
}

// proveTx is a proof's transaction, in the challenge's workspace.
func (a *API) proveTx(ctx context.Context, tx pgx.Tx, ch botkeys.Challenge, missing, v2 bool, pub ed25519.PublicKey, pubOK bool, sig []byte, sigOK bool) (grokAnswer, error) {
	var ans grokAnswer
	row, err := botkeys.Lock(ctx, tx, ch)
	if errors.Is(err, botkeys.ErrUnknown) {
		ans.logOutcome = "unknown"
		return ans, errGrokRefused
	}
	if err != nil {
		return ans, err
	}
	ans.app = row.App
	s, err := sessions.Get(ctx, tx, row.Session)
	if errors.Is(err, sessions.ErrNotFound) {
		ans.logOutcome = "session_gone"
		return ans, errGrokRefused
	}
	if err != nil {
		return ans, err
	}
	tenant := ch.Tenant
	challenge := ch.String()
	if row.Used || row.Expired {
		// A retried setup run proving twice: the second is refused as used,
		// but told wake-ups are on if they are (only a holder of the key
		// learns it).
		if row.Used && !missing && row.Purpose == botkeys.Setup && sigOK {
			if on, err := a.setupBoundBy(ctx, tx, row, challenge, sig); err != nil {
				return ans, err
			} else if on {
				ans.text, ans.logOutcome = harnesses.GrokBoundNote(appNameTx(ctx, tx, row.App)), "already_bound"
				return ans, nil
			}
		}
		ans.logOutcome = "used_or_expired"
		return ans, errGrokRefused
	}
	// The access the challenge was handed out under must still stand (a
	// revoked grant revoked the session's links and its webhook).
	if row.Grant != "" {
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM grants WHERE id = $1 AND revoked_at IS NULL AND expires_at > now())`, row.Grant).Scan(&live); err != nil {
			return ans, err
		}
		if !live {
			ans.logOutcome = "access_ended"
			return ans, errGrokRefused
		}
	}
	appName := appNameTx(ctx, tx, row.App)
	fg, err := a.grokForeground(ctx, tx, tenant, s, row.App, appName, row.Grant)
	if errors.Is(err, errGrantGone) || errors.Is(err, streams.ErrLinkInvalid) {
		ans.logOutcome = "access_ended"
		return ans, errGrokRefused
	}
	if err != nil {
		return ans, err
	}

	if missing {
		return a.keyMissing(ctx, tx, tenant, s, row, appName)
	}

	// The key a setup proof must be signed by is the one its link was
	// minted for; a register proof brings its own.
	var su setupRow
	verifyWith := pub
	if row.Purpose == botkeys.Setup {
		su, err = scanSetup(tx.QueryRow(ctx, `SELECT `+setupColumns+` FROM agent_webhook_setups WHERE id = $1 FOR UPDATE`, row.SetupID))
		if errors.Is(err, pgx.ErrNoRows) {
			ans.logOutcome = "setup_gone"
			return ans, errGrokRefused
		}
		if err != nil {
			return ans, err
		}
		verifyWith = ed25519.PublicKey(su.Public)
		if pubOK && !bytes.Equal(botkeys.Fingerprint(pub), su.Fingerprint) {
			sigOK = false // signed by another key than the link's
		}
	}
	verified := false
	switch {
	case !sigOK || (row.Purpose == botkeys.Register && !pubOK):
	case row.Purpose == botkeys.Register && v2:
		// Signed with the workspace and app the connector showed: this
		// workspace and the challenge's app, or it doesn't verify.
		verified = botkeys.VerifyRegister(verifyWith, row.Bot, challenge, tenant, row.App, sig)
	default:
		verified = botkeys.Verify(verifyWith, row.Bot, challenge, sig)
	}
	if !verified {
		if row.Purpose == botkeys.Setup {
			// The run the webhook started can't sign for this bot: the
			// webhook starts runs elsewhere (a leaked link pasted with
			// someone else's webhook). The attempt fails now, and counts, so
			// the user's link is free for another try.
			if err := botkeys.Use(ctx, tx, row.ID); err != nil {
				return ans, err
			}
			if _, err := tx.Exec(ctx, `UPDATE agent_webhook_setups SET sealed = NULL, challenge_id = NULL, proof_due = NULL, failures = failures + 1, last_error = 'unconfirmed'
				WHERE id = $1 AND challenge_id = $2 AND used_at IS NULL`, su.ID, row.ID); err != nil {
				return ans, err
			}
			ans.status, ans.text, ans.logOutcome, ans.failed = http.StatusUnauthorized, harnesses.GrokSetupMismatchNote, "setup_mismatch", true
			return ans, nil
		}
		// A signature that doesn't verify earns no more than the challenge
		// itself did: not the session's wait URL (fg).
		ended, err := botkeys.Fail(ctx, tx, row.ID)
		if err != nil {
			return ans, err
		}
		ans.status, ans.logOutcome, ans.failed = http.StatusUnauthorized, "bad_signature", true
		if ended {
			ans.text = harnesses.GrokChallengeGone
			return ans, nil
		}
		ans.text = harnesses.GrokBadSignatureNote
		return ans, nil
	}
	if err := botkeys.Use(ctx, tx, row.ID); err != nil {
		return ans, err
	}
	if row.Purpose == botkeys.Setup {
		return a.grokBind(ctx, tx, tenant, s, row, su, appName)
	}
	return a.registered(ctx, tx, tenant, s, row, pub, appName, fg)
}

// setupBoundBy: a used setup challenge's setup bound the webhook, and sig
// over it verifies with the setup's key.
func (a *API) setupBoundBy(ctx context.Context, tx pgx.Tx, row botkeys.Row, challenge string, sig []byte) (bool, error) {
	var pub []byte
	err := tx.QueryRow(ctx, `SELECT su.public_key FROM agent_webhook_setups su JOIN agent_webhooks w ON w.tenant_id = su.tenant_id AND w.session_id = su.session_id
		WHERE su.id = $1 AND su.used_at IS NOT NULL AND w.kind = 'cursor-automations' AND w.state = 'live' AND w.key_fingerprint = su.key_fingerprint`, row.SetupID).Scan(&pub)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return botkeys.Verify(pub, row.Bot, challenge, sig), nil
}

// keyMissing: the bot reported its key file gone on a valid challenge.
// The challenge is used up, and the answer tells the agent to tell the
// user, with a new register challenge for setting up again.
//
// On a setup challenge (the run a pasted webhook started), only that
// attempt fails (key_missing, which the page shows): the session's
// binding, if it has one, and its key stay as they are, and later
// subscribing results offer the new key while the attempt is open
// (setupKeyMissing). On a register challenge, the session (only this
// one: a report marks only the session whose challenge carried it) needs
// setup in what its subscribing results say.
//
// A report is unsigned (the key is gone), so anyone holding the challenge
// could send one: it changes no stored key and no destination. A session
// whose bot registered a key keeps it, and its webhook keeps delivering,
// until a proof by a new key is bound through the page; a session without
// one has nothing to change. Nor does the answer repeat the foreground
// command, which only a proof earns.
func (a *API) keyMissing(ctx context.Context, tx pgx.Tx, tenant string, s sessions.Session, row botkeys.Row, appName string) (grokAnswer, error) {
	fg := harnesses.GrokForegroundAsGiven
	var ans grokAnswer
	if _, err := tx.Exec(ctx, `UPDATE bot_challenges SET used_at = now(), missing = true WHERE id = $1`, row.ID); err != nil {
		return ans, err
	}
	// The answer says what's true: with the session's webhook live, wake-ups
	// go on (delivery doesn't need the key), and the key is needed only to
	// change the setup; without one, they're off until it's set up again.
	d, has, err := agenthooks.Get(ctx, tx, s.ID)
	if err != nil {
		return ans, err
	}
	live := has && d.Kind == agenthooks.KindCursor && d.State == agenthooks.Live && !d.NeedsSetup
	note := func(ch string) string {
		if live {
			return harnesses.GrokKeyMissingLiveNote(appName, a.grokInstall(row.App), row.Bot, challengeID(ch))
		}
		return harnesses.GrokKeyMissingNote(appName, a.grokInstall(row.App), row.Bot, challengeID(ch), fg)
	}
	if row.Purpose == botkeys.Setup {
		if _, err := tx.Exec(ctx, `UPDATE agent_webhook_setups SET sealed = NULL, challenge_id = NULL, proof_due = NULL, failures = failures + 1, last_error = 'key_missing'
			WHERE id = $1 AND challenge_id = $2 AND used_at IS NULL`, row.SetupID, row.ID); err != nil {
			return ans, err
		}
		ch, _, err := a.Grok.Proofs.Issue(ctx, tx, tenant, botkeys.Register, s.ID, row.Bot, row.App, row.Grant, "")
		if err != nil {
			return ans, err
		}
		ans.text, ans.logOutcome = note(ch), "key_missing_setup"
		return ans, nil
	}
	ch, _, err := a.Grok.Proofs.Issue(ctx, tx, tenant, botkeys.Register, s.ID, row.Bot, row.App, row.Grant, "")
	if err != nil {
		return ans, err
	}
	ans.text, ans.logOutcome = note(ch), "key_missing"
	return ans, nil
}

// registered: a register proof checked out. A first key is registered; the
// same key again (it may have been reported missing: it's back) joins the
// bot's webhook if it has a live one through another app, else gets a
// setup link; a different key changes nothing until the user confirms it
// on the page (GrokRekeyNote), or, after a missing key, gets a link that
// replaces the old key once bound.
func (a *API) registered(ctx context.Context, tx pgx.Tx, tenant string, s sessions.Session, row botkeys.Row, pub ed25519.PublicKey, appName, fg string) (grokAnswer, error) {
	var ans grokAnswer
	if s.Conversation != row.Bot {
		ans.logOutcome = "bot_changed"
		return ans, errGrokRefused
	}
	key, hasKey, err := botkeys.GetKey(ctx, tx, s.ID)
	if err != nil {
		return ans, err
	}
	d, registered, err := agenthooks.Get(ctx, tx, s.ID)
	if err != nil {
		return ans, err
	}
	fp := botkeys.Fingerprint(pub)
	needsSetup := hasKey && key.Missing || registered && d.Kind == agenthooks.KindCursor && d.NeedsSetup
	if !needsSetup {
		// A setup run reported the key missing: a new key replaces it.
		if needsSetup, err = setupKeyMissing(ctx, tx, s.ID, row.ID); err != nil {
			return ans, err
		}
	}
	switch {
	case !hasKey:
		if _, err := botkeys.PutKey(ctx, tx, s.ID, row.Bot, row.App, pub); err != nil {
			return ans, err
		}
		if err := audit.Record(ctx, tx, audit.Entry{Actor: "agent", Action: audit.BotKeyRegistered, AgentID: s.Agent, AppID: row.App, GrantID: row.Grant,
			Detail: map[string]any{"session": s.Handle, "fingerprint": botkeys.Short(fp)}}); err != nil {
			return ans, err
		}
	case bytes.Equal(key.Fingerprint, fp):
		if needsSetup {
			// The key is back (it was reported missing, perhaps by the app):
			// wake-ups resume.
			if err := botkeys.ClearMissing(ctx, tx, s.ID); err != nil {
				return ans, err
			}
			if _, err := tx.Exec(ctx, `UPDATE agent_webhooks SET needs_setup_at = NULL WHERE session_id = $1`, s.ID); err != nil {
				return ans, err
			}
			registered = registered && d.Kind == agenthooks.KindCursor
			d.NeedsSetup = false
		}
	default:
		// A different key: nothing changes until the user's paste on the
		// page and a proof through the webhook.
		link, _, err := a.newSetup(ctx, tx, tenant, s, row.App, row.Grant, pub, true)
		if err != nil {
			return ans, err
		}
		if needsSetup {
			ans.text, ans.logOutcome = harnesses.GrokWebhookNote(appName, a.base(), row.Bot, link, a.Grok.LinkTTL, fg), "rekey_after_missing"
		} else {
			ans.text, ans.logOutcome = harnesses.GrokRekeyNote(botkeys.Short(key.Fingerprint), botkeys.Short(fp), link, fg), "rekey"
		}
		return ans, nil
	}
	if registered && d.Kind == agenthooks.KindCursor && d.State == agenthooks.Live && !d.NeedsSetup {
		ans.text, ans.logOutcome = harnesses.GrokBoundNote(appName), "already_bound"
		return ans, nil
	}
	// A live webhook for this key elsewhere: join it after the commit.
	peers, err := agenthooks.PeersByKey(ctx, tx, fp)
	if err != nil {
		return ans, err
	}
	var others []agenthooks.Peer
	for _, p := range peers {
		if p.Tenant != tenant || p.Session != s.ID {
			others = append(others, p)
		}
	}
	if len(others) > 0 {
		ans.joinPeers = others
		ans.join = &joinTry{tenant: tenant, app: row.App, appName: appName, grant: row.Grant, bot: row.Bot, session: s, pub: pub, fg: fg}
		ans.logOutcome = "join"
		return ans, nil
	}
	link, _, err := a.newSetup(ctx, tx, tenant, s, row.App, row.Grant, pub, false)
	if err != nil {
		return ans, err
	}
	ans.text, ans.logOutcome = harnesses.GrokWebhookNote(appName, a.base(), row.Bot, link, a.Grok.LinkTTL, fg), "registered"
	return ans, nil
}

// grokJoin joins the session (j) to the bot's live webhook through another
// app (one of peers, found by the key's fingerprint after the proof checked
// out): the peer's destination must be live, not need setup, and be for
// the same bot ID and key. Its URL and key are copied into the session's
// workspace, sealed for it, and the join is announced through the webhook.
func (a *API) grokJoin(ctx context.Context, j joinTry, peers []agenthooks.Peer) (bool, error) {
	fp := botkeys.Fingerprint(j.pub)
	for _, p := range peers {
		var hook agenthooks.CursorHook
		var binding []byte
		ok := false
		err := a.DB.InTenant(ctx, p.Tenant, func(tx pgx.Tx) error {
			d, has, err := agenthooks.Get(ctx, tx, p.Session)
			if err != nil || !has || d.Kind != agenthooks.KindCursor || d.State != agenthooks.Live || d.NeedsSetup || d.Bot != j.bot || !bytes.Equal(d.Fingerprint, fp) {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT removal_hash FROM agent_webhooks WHERE session_id = $1`, p.Session).Scan(&binding); err != nil {
				return err
			}
			hook, err = agenthooks.OpenDestination(ctx, tx, a.HookSeal, p.Tenant, p.Session)
			if err != nil {
				slog.Warn("grok webhooks: a peer's destination can't be opened", "reason", "unreadable")
				return nil
			}
			ok = true
			return nil
		})
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		joined := false
		routine := a.Grok.Links.Routine(j.bot, hook.URL)
		err = a.DB.InTenant(ctx, j.tenant, func(tx pgx.Tx) error {
			// The routine's binding first, locked for share: a bind on the
			// routine committed since the read above wrote a new one, and a
			// join copying the old would copy an older webhook key.
			if cur, ok, err := agenthooks.RoutineBinding(ctx, tx, routine); err != nil || (ok && !bytes.Equal(cur, binding)) {
				return err
			}
			// The peer's index row, held until this commits: one removed
			// first leaves nothing to join. Its binding must still be the one
			// read above: a re-setup in between wrote a new hook and binding,
			// and a join copying the old ones would keep the old webhook key,
			// so it joins nothing and the bot gets the setup path.
			var still bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM bot_webhooks WHERE tenant_id = $1 AND session_id = $2 AND fingerprint = $3 AND removal_hash = $4 FOR SHARE)`,
				p.Tenant, p.Session, fp, binding).Scan(&still); err != nil || !still {
				return err
			}
			replaced, err := agenthooks.RegisterCursor(ctx, tx, a.HookSeal, j.tenant, agenthooks.Destination{Session: j.session.ID, Agent: j.session.Agent,
				App: j.app, Grant: j.grant, Bot: j.bot, Fingerprint: fp, Routine: routine}, hook, binding, true, true)
			if err != nil {
				return err
			}
			joined = true
			return audit.Record(ctx, tx, audit.Entry{Actor: "agent", Action: audit.AgentWebhookRegistered, AgentID: j.session.Agent, AppID: j.app, GrantID: j.grant,
				Detail: map[string]any{"session": j.session.Handle, "kind": agenthooks.KindCursor, "replaced": replaced, "via": "bot_key", "joined": true}})
		})
		if err != nil {
			return false, err
		}
		if joined {
			if a.HookWake != nil {
				a.HookWake(j.tenant, j.session.Agent) // the announcement, then what waited
			}
			return true, nil
		}
	}
	return false, nil
}

// grokBind: a setup proof checked out, by the key the link was minted for,
// over the bot ID it recorded and a challenge Wakeshift sent through the
// webhook. The webhook is bound to the session; with rekey, the bot's key
// is replaced. The audit entry is recorded here, in the bind's
// transaction; a running foreground wait ends.
func (a *API) grokBind(ctx context.Context, tx pgx.Tx, tenant string, s sessions.Session, row botkeys.Row, su setupRow, appName string) (grokAnswer, error) {
	var ans grokAnswer
	if su.Used || su.Expired || su.Challenge != row.ID || len(su.Sealed) == 0 || su.Failures >= a.Grok.MaxAttempts || su.Bot != row.Bot || s.Conversation != su.Bot {
		ans.text, ans.logOutcome = harnesses.GrokSetupFailedNote, "setup_closed"
		return ans, nil
	}
	if _, err := streams.LinkLive(ctx, tx, su.Link); err != nil {
		if errors.Is(err, streams.ErrLinkInvalid) {
			ans.logOutcome = "access_ended"
			return ans, errGrokRefused
		}
		return ans, err
	}
	// One of the user's binds today (BindsPerUser). The save checked it
	// too, but saves on several links can pass that check before any of
	// them binds: here the count is taken under its row's lock, so the
	// cap holds. Past it, the attempt fails (the page says why) and
	// nothing is bound; it doesn't count as one of the link's failures.
	if a.Grok.RateLimits {
		user, err := grokBindSubject(ctx, tx, tenant, su.Grant)
		if err != nil {
			return ans, err
		}
		if ok, _, err := a.Grok.Counts.Take(ctx, tx, a.Grok.Limits.BindsPerUser, user, a.Grok.Now()); err != nil {
			return ans, err
		} else if !ok {
			if _, err := tx.Exec(ctx, `UPDATE agent_webhook_setups SET sealed = NULL, challenge_id = NULL, proof_due = NULL, last_error = 'too_many_webhooks'
				WHERE id = $1`, su.ID); err != nil {
				return ans, err
			}
			ans.text, ans.logOutcome = harnesses.GrokSetupFailedNote, "bind_limit"
			return ans, nil
		}
	}
	hook, err := agenthooks.OpenCursor(a.HookSeal, agenthooks.SetupBinding(tenant, su.ID), su.Sealed)
	if err != nil {
		slog.Error("grok webhooks: a setup's sealed webhook can't be opened", "reason", "unreadable")
		ans.text, ans.logOutcome = harnesses.GrokSetupFailedNote, "unreadable"
		return ans, nil
	}
	old, hadOld, err := agenthooks.Get(ctx, tx, s.ID)
	if err != nil {
		return ans, err
	}
	pub := ed25519.PublicKey(su.Public)
	_, hasKey, err := botkeys.GetKey(ctx, tx, s.ID)
	if err != nil {
		return ans, err
	}
	// A re-setup of the session's webhook by the same key carries over to
	// the other destinations on that key (the proof was by it). A bind by
	// another key (a rekey) carries nothing by key: the old key's other
	// destinations are other apps', and a proof by a new key, from this
	// session, says nothing about them, so they stay as they are. Those on
	// this webhook's routine follow it by the routine (carryRoutine), whose
	// own run proved the new key.
	resetup := hadOld && old.Kind == agenthooks.KindCursor && bytes.Equal(old.Fingerprint, su.Fingerprint)
	if replaced, err := botkeys.PutKey(ctx, tx, s.ID, su.Bot, su.App, pub); err != nil {
		return ans, err
	} else if replaced != nil {
		if err := audit.Record(ctx, tx, audit.Entry{Actor: "user", Action: audit.BotKeyReplaced, AgentID: s.Agent, AppID: su.App, GrantID: su.Grant,
			Detail: map[string]any{"session": s.Handle, "old": botkeys.Short(replaced), "new": botkeys.Short(su.Fingerprint)}}); err != nil {
			return ans, err
		}
	} else if !hasKey {
		if err := audit.Record(ctx, tx, audit.Entry{Actor: "agent", Action: audit.BotKeyRegistered, AgentID: s.Agent, AppID: su.App, GrantID: su.Grant,
			Detail: map[string]any{"session": s.Handle, "fingerprint": botkeys.Short(su.Fingerprint)}}); err != nil {
			return ans, err
		}
	}
	binding := agenthooks.BindingID(agenthooks.Setup{Tenant: tenant, ID: su.ID})
	// One routine, one binding: this bind becomes the routine's, under its
	// row's lock, and grokPropagate carries it to every other destination
	// on the routine, in any app.
	routine := a.Grok.Links.Routine(su.Bot, hook.URL)
	if err := agenthooks.BindRoutine(ctx, tx, routine, binding, su.Fingerprint); err != nil {
		return ans, err
	}
	replaced, err := agenthooks.RegisterCursor(ctx, tx, a.HookSeal, tenant, agenthooks.Destination{Session: s.ID, Agent: s.Agent, App: su.App, Grant: su.Grant,
		Bot: su.Bot, Fingerprint: su.Fingerprint, Routine: routine}, hook, binding, false, false)
	if err != nil {
		return ans, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_webhook_setups SET used_at = now(), sealed = NULL, claim_id = NULL, claimed_until = NULL, proof_due = NULL, last_error = NULL WHERE id = $1`, su.ID); err != nil {
		return ans, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{Actor: "user", Action: audit.AgentWebhookRegistered, AgentID: s.Agent, AppID: su.App, GrantID: su.Grant,
		Detail: map[string]any{"session": s.Handle, "kind": agenthooks.KindCursor, "replaced": replaced, "via": "setup_page", "confirmed": "bot_key"}}); err != nil {
		return ans, err
	}
	// A foreground wait still running would race the webhook.
	if err := streams.EndWaitForWebhook(ctx, tx, s.Agent, s.ID); err != nil {
		return ans, err
	}
	var peers []agenthooks.Peer
	if resetup {
		ps, err := agenthooks.PeersByKey(ctx, tx, su.Fingerprint)
		if err != nil {
			return ans, err
		}
		for _, p := range ps {
			if p.Tenant != tenant || p.Session != s.ID {
				peers = append(peers, p)
			}
		}
	}
	ans.text, ans.logOutcome = harnesses.GrokBoundNote(appName), "bound"
	ans.proofMs = time.Since(row.CreatedAt).Milliseconds()
	ans.bound = &boundFollowUp{tenant: tenant, session: s.ID, agent: s.Agent, bot: su.Bot, hook: hook, pub: pub, binding: binding, routine: routine, peers: peers}
	return ans, nil
}

// grokPropagate carries a bind over to the bot's other workspaces on the
// same webhook: the user set up the bot's webhook again, so each gets its
// new URL and key, each in its own workspace, audited there. By key
// (b.peers), only live destinations on the bind's own key follow, and
// their key doesn't change; by routine, every destination on the
// webhook's routine follows, with the key the routine's run proved.
func (a *API) grokPropagate(ctx context.Context, b boundFollowUp) {
	bg := context.WithoutCancel(ctx)
	seen := map[agenthooks.Peer]bool{{Tenant: b.tenant, Session: b.session}: true}
	// The live destinations on the bind's key (a webhook moved to a new
	// routine takes them along), then every destination on the routine
	// still on another binding, round after round until none is left: a
	// join that read the old binding before this bind commits lands on the
	// routine and is found by the next round.
	for _, p := range b.peers {
		if seen[p] {
			continue
		}
		seen[p] = true
		if goOn, _ := a.carryOver(bg, b, p, carryByKey); !goOn {
			return
		}
	}
	a.carryRoutine(bg, b)
}

// carryRoutine carries b's binding to every destination on its routine
// still on another one, round after round until none is left or a newer
// bind takes the routine, and returns how many it carried it to.
func (a *API) carryRoutine(ctx context.Context, b boundFollowUp) int {
	n := 0
	for range 10 {
		stale, err := agenthooks.StaleOnRoutine(ctx, a.DB.Pool, b.routine, b.binding)
		if err != nil {
			slog.Warn("grok webhooks: finding a routine's other destinations failed", "err", err)
			return n
		}
		if len(stale) == 0 {
			return n
		}
		for _, p := range stale {
			goOn, carried := a.carryOver(ctx, b, p, carryByURL)
			if carried {
				n++
			}
			if !goOn {
				return n
			}
		}
	}
	slog.Warn("grok webhooks: a routine's destinations kept changing; carrying over stopped")
	return n
}

// FinishGrokCarryOvers is the expiry pass's part for routines: a bind
// whose carry-over didn't finish (the core stopped after the bind
// committed) left destinations on the routine with another binding than
// bot_routines'. For each such routine it reads the binding from a
// destination that holds it (in that workspace, with the routine's row
// locked for share) and carries it over as the bind would have, under the
// same rule: each destination in its own workspace's transaction, audited
// there, only while the binding is still the routine's. Rewriting a
// destination that already has it is skipped, so passes that overlap a
// bind's own carry-over (or each other) do nothing twice.
func (a *API) FinishGrokCarryOvers(ctx context.Context) (int, error) {
	if !a.grokOn() {
		return 0, nil
	}
	type pending struct{ Routine, Binding []byte }
	rows, _ := a.DB.Pool.Query(ctx, `SELECT DISTINCT w.routine, r.removal_hash FROM bot_webhooks w JOIN bot_routines r ON r.routine = w.routine
		WHERE w.removal_hash <> r.removal_hash LIMIT 100`)
	todo, err := pgx.CollectRows(rows, pgx.RowToStructByPos[pending])
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range todo {
		var src agenthooks.Peer
		err := a.DB.Pool.QueryRow(ctx, `SELECT tenant_id::text, session_id::text FROM bot_webhooks WHERE routine = $1 AND removal_hash = $2 ORDER BY tenant_id, session_id LIMIT 1`,
			t.Routine, t.Binding).Scan(&src.Tenant, &src.Session)
		if errors.Is(err, pgx.ErrNoRows) {
			// No destination holds the binding any more (removed meanwhile):
			// nothing to carry; the routine's next bind settles it.
			continue
		}
		if err != nil {
			return n, err
		}
		b := boundFollowUp{tenant: src.Tenant, session: src.Session, binding: t.Binding, routine: t.Routine}
		ok := false
		err = a.DB.InTenant(ctx, src.Tenant, func(tx pgx.Tx) error {
			if cur, has, err := agenthooks.RoutineBinding(ctx, tx, t.Routine); err != nil || !has || !bytes.Equal(cur, t.Binding) {
				return err
			}
			d, has, err := agenthooks.Get(ctx, tx, src.Session)
			if err != nil || !has || d.Kind != agenthooks.KindCursor {
				return err
			}
			key, has, err := botkeys.GetKey(ctx, tx, src.Session)
			if err != nil || !has || !bytes.Equal(key.Fingerprint, d.Fingerprint) {
				return err
			}
			hook, err := agenthooks.OpenDestination(ctx, tx, a.HookSeal, src.Tenant, src.Session)
			if err != nil {
				slog.Warn("grok webhooks: a routine's binding can't be opened", "reason", "unreadable")
				return nil
			}
			b.agent, b.bot, b.hook, b.pub, ok = d.Agent, d.Bot, hook, ed25519.PublicKey(key.Public), true
			return nil
		})
		if err != nil {
			return n, err
		}
		if ok {
			n += a.carryRoutine(ctx, b)
		}
	}
	return n, nil
}

// carryMode is how a carry-over found the destination it may rewrite,
// which decides which destinations it may reach (carryOver).
type carryMode int

const (
	// carryByURL: on the bind's routine (its webhook URL), whose own run
	// proved the bind's key, so any destination there follows, new key
	// included.
	carryByURL carryMode = iota
	// carryByKey: by the bind's key only, so only a live destination
	// already on that same key follows, and its key never changes.
	carryByKey
)

// carryOver gives one destination the bind's webhook, key and binding
// (BindingID), in its own workspace, audited there, while the bind is still the
// routine's (its row locked for share: a newer bind waits, and then
// carries over itself). It reports whether to go on (false once a newer
// bind took the routine), and whether it carried the binding over (not
// when the destination already had it).
//
// carryByKey: p was found by the bind's key, not on its routine. Then only a
// live destination whose key (the session's and the destination's) is
// the bind's own follows, and its key doesn't change: a proof by one key
// never gives another app's destination a new key, and a destination the
// user stopped (dead: its routine deleted) isn't revived by another app's
// setup.
func (a *API) carryOver(ctx context.Context, b boundFollowUp, p agenthooks.Peer, mode carryMode) (goOn, carried bool) {
	fp := botkeys.Fingerprint(b.pub)
	var agent string
	superseded := false
	err := a.DB.InTenant(ctx, p.Tenant, func(tx pgx.Tx) error {
		if cur, ok, err := agenthooks.RoutineBinding(ctx, tx, b.routine); err != nil {
			return err
		} else if !ok || !bytes.Equal(cur, b.binding) {
			superseded = true
			return nil
		}
		// The destination's index row, locked: another carry-over of the
		// same binding (the bind's own, or an expiry pass) waits, then finds
		// it done.
		var now []byte
		if err := tx.QueryRow(ctx, `SELECT removal_hash FROM bot_webhooks WHERE tenant_id = current_tenant() AND session_id = $1 FOR UPDATE`, p.Session).Scan(&now); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if bytes.Equal(now, b.binding) {
			return nil
		}
		d, has, err := agenthooks.Get(ctx, tx, p.Session)
		if err != nil || !has || d.Kind != agenthooks.KindCursor || d.Bot != b.bot {
			return err
		}
		if mode == carryByKey {
			if d.State != agenthooks.Live || !bytes.Equal(d.Fingerprint, fp) {
				return nil
			}
			key, has, err := botkeys.GetKey(ctx, tx, p.Session)
			if err != nil || !has || !bytes.Equal(key.Fingerprint, fp) {
				return err
			}
		}
		s, err := sessions.Get(ctx, tx, p.Session)
		if err != nil {
			return err
		}
		if replaced, err := botkeys.PutKey(ctx, tx, p.Session, b.bot, d.App, b.pub); err != nil {
			return err
		} else if replaced != nil {
			if err := audit.Record(ctx, tx, audit.Entry{Actor: "user", Action: audit.BotKeyReplaced, AgentID: d.Agent, AppID: d.App, GrantID: d.Grant,
				Detail: map[string]any{"session": s.Handle, "old": botkeys.Short(replaced), "new": botkeys.Short(fp)}}); err != nil {
				return err
			}
		}
		if _, err := agenthooks.RegisterCursor(ctx, tx, a.HookSeal, p.Tenant, agenthooks.Destination{Session: p.Session, Agent: d.Agent, App: d.App, Grant: d.Grant,
			Bot: b.bot, Fingerprint: fp, Routine: b.routine}, b.hook, b.binding, d.Joined, false); err != nil {
			return err
		}
		agent = d.Agent
		return audit.Record(ctx, tx, audit.Entry{Actor: "user", Action: audit.AgentWebhookRegistered, AgentID: d.Agent, AppID: d.App, GrantID: d.Grant,
			Detail: map[string]any{"session": s.Handle, "kind": agenthooks.KindCursor, "replaced": true, "via": "setup_page", "confirmed": "bot_key", "joined": d.Joined}})
	})
	if err != nil {
		slog.Warn("grok webhooks: carrying a setup over to another workspace failed", "err", err)
		return true, false
	}
	if superseded {
		slog.Info("grok webhooks: carrying over stopped", "reason", "superseded")
		return false, false
	}
	if agent != "" && a.HookWake != nil {
		a.HookWake(p.Tenant, agent)
	}
	return true, agent != ""
}

// The setup page's routes. Every refusal of a token is the same 401.
const grokInvalidLink = "This link no longer works: it was used, it expired, or it isn't valid."

func invalidLink(w http.ResponseWriter) {
	httpx.WriteError(w, http.StatusUnauthorized, "invalid_link", grokInvalidLink)
}

// grokTokenAllowed: addr hasn't used up its failed checks; otherwise it
// answers as for an invalid link, byte for byte, without checking the
// token.
func (a *API) grokTokenAllowed(w http.ResponseWriter, r *http.Request, addr string) bool {
	blocked, err := a.grokBlocked(r.Context(), addr)
	if err != nil {
		a.respond(w, nil, err)
		return false
	}
	if blocked {
		slog.Info("grok webhooks: setup link refused", "reason", "too_many_failures")
		invalidLink(w)
		return false
	}
	return true
}

// grokDecode reads a setup route's JSON body (at most grokMaxBody).
func (a *API) grokDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !a.grokOn() {
		grokOff(w)
		return false
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, grokMaxBody))
	if err != nil || json.Unmarshal(b, v) != nil {
		httpx.WriteError(w, http.StatusBadRequest, "bad_request", "the body must be JSON, at most 4 KB")
		return false
	}
	return true
}

// expireProof clears a setup attempt whose proof didn't come in time
// (locked row): its sealed URL and key go, and it counts a failure.
func expireProof(ctx context.Context, tx pgx.Tx, su *setupRow) error {
	if len(su.Sealed) == 0 || su.Awaiting || su.Used {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_webhook_setups SET sealed = NULL, challenge_id = NULL, proof_due = NULL, failures = failures + 1, last_error = 'unconfirmed'
		WHERE id = $1`, su.ID); err != nil {
		return err
	}
	if su.Challenge != "" {
		if err := botkeys.Use(ctx, tx, su.Challenge); err != nil {
			return err
		}
	}
	su.Sealed, su.Challenge, su.Failures, su.LastError = nil, "", su.Failures+1, "unconfirmed"
	return nil
}

// Setup states, as describe answers them.
const (
	setupOpen     = "open"
	setupAwaiting = "awaiting_proof"
	setupOn       = "on"
)

// grokDescribe is POST /v1/stream/webhook-setup/describe {token}: the
// app's name, the end of the bot's ID, the link's expiry and the setup's
// state. Describing doesn't use the link up.
func (a *API) grokDescribe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !a.grokDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	addr := a.grokAddr(r)
	if a.Grok.RateLimits {
		if ok, retry, err := a.Grok.Counts.Take(ctx, a.DB.Pool, a.Grok.Limits.DescribePerAddress, addr, a.Grok.Now()); err != nil {
			a.respond(w, nil, err)
			return
		} else if !ok {
			slog.Info("grok webhooks: describe refused", "reason", "rate_limited")
			grokRateLimited(w, retry, "rate_limited", "Too many requests from here. Try again in a few minutes.")
			return
		}
	}
	if !a.grokTokenAllowed(w, r, addr) {
		return
	}
	ref, err := a.Grok.Links.ParseSetup(in.Token)
	if err != nil {
		slog.Info("grok webhooks: setup link refused", "reason", "bad_token")
		a.grokCheckFailed(ctx, addr)
		invalidLink(w)
		return
	}
	out := map[string]any{}
	refused := false
	err = a.DB.InTenant(ctx, ref.Tenant, func(tx pgx.Tx) error {
		su, err := scanSetup(tx.QueryRow(ctx, `SELECT `+setupColumns+` FROM agent_webhook_setups WHERE id = $1 FOR UPDATE`, ref.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			return errGrokRefused
		}
		if err != nil {
			return err
		}
		if err := expireProof(ctx, tx, &su); err != nil {
			return err
		}
		// A refusal from here on still commits the expired attempt.
		err = a.describeTx(ctx, tx, ref, su, out)
		if errors.Is(err, errGrokRefused) {
			refused = true
			return nil
		}
		return err
	})
	if refused {
		err = errGrokRefused
	}
	switch {
	case errors.Is(err, errGrokRefused):
		slog.Info("grok webhooks: setup link refused", "reason", "closed")
		a.grokCheckFailed(ctx, addr)
		invalidLink(w)
	case err != nil:
		a.respond(w, nil, err)
	default:
		httpx.WriteJSON(w, http.StatusOK, out)
	}
}

// describeTx is describe's answer for a locked setup.
func (a *API) describeTx(ctx context.Context, tx pgx.Tx, ref agenthooks.Setup, su setupRow, out map[string]any) error {
	{
		state := setupOpen
		switch {
		case su.Used:
			// On while the session's webhook destination is live: whichever
			// bind (this one, a later re-setup, or a revision before binding
			// IDs) made it. An unwatch ends it; a dead one (the routine
			// deleted, its key rotated) or one needing setup isn't on.
			var on bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_webhooks WHERE session_id = $1 AND kind = 'cursor-automations'
				AND state = 'live' AND needs_setup_at IS NULL)`,
				su.Session).Scan(&on); err != nil {
				return err
			}
			if !on {
				return errGrokRefused
			}
			state = setupOn
		case su.Expired || su.Failures >= a.Grok.MaxAttempts:
			return errGrokRefused
		case len(su.Sealed) > 0:
			state = setupAwaiting
		}
		if !su.Used {
			if _, err := streams.LinkLive(ctx, tx, su.Link); errors.Is(err, streams.ErrLinkInvalid) {
				return errGrokRefused
			} else if err != nil {
				return err
			}
		}
		out["app"] = appNameTx(ctx, tx, su.App)
		out["bot_tail"] = tail(su.Bot, 6)
		// The key the link binds, as the bot's connector prints it, so the
		// user sees which key they're confirming.
		out["key"] = botkeys.Short(su.Fingerprint)
		out["expires_at"] = su.ExpiresAt.UTC().Format(time.RFC3339)
		out["state"] = state
		if su.LastError != "" && state == setupOpen {
			out["error"] = su.LastError
		}
		return nil
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// grokSetupSave is POST /v1/stream/webhook-setup {token, url, key}: the
// user's paste. Wakeshift POSTs a fresh setup challenge through the
// webhook: in a short transaction it claims the setup and keeps the
// URL and key sealed on it, with the challenge; outside any transaction it
// POSTs; then it records the outcome. The bind happens when the run's
// proof comes (grokProof). Nothing is bound before.
func (a *API) grokSetupSave(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
		URL   string `json:"url"`
		Key   string `json:"key"`
	}
	if !a.grokDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	addr := a.grokAddr(r)
	if !a.grokTokenAllowed(w, r, addr) {
		return
	}
	ref, err := a.Grok.Links.ParseSetup(in.Token)
	if err != nil {
		slog.Info("grok webhooks: setup link refused", "reason", "bad_token")
		a.grokCheckFailed(ctx, addr)
		invalidLink(w)
		return
	}
	hook := agenthooks.CursorHook{URL: strings.TrimSpace(in.URL), Key: strings.TrimSpace(in.Key)}
	if agenthooks.CheckCursorURL(hook.URL) != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_url", agenthooks.ErrBadCursorURL.Error())
		return
	}
	if agenthooks.CheckCursorKey(hook.Key) != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_key", agenthooks.ErrBadCursorKey.Error())
		return
	}
	claim := randomID()
	var challengeID, app, bot string
	var busy, refused bool
	var limited struct {
		retry         time.Duration
		code, message string
	}
	err = a.DB.InTenant(ctx, ref.Tenant, func(tx pgx.Tx) error {
		su, err := scanSetup(tx.QueryRow(ctx, `SELECT `+setupColumns+` FROM agent_webhook_setups WHERE id = $1 FOR UPDATE`, ref.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			return errGrokRefused
		}
		if err != nil {
			return err
		}
		if err := expireProof(ctx, tx, &su); err != nil {
			return err
		}
		if su.Used || su.Expired || su.Failures >= a.Grok.MaxAttempts {
			refused = true // still commits the expired attempt
			return nil
		}
		if _, err := streams.LinkLive(ctx, tx, su.Link); errors.Is(err, streams.ErrLinkInvalid) {
			return errGrokRefused
		} else if err != nil {
			return err
		}
		if su.Awaiting || su.Claimed {
			busy = true
			return nil
		}
		// One check at a time per bot, in any workspace: the routine's run
		// fetches the bot's open setup challenge by its ID, so it must be
		// the one this save's POST carries. Saves for one bot take turns
		// here (a lock on the bot's ID, until this commits), so two can't
		// both find none open.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('wakeshift-grok-bot:' || $1, 0))`, su.Bot); err != nil {
			return err
		}
		if others, err := botkeys.OpenFor(ctx, tx, su.Bot, botkeys.Setup, "", 1); err != nil {
			return err
		} else if len(others) > 0 {
			busy = true
			return nil
		}
		if _, err := sessions.Get(ctx, tx, su.Session); err != nil {
			if errors.Is(err, sessions.ErrNotFound) {
				return errGrokRefused
			}
			return err
		}
		// The limits on saves that POST to Cursor (GrokLimits), counted here, once
		// the link is known good and free, so a refused or busy save doesn't
		// use them. A refusal commits: it still counts, and so does an
		// expired attempt above.
		if a.Grok.RateLimits {
			lim, now := a.Grok.Limits, a.Grok.Now()
			user, err := grokBindSubject(ctx, tx, ref.Tenant, su.Grant)
			if err != nil {
				return err
			}
			if over, retry, err := a.Grok.Counts.Over(ctx, tx, lim.BindsPerUser, user, now); err != nil {
				return err
			} else if over {
				limited.retry, limited.code, limited.message = retry, "too_many_webhooks", "You've set up as many webhooks as you can today. Try again tomorrow."
				return nil
			}
			if ok, retry, err := a.Grok.Counts.Take(ctx, tx, lim.SavePerLink, ref.ID, now); err != nil {
				return err
			} else if !ok {
				limited.retry, limited.code, limited.message = retry, "too_many_tries", "This link has been tried too often. Try again later."
				return nil
			}
			if ok, retry, err := a.Grok.Counts.Take(ctx, tx, lim.SavePerAddress, addr, now); err != nil {
				return err
			} else if !ok {
				limited.retry, limited.code, limited.message = retry, "rate_limited", "Too many tries from here. Try again later."
				return nil
			}
		}
		_, row, err := a.Grok.Proofs.Issue(ctx, tx, ref.Tenant, botkeys.Setup, su.Session, su.Bot, su.App, su.Grant, su.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_webhook_setups SET claim_id = $2, claimed_until = now() + interval '30 seconds', sealed = $3,
				challenge_id = $4, proof_due = $5, last_error = NULL WHERE id = $1`,
			su.ID, claim, agenthooks.SealCursor(a.HookSeal, agenthooks.SetupBinding(ref.Tenant, su.ID), hook), row.ID, row.ExpiresAt); err != nil {
			return err
		}
		challengeID, app, bot = row.ID, appNameTx(ctx, tx, su.App), su.Bot
		return nil
	})
	if refused && err == nil {
		err = errGrokRefused
	}
	switch {
	case errors.Is(err, errGrokRefused):
		slog.Info("grok webhooks: setup link refused", "reason", "closed")
		a.grokCheckFailed(ctx, addr)
		invalidLink(w)
		return
	case err != nil:
		a.respond(w, nil, err)
		return
	case limited.code != "":
		slog.Info("grok webhooks: save refused", "reason", limited.code)
		grokRateLimited(w, limited.retry, limited.code, limited.message)
		return
	case busy:
		httpx.WriteError(w, http.StatusConflict, "awaiting_proof", "Wakeshift already sent your bot a check through a webhook; wait for it to answer.")
		return
	}
	// Within the claim's 30 seconds, whatever the client's own timeouts, so
	// no later save can claim the setup while this POST is out.
	postCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	sent, perr := agenthooks.PostCursor(postCtx, a.Grok.Client, hook, "", agenthooks.ChallengeBody(app, bot))
	cancel()
	answer := agenthooks.JudgeCursor(sent, perr)
	failure := ""
	switch answer {
	case agenthooks.CursorAccepted:
	case agenthooks.CursorUnauthorized:
		failure = "unauthorized"
	case agenthooks.CursorNotFound:
		failure = "not_found"
	default:
		failure = "unanswered"
	}
	ended := false
	err = a.DB.InTenant(context.WithoutCancel(ctx), ref.Tenant, func(tx pgx.Tx) error {
		bg := context.WithoutCancel(ctx)
		var used bool
		var failures int
		// Settled only while this claim and its challenge are still the
		// setup's: a proof that bound it, or a newer attempt, is left alone.
		if err := tx.QueryRow(bg, `SELECT used_at IS NOT NULL, failures FROM agent_webhook_setups WHERE id = $1 AND claim_id = $2
				AND (challenge_id = $3 OR used_at IS NOT NULL) FOR UPDATE`, ref.ID, claim, challengeID).Scan(&used, &failures); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // already settled
			}
			return err
		}
		if used || failure == "" {
			_, err := tx.Exec(bg, `UPDATE agent_webhook_setups SET claim_id = NULL, claimed_until = NULL WHERE id = $1`, ref.ID)
			if used {
				failure = ""
			}
			return err
		}
		ended = failures+1 >= a.Grok.MaxAttempts
		if _, err := tx.Exec(bg, `UPDATE agent_webhook_setups SET claim_id = NULL, claimed_until = NULL, sealed = NULL, challenge_id = NULL, proof_due = NULL,
				failures = failures + 1, last_error = $2 WHERE id = $1`, ref.ID, failure); err != nil {
			return err
		}
		return tx.QueryRow(bg, `UPDATE bot_challenges SET used_at = now() WHERE id = $1 AND used_at IS NULL RETURNING true`, challengeID).Scan(new(bool))
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		a.respond(w, nil, err)
		return
	}
	slog.Info("grok webhooks: setup challenge sent", "outcome", cmpOr(failure, "accepted"), "status", sent.Status, "why", agenthooks.Category(sent.Status, perr), "app", app)
	switch {
	case failure == "":
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"awaiting_proof": true})
	case ended:
		invalidLink(w)
	case failure == "unauthorized":
		httpx.WriteError(w, http.StatusBadRequest, "webhook_unauthorized", "The webhook didn't accept this key.")
	case failure == "not_found":
		httpx.WriteError(w, http.StatusBadRequest, "webhook_not_found", "This webhook doesn't exist.")
	default:
		httpx.WriteError(w, http.StatusBadGateway, "webhook_unanswered", "The webhook didn't answer.")
	}
}

// ExpireGrokSetups is the expiry pass's part: a setup attempt whose proof
// didn't come in time loses its sealed URL and key and counts a failure;
// setups and challenges go an hour after they're used or expire.
func ExpireGrokSetups(ctx context.Context, d interface {
	InTenant(context.Context, string, func(pgx.Tx) error) error
}, pool interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (int, error) {
	// Every workspace on the list, 1,000 at a time in order, so one with
	// setups still open never keeps a later one from its turn.
	var tenants []string
	after := ""
	for {
		rows, _ := pool.Query(ctx, `SELECT tenant_id::text FROM bot_setup_targets WHERE tenant_id::text > $1 ORDER BY tenant_id::text LIMIT 1000`, after)
		page, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return 0, err
		}
		tenants = append(tenants, page...)
		if len(page) < 1000 {
			break
		}
		after = page[len(page)-1]
	}
	n := 0
	for _, t := range tenants {
		err := d.InTenant(ctx, t, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE agent_webhook_setups SET sealed = NULL, challenge_id = NULL, proof_due = NULL, failures = failures + 1, last_error = 'unconfirmed'
				WHERE sealed IS NOT NULL AND proof_due <= now() AND used_at IS NULL AND (claimed_until IS NULL OR claimed_until < now())`)
			if err != nil {
				return err
			}
			n += int(tag.RowsAffected())
			if _, err := tx.Exec(ctx, `DELETE FROM agent_webhook_setups WHERE used_at < now() - interval '1 hour' OR expires_at < now() - interval '1 hour'`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM bot_challenges WHERE used_at < now() - interval '1 hour' OR expires_at < now() - interval '1 hour'`); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `DELETE FROM bot_setup_targets t WHERE t.tenant_id = current_tenant()
				AND NOT EXISTS (SELECT 1 FROM agent_webhook_setups) AND NOT EXISTS (SELECT 1 FROM bot_challenges)`)
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// grokAddr is the request's client address, as the other services'
// limits per address count it (agentkit.ClientAddr).
// With the limits off it's empty: no client address is read or stored.
func (a *API) grokAddr(r *http.Request) string {
	if !a.Grok.RateLimits {
		return ""
	}
	return agentkit.ClientAddr(r, a.Grok.ProxyHops)
}

// grokBlocked: addr has failed FailuresPerAddress token or challenge
// checks in the window. Its tokens and challenges are then refused, valid
// or not, before they're checked, with the answer an invalid one gets.
func (a *API) grokBlocked(ctx context.Context, addr string) (bool, error) {
	if !a.Grok.RateLimits {
		return false, nil
	}
	over, _, err := a.Grok.Counts.Over(ctx, a.DB.Pool, a.Grok.Limits.FailuresPerAddress, addr, a.Grok.Now())
	return over, err
}

// grokCheckFailed counts a failed token or challenge check by addr.
func (a *API) grokCheckFailed(ctx context.Context, addr string) {
	if !a.Grok.RateLimits {
		return
	}
	if _, _, err := a.Grok.Counts.Add(context.WithoutCancel(ctx), a.DB.Pool, a.Grok.Limits.FailuresPerAddress, addr, a.Grok.Now()); err != nil {
		slog.Warn("grok webhooks: counting a failed check failed", "err", err)
	}
}

// grokProofFailed counts a failed proof (a signature that doesn't verify
// on a real challenge) across Wakeshift. The proof that reaches
// FailedProofs in its window (exactly one, across instances) logs the
// alert (the hosted deployment's grok_failed_proofs), naming the proof's
// app, and nothing else. It never refuses anything.
func (a *API) grokProofFailed(ctx context.Context, app string) {
	ctx = context.WithoutCancel(ctx)
	l := a.Grok.Limits.FailedProofs
	n, _, err := a.Grok.Counts.Add(ctx, a.DB.Pool, l, "all", a.Grok.Now())
	if err != nil {
		slog.Warn("grok webhooks: counting a failed proof failed", "err", err)
		return
	}
	if n != l.Max {
		return
	}
	name := ""
	if app != "" {
		if err := a.DB.Pool.QueryRow(ctx, `SELECT name FROM apps WHERE id = $1`, app).Scan(&name); err != nil || name == "" {
			name = app
		}
	}
	slog.Warn("grok webhooks: failed proofs over the alert threshold", "count", n, "window", l.Window.String(), "app", name)
}

// grokRateLimited answers 429 with Retry-After.
func grokRateLimited(w http.ResponseWriter, retry time.Duration, code, message string) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retry/time.Second)))
	httpx.WriteError(w, http.StatusTooManyRequests, code, message)
}

// grokBindSubject is who a webhook bound through a setup counts against
// for BindsPerUser: the user who granted the app access, or, in a
// workspace an app owns (no such user; its one person, the app's user),
// the workspace.
func grokBindSubject(ctx context.Context, tx pgx.Tx, tenant, grant string) (string, error) {
	if grant != "" {
		var user string
		err := tx.QueryRow(ctx, `SELECT coalesce(granted_by::text, '') FROM grants WHERE id = $1`, grant).Scan(&user)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		if user != "" {
			return "user:" + user, nil
		}
	}
	return "workspace:" + tenant, nil
}
