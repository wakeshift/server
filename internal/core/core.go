// Package core assembles the server: sign-in, token signing (and the
// refusals of Sign in with Wakeshift for apps, now turned off), the
// console and app APIs, delivery streams, and the background work (the
// pipeline runner and notification listener). cmd/wakeshift and tests build
// it the same way.
package core

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/trace"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/adminapi"
	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/auth"
	"github.com/wakeshift/server/internal/botkeys"
	"github.com/wakeshift/server/internal/config"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/console"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dblimit"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/downloads"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/iap"
	"github.com/wakeshift/server/internal/mail"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/internal/notify"
	"github.com/wakeshift/server/internal/oauth"
	"github.com/wakeshift/server/internal/orgs"
	"github.com/wakeshift/server/internal/pipeline"
	"github.com/wakeshift/server/internal/server"
	"github.com/wakeshift/server/internal/streams"
	"github.com/wakeshift/server/internal/subscriptions"
	"github.com/wakeshift/server/internal/telemetry"
	"github.com/wakeshift/server/internal/waittask"
)

// Options configure a Core.
type Options struct {
	DB        *db.DB
	PublicURL string
	// SiteURL is the public site's (server.Server.SiteURL).
	SiteURL string
	// Key derives the key for a purpose from the secret key.
	Key            func(purpose string) []byte
	Apps           []apps.Config
	AllowedSignups string
	DevSignIn      bool
	// DevSignInToken is what dev sign-in must present (auth.Console).
	DevSignInToken string
	// TermsURL, TermsVersion and PrivacyURL are the Terms new accounts
	// accept and the Privacy Policy beside them (empty: none); SupportEmail
	// is where app owners write (config.Config's).
	TermsURL, TermsVersion, PrivacyURL, SupportEmail string
	Google                                           *auth.Google
	GitHub                                           *auth.GitHub
	InvitesPerUser                                   int
	// Mail sends organization invites and the waitlist's invitations (nil:
	// none).
	Mail mail.Sender
	Web  fs.FS
	// Poll is how often background work checks without a notification
	// (default 2s; tests shorten it).
	Poll time.Duration
	// ConnectorDir holds the connector for Codex, built per platform.
	ConnectorDir string
	// MaxStreams caps the streams and long polls this instance holds (0:
	// no cap; streams.Server.MaxStreams).
	MaxStreams int
	// Connectors are the oldest connector versions served (none: every
	// version; streams.Server.Connectors).
	Connectors connectorversion.Minimums
	// StreamRenew is how often a stream renews and re-checks (default
	// 30s; streams.Server.Renew; tests shorten it).
	StreamRenew time.Duration
	// StreamMaxAge is how long a stream stays open before it's closed to
	// reconnect (default 50m; streams.Server.MaxAge; tests shorten it).
	StreamMaxAge time.Duration
	// IdleFor is how long a conversation without a user may go
	// unused before the expiry pass forgets it (default appapi.IdleFor;
	// tests lengthen it).
	IdleFor time.Duration
	// The internal admin API (internal/adminapi): the admin service's
	// account and IAP audience; AdminEmails may act through it. Any unset:
	// off.
	AdminServiceAccount string
	AdminIAPAudience    string
	// AdminVerifier verifies the admin service's ID tokens (default:
	// Google's keys; tests replace it).
	AdminVerifier adminapi.IDTokenVerifier
	// AdminIAPKeys verify the forwarded IAP assertions (default: IAP's
	// keys; tests replace them).
	AdminIAPKeys oidc.KeySet
	// AdminEmails are the people who may act through the internal admin
	// API (exact emails).
	AdminEmails []string
	// Tracing traces requests (telemetry.Start); nil: they aren't.
	Tracing trace.TracerProvider
	// SubscribeVouchers turns subscribe vouchers on
	// (appapi.API.SubscribeVouchers).
	SubscribeVouchers bool
	// RedeemByToolCall: appapi.API.RedeemByToolCall.
	RedeemByToolCall bool
	// InlineSessionSteps: appapi.API.InlineSessionSteps.
	InlineSessionSteps bool
	// CodexElicit: appapi.API.CodexElicit.
	CodexElicit config.Allowlist
	// MCPEventsPush serves MCP Events push (appapi.API.Push;
	// WAKESHIFT_MCP_EVENTS_PUSH).
	MCPEventsPush bool
	// GrokWebhooks turns Grok Bot's webhook wake on (appapi.API.Grok;
	// WAKESHIFT_GROK_WEBHOOKS); GrokLinkTTL, GrokPacing and GrokLimits set
	// it (zero: the defaults; GrokLimits only in tests).
	GrokWebhooks bool
	GrokLinkTTL  time.Duration
	GrokPacing   agenthooks.CursorPacing
	GrokLimits   appapi.GrokLimits
	// GrokRateLimits turns the setup and proof routes' request limits on
	// (WAKESHIFT_GROK_RATE_LIMITS; off by default).
	GrokRateLimits bool
	// ProxyHops is how many trusted proxies are in front, each appending to
	// X-Forwarded-For, for limits per client address (agentkit.ClientAddr;
	// WAKESHIFT_PROXY_HOPS).
	ProxyHops int
}

// Core is an assembled server.
type Core struct {
	Handler  http.Handler
	Streams  *streams.Server
	Apps     *apps.Registry
	OAuth    *oauth.Server
	SignIn   *auth.Console
	db       *db.DB
	hub      *notify.Hub
	poll     time.Duration
	webhooks *mcpevents.Deliverer
	// agentHooks: agent-hosted webhook destinations (Capy's watches).
	agentHooks *agenthooks.Deliverer
	// idleFor: Options.IdleFor.
	idleFor time.Duration
	// appAPI: the app API, for the expiry pass's parts that need it.
	appAPI *appapi.API
}

// New assembles a Core and registers the configured apps.
func New(ctx context.Context, o Options) (*Core, error) {
	signIn := &auth.Console{
		Key: o.Key("console-session"), PublicURL: o.PublicURL, DevSignIn: o.DevSignIn, DevSignInToken: o.DevSignInToken, Google: o.Google, GitHub: o.GitHub,
		SiteURL: o.SiteURL, TermsURL: o.TermsURL, PrivacyURL: o.PrivacyURL, SupportEmail: o.SupportEmail,
		Accounts: &accounts.Store{Pool: o.DB.Pool, Allowed: accounts.AllowList(o.AllowedSignups), InvitesPerUser: o.InvitesPerUser, TermsVersion: o.TermsVersion},
	}
	hub := notify.New(o.DB.Pool, deliveries.NotifyChannel, streams.TakeoverChannel, streams.RevokedChannel, streams.PushEndedChannel)
	// Streams waiting for deliveries check again whenever the listener
	// reconnects (it may have missed some).
	hub.Resync = []string{deliveries.NotifyChannel}
	deliver := &streams.Server{DB: o.DB, Key: o.Key("stream-link"), PublicURL: o.PublicURL, Notify: hub, Poll: o.Poll, MaxStreams: o.MaxStreams, Connectors: o.Connectors, Renew: o.StreamRenew, MaxAge: o.StreamMaxAge,
		CursorWebhooks: o.GrokWebhooks}
	registry := &apps.Registry{Pool: o.DB.Pool, Issuer: o.PublicURL}
	if err := registry.Sync(ctx, o.Apps); err != nil {
		return nil, err
	}
	if err := registry.LogAmbiguousTopics(ctx); err != nil {
		slog.Warn("apps: checking stored manifests' topics failed", "err", err)
	}
	signInWith, err := oauth.New(o.DB, o.PublicURL, o.Key("oauth-signing-keys"), registry)
	if err != nil {
		return nil, err
	}
	signInWith.SupportEmail = o.SupportEmail
	if err := signInWith.LoadKeys(ctx); err != nil {
		return nil, fmt.Errorf("loading signing keys: %w", err)
	}
	dl := &downloads.Server{PublicURL: o.PublicURL, ConnectorDir: o.ConnectorDir}
	webhookSecrets, err := mcpevents.NewSealer(o.Key("mcp-events-webhook-secrets"))
	if err != nil {
		return nil, err
	}
	publishSecrets, err := mcpevents.NewSealer(o.Key("publish-url-signing-secrets"))
	if err != nil {
		return nil, err
	}
	// Agent-hosted webhooks' URLs (Capy's watches) are credentials: sealed
	// with a key of their own.
	hookSeal, err := mcpevents.NewSealer(o.Key("agent-webhook-urls"))
	if err != nil {
		return nil, err
	}
	hooks := &agenthooks.Deliverer{DB: o.DB, Seal: hookSeal, Poll: o.Poll}
	var grok *appapi.GrokHooks
	if o.GrokWebhooks {
		pacing := o.GrokPacing
		hooks.Cursor = &pacing
		counts, err := dblimit.New(o.Key("rate-counts"))
		if err != nil {
			return nil, err
		}
		grok = &appapi.GrokHooks{Links: agenthooks.NewSetupLinks(o.Key("agent-webhook-setup")), Proofs: botkeys.NewCodec(o.Key("bot-proof")),
			LinkTTL: o.GrokLinkTTL, Counts: counts, ProxyHops: o.ProxyHops, Limits: o.GrokLimits, RateLimits: o.GrokRateLimits}
	}
	var callLimits *dblimit.Counter
	if o.RedeemByToolCall {
		if callLimits, err = dblimit.New(o.Key("rate-counts")); err != nil {
			return nil, err
		}
	}
	verifyLimits, err := dblimit.New(o.Key("rate-counts"))
	if err != nil {
		return nil, err
	}
	waitTasks, err := waittask.New(o.Key("wait-tasks"))
	if err != nil {
		return nil, err
	}
	appAPI := &appapi.API{DB: o.DB, Apps: registry, SupportEmail: o.SupportEmail, Streams: deliver, Issuer: o.PublicURL, SiteURL: o.SiteURL, Keys: signInWith.PublicKey, Downloads: dl, Sign: signInWith.Sign,
		Events: webhookSecrets, PublishSecrets: publishSecrets, Tasks: waitTasks,
		Hooks: agenthooks.NewLinks(o.Key("agent-webhook-register")), HookSeal: hookSeal, HookWake: hooks.Wake, SubscribeVouchers: o.SubscribeVouchers, RedeemByToolCall: o.RedeemByToolCall,
		InlineSessionSteps: o.InlineSessionSteps,
		CodexElicit:        o.CodexElicit,
		Push:               o.MCPEventsPush, PushCursorKey: o.Key("mcp-events-push-cursors"),
		CallLimits: callLimits, CallProxyHops: o.ProxyHops, VerifyLimits: verifyLimits, Grok: grok}
	deliver.Ending = appAPI.StreamEnding // why a session's stream ends, for its agent
	routes := map[string]http.Handler{
		"GET /v1/stream":      deliver,
		"GET /v1/stream/poll": http.HandlerFunc(deliver.ServePoll), // the same stream, long-polled
		"POST /v1/stream/ack": http.HandlerFunc(deliver.ServeAck),
		"GET /v1/stream/wait": http.HandlerFunc(deliver.ServeWait), // one-shot: returns when something arrives
	}
	for p, h := range dl.Routes() {
		routes[p] = h
	}
	admin, err := adminAPI(ctx, o, registry, appAPI)
	if err != nil {
		return nil, err
	}
	srv := &server.Server{
		DB:      o.DB,
		Web:     o.Web,
		HSTS:    strings.HasPrefix(o.PublicURL, "https://"),
		SiteURL: o.SiteURL,
		APIs: []server.Registrar{
			signIn, signInWith,
			&console.API{DB: o.DB, Auth: signIn, SupportEmail: o.SupportEmail, Streams: deliver, Apps: registry, AppAPI: appAPI, Mail: o.Mail},
			appAPI,
			admin,
		},
		Routes: routes,
	}
	return &Core{appAPI: appAPI, Handler: telemetry.Handler(o.Tracing, srv.Handler()), Streams: deliver, Apps: registry, OAuth: signInWith, SignIn: signIn,
		db: o.DB, hub: hub, poll: o.Poll, webhooks: &mcpevents.Deliverer{DB: o.DB, Seal: webhookSecrets, Poll: o.Poll}, agentHooks: hooks,
		idleFor: cmp.Or(o.IdleFor, appapi.IdleFor)}, nil
}

// adminAPI is the internal admin API, off unless it's configured.
func adminAPI(ctx context.Context, o Options, registry *apps.Registry, appAPI *appapi.API) (*adminapi.API, error) {
	a := &adminapi.API{DB: o.DB, Apps: registry, AppAPI: appAPI, Mail: o.Mail, ConsoleURL: o.PublicURL}
	if o.AdminServiceAccount == "" || o.AdminIAPAudience == "" || len(o.AdminEmails) == 0 {
		return a, nil
	}
	keys := o.AdminIAPKeys
	if keys == nil {
		keys = oidc.NewRemoteKeySet(ctx, iap.KeysURL)
	}
	v, err := iap.NewWithKeys(o.AdminIAPAudience, o.AdminEmails, keys)
	if err != nil {
		return nil, fmt.Errorf("the internal admin API: %w", err)
	}
	a.IAP, a.ServiceAccount, a.Service = v, o.AdminServiceAccount, o.AdminVerifier
	if a.Service == nil {
		a.Service = adminapi.NewGoogleVerifier(ctx, adminapi.Audience(o.PublicURL))
	}
	return a, nil
}

// Run does the background work until ctx ends, then waits for the pipeline
// runner's current step to commit or roll back.
func (c *Core) Run(ctx context.Context) {
	go c.hub.Run(ctx)
	go c.expireGrants(ctx)
	go c.webhooks.Run(ctx)   // MCP Events deliveries (ChatGPT, house agents)
	go c.agentHooks.Run(ctx) // agent-hosted webhooks (Capy)
	(&pipeline.Runner{DB: c.db, Poll: c.poll, Delivered: func(tenant, agent string) {
		c.webhooks.Wake(tenant, agent)
		c.agentHooks.Wake(tenant, agent)
	}, Finished: appapi.FinishedOnce}).Run(ctx)
}

// expireGrants revokes expired grants (and what they created) every few
// minutes, so a consent's end date is when it actually stops; and ends
// topic subscriptions whose ttl ran out, and deletes long-expired connector
// tickets and stream links.
func (c *Core) expireGrants(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		// One instance at a time: it's a pass over every tenant.
		if _, err := c.db.Exclusive(ctx, expireLock, func() { c.expireAll(ctx) }); err != nil && ctx.Err() == nil {
			slog.Warn("grants: expiring", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ExpireNow runs one expiry pass now (one instance at a time, as the
// ticker's), for tests.
func (c *Core) ExpireNow(ctx context.Context) error {
	_, err := c.db.Exclusive(ctx, expireLock, func() { c.expireAll(ctx) })
	return err
}

// expireLock is the advisory lock key for expireGrants' pass.
const expireLock = 0x77616b6502 // "wake" 2

// expireAll is one pass of expireGrants over every tenant.
func (c *Core) expireAll(ctx context.Context) {
	// Publishes whose copies into some workspace failed part-way (the app
	// may never retry them).
	// One that fails doesn't stop the rest: both are logged. (In a block of
	// its own, so later passes can declare their own n and err.)
	{
		n, err := appapi.FinishFanouts(ctx, c.db)
		if err != nil && ctx.Err() == nil {
			slog.Warn("topics: finishing interrupted publishes failed (retried on the next pass)", "err", err)
		}
		if n > 0 {
			slog.Info("topics: finished interrupted publishes", "count", n)
		}
	}
	// Request counts (internal/dblimit) whose window is over.
	if n, err := dblimit.Prune(ctx, c.db.Pool, time.Now()); err != nil && ctx.Err() == nil {
		slog.Warn("limits: deleting old request counts failed (retried on the next pass)", "err", err)
	} else if n > 0 {
		slog.Info("limits: old request counts deleted", "count", n)
	}
	// Publishes that asked for their content to go once delivered, and that
	// nobody got within their day.
	if n, err := appapi.ExpirePublishContent(ctx, c.db); err != nil && ctx.Err() == nil {
		slog.Warn("topics: deleting undelivered publishes' content failed (retried on the next pass)", "err", err)
	} else if n > 0 {
		slog.Info("topics: undelivered publishes' content deleted", "count", n)
	}
	// Publishes over a day old, of apps that stopped publishing (an app's
	// next publish deletes its own).
	if n, err := appapi.ExpireOldPublishes(ctx, c.db, 10000); err != nil && ctx.Err() == nil {
		slog.Warn("topics: deleting old publishes failed (retried on the next pass)", "err", err)
	} else if n > 0 {
		slog.Info("topics: old publishes deleted", "count", n)
	}
	// Deleted apps: their users' data, then (30 days on) their logs.
	if p, err := appapi.FinishAppDeletions(ctx, c.db, appWorkspacesPerPass, appSweepPerPass); err != nil && ctx.Err() == nil {
		slog.Warn("apps: deleting deleted apps' users' data failed (retried on the next pass)", "err", err)
	} else if p != (appapi.AppDeletionPass{}) {
		slog.Info("apps: deleted apps' data deleted", "apps_finished", p.Finished, "workspaces", p.Workspaces, "logs", p.Logs)
	}
	// App assertions' IDs, once the assertions have expired.
	if _, err := apps.DeleteExpiredAssertionIDs(ctx, c.db.Pool, 100000); err != nil && ctx.Err() == nil {
		slog.Warn("apps: deleting expired assertion IDs failed (retried on the next pass)", "err", err)
	}
	// Organizations' ended invites, and deleted organizations' logs, past
	// their retention.
	if inv, logs, err := orgs.Prune(ctx, c.db.Pool, 10000); err != nil && ctx.Err() == nil {
		slog.Warn("orgs: deleting old invites and deleted organizations' logs failed (retried on the next pass)", "err", err)
	} else if inv+logs > 0 {
		slog.Info("orgs: old invites and deleted organizations' logs deleted", "invites", inv, "log_entries", logs)
	}
	// Waitlist entries past their retention: signed up 30 days ago, or
	// invited 90 days ago and never used.
	if n, err := accounts.PruneWaitlist(ctx, c.db.Pool, 10000); err != nil && ctx.Err() == nil {
		slog.Warn("accounts: deleting old waitlist entries failed (retried on the next pass)", "err", err)
	} else if n > 0 {
		slog.Info("accounts: old waitlist entries deleted", "count", n)
	}
	// Keys held on the waitlist for @wakeshift/cli, a day on.
	if n, err := apps.PrunePendingKeys(ctx, c.db.Pool, 10000); err != nil && ctx.Err() == nil {
		slog.Warn("apps: deleting expired pending keys failed (retried on the next pass)", "err", err)
	} else if n > 0 {
		slog.Info("apps: expired pending keys deleted", "count", n)
	}
	// Subscribe vouchers past their use: unused an hour after expiring, used
	// an hour after their subscription stopped being live.
	if n, err := appapi.PruneSubscribeVouchers(ctx, c.db, 10000); err != nil && ctx.Err() == nil {
		slog.Warn("vouchers: deleting spent vouchers failed (retried on the next pass)", "err", err)
	} else if n > 0 {
		slog.Info("vouchers: spent vouchers deleted", "count", n)
	}
	// Redeem by call: claimed calls and the call index, an hour past their expiry.
	if n, err := appapi.PruneCallClaims(ctx, c.db, 10000); err != nil && ctx.Err() == nil {
		slog.Warn("appapi: deleting expired call claims failed (retried on the next pass)", "err", err)
	} else if n > 0 {
		slog.Info("appapi: expired call claims deleted", "count", n)
	}
	// Grok Bot's webhook setups: attempts whose proof didn't come in time,
	// and setups and challenges an hour past their use or expiry.
	if n, err := appapi.ExpireGrokSetups(ctx, c.db, c.db.Pool); err != nil && ctx.Err() == nil {
		slog.Warn("grok webhooks: expiring setups failed (retried on the next pass)", "err", err)
	} else if n > 0 {
		slog.Info("grok webhooks: unconfirmed setup attempts expired", "count", n)
	}
	// Grok Bot routines whose carry-over didn't finish (a core stopped
	// after a bind committed): carried over now.
	if c.appAPI != nil {
		if n, err := c.appAPI.FinishGrokCarryOvers(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("grok webhooks: finishing carry-overs failed (retried on the next pass)", "err", err)
		} else if n > 0 {
			slog.Info("grok webhooks: interrupted carry-overs finished", "count", n)
		}
	}
	// Grok Bot routines no destination has named for an hour.
	if n, err := agenthooks.PruneRoutines(ctx, c.db.Pool); err != nil && ctx.Err() == nil {
		slog.Warn("grok webhooks: deleting unused routines failed (retried on the next pass)", "err", err)
	} else if n > 0 {
		slog.Info("grok webhooks: unused routines deleted", "count", n)
	}
	// Idle conversations without a user gone for good:
	// their workspaces, before the per-workspace jobs. One that fails
	// doesn't stop the rest: both are logged.
	n, err := appapi.ExpireIdle(ctx, c.db, c.idleFor, idlePerPass)
	if err != nil && ctx.Err() == nil {
		slog.Warn("idle: forgetting idle workspaces failed (retried on the next pass)", "err", err)
	}
	if n > 0 {
		slog.Info("idle: forgot idle workspaces", "count", n)
	}
	c.expireTenants(ctx)
}

// expireTenants is the expiry pass's part in each workspace: the jobs in
// one transaction per workspace (expireTenant), in workspace ID order,
// from where the last pass stopped (expiry_state).
func (c *Core) expireTenants(ctx context.Context) {
	st, err := loadExpiryState(ctx, c.db)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("expiry pass: reading where the last pass stopped failed", "err", err)
		}
		return
	}
	if st.resumeAfter == "" {
		// A new pass: the daily usage rollup (usage_daily) at most once an
		// hour, as it's for the admin service's daily numbers, not the
		// minute; and every ended subscription's waiting deliveries checked
		// once a day (appapi.CancelStranded), only recent ones otherwise.
		// Kept for the whole pass, so a resumed pass does what it started.
		st.rollingUp = st.rolledUpAt.IsZero() || time.Since(st.rolledUpAt) >= usageEvery
		st.strandedFull = st.strandedFullAt.IsZero() || time.Since(st.strandedFullAt) >= strandedFullEvery
		st.rollupFailed, st.strandedFailed = false, false
		if st.rollingUp {
			if _, err := deliveries.PruneUsage(ctx, c.db.Pool, time.Now(), 100000); err != nil && ctx.Err() == nil {
				slog.Warn("usage: pruning old days failed (retried on the next pass)", "err", err)
				st.rollupFailed = true
			}
		}
		if _, err := c.db.Pool.Exec(ctx, `UPDATE expiry_state SET rolling_up = $1, stranded_full = $2, rollup_failed = $3, stranded_failed = false`,
			st.rollingUp, st.strandedFull, st.rollupFailed); err != nil {
			if ctx.Err() == nil {
				slog.Warn("expiry pass: starting a pass failed", "err", err)
			}
			return
		}
	} else {
		slog.Info("expiry pass: resuming an interrupted pass", "after", st.resumeAfter)
	}
	tenants, err := c.db.TenantsAfter(ctx, st.resumeAfter)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("grants: listing tenants to expire grants failed", "err", err)
		}
		return
	}
	jobs := expiryJobs
	if st.rollingUp {
		jobs = append(append([]expiryJob(nil), jobs...), usageJob)
	}
	p := expiryPass{strandedAll: st.strandedFull}
	saved := time.Now()
	for _, tenant := range tenants {
		if ctx.Err() != nil {
			return
		}
		// Where the pass stands is saved every few seconds, in the
		// workspace's own transaction (no transaction of its own): an
		// interrupted pass repeats at most those few seconds' workspaces,
		// and every job is safe to repeat.
		save := time.Since(saved) >= expirySaveEvery
		if c.expireTenant(ctx, tenant, jobs, p, &st, save) {
			saved = time.Now()
		}
	}
	if ctx.Err() != nil {
		return
	}
	// Done: the next pass starts from the first workspace. A periodic job
	// that failed somewhere is run again on the next pass, not after its
	// interval.
	if _, err := c.db.Pool.Exec(ctx, `UPDATE expiry_state SET resume_after = NULL,
		rolled_up_at = CASE WHEN rolling_up AND NOT (rollup_failed OR $1) THEN now() ELSE rolled_up_at END,
		stranded_full_at = CASE WHEN stranded_full AND NOT (stranded_failed OR $2) THEN now() ELSE stranded_full_at END,
		rolling_up = false, stranded_full = false, rollup_failed = false, stranded_failed = false`,
		st.rollupFailed, st.strandedFailed); err != nil && ctx.Err() == nil {
		slog.Warn("expiry pass: recording a finished pass failed", "err", err)
	}
}

// expirySaveEvery is how often the expiry pass saves where it stands (a
// variable for tests).
var expirySaveEvery = 2 * time.Second

// strandedFullEvery is how often the expiry pass checks every ended
// subscription's waiting deliveries (appapi.CancelStranded), not only
// those of subscriptions ended in the last appapi.StrandedRecent.
const strandedFullEvery = 24 * time.Hour

// expiryState is the expiry_state row (161_expiry_state.sql).
type expiryState struct {
	resumeAfter                  string
	rollingUp, rollupFailed      bool
	rolledUpAt                   time.Time
	strandedFull, strandedFailed bool
	strandedFullAt               time.Time
}

func loadExpiryState(ctx context.Context, d *db.DB) (expiryState, error) {
	var st expiryState
	var rolled, stranded *time.Time
	err := d.Pool.QueryRow(ctx, `SELECT coalesce(resume_after::text, ''), rolling_up, rollup_failed, rolled_up_at,
		stranded_full, stranded_failed, stranded_full_at FROM expiry_state`).
		Scan(&st.resumeAfter, &st.rollingUp, &st.rollupFailed, &rolled, &st.strandedFull, &st.strandedFailed, &stranded)
	if rolled != nil {
		st.rolledUpAt = *rolled
	}
	if stranded != nil {
		st.strandedFullAt = *stranded
	}
	return st, err
}

// expireTenant runs the jobs in one workspace in one transaction (one
// BEGIN, one set of bounds, one COMMIT, not one of each per job), each job
// in a savepoint of its own, grants first: a job that fails rolls back only
// its own work, and is retried on the next pass. With save, the
// transaction also records the workspace as where the pass stands.
//
// If the transaction itself fails (its transaction_timeout, a lost
// connection), each job runs again in a transaction of its own, as it used
// to, so a workspace whose jobs together outrun the bound still gets its
// revocations committed. A periodic job that failed is marked in st (and
// saved with the workspace, so a resumed pass doesn't forget it). It
// reports whether the workspace was saved as where the pass stands.
func (c *Core) expireTenant(ctx context.Context, tenant string, jobs []expiryJob, p expiryPass, st *expiryState, save bool) (saved bool) {
	// failed marks a periodic job that failed in st.
	failed := func(st *expiryState, name string) {
		switch {
		case name == usageJob.name:
			st.rollupFailed = true
		case name == strandedJobName && p.strandedAll:
			st.strandedFailed = true
		}
	}
	saveSQL := `UPDATE expiry_state SET resume_after = $1::uuid, rollup_failed = rollup_failed OR $2, stranded_failed = stranded_failed OR $3`
	type result struct {
		job expiryJob
		n   int64
		err error
	}
	var results []result
	err := c.db.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		results = results[:0]
		if err := setJobTimeouts(ctx, tx); err != nil {
			return err
		}
		for _, job := range jobs {
			if err := ctx.Err(); err != nil {
				return err
			}
			sp, err := tx.Begin(ctx) // a savepoint
			if err != nil {
				return err
			}
			n, err := job.run(ctx, sp, p)
			if err != nil {
				if rbErr := sp.Rollback(ctx); rbErr != nil {
					return err // the transaction itself failed
				}
				results = append(results, result{job: job, err: err})
				continue
			}
			if err := sp.Commit(ctx); err != nil {
				return err
			}
			results = append(results, result{job: job, n: n})
		}
		if save {
			marked := *st
			for _, r := range results {
				if r.err != nil {
					failed(&marked, r.job.name)
				}
			}
			if _, err := tx.Exec(ctx, saveSQL, tenant, marked.rollupFailed, marked.strandedFailed); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		slog.Warn("expiry pass: the workspace's transaction failed; running each job on its own", "tenant", tenant, "err", err)
		for _, job := range jobs {
			if ctx.Err() != nil {
				return false
			}
			if err := c.expireJobAlone(ctx, tenant, job, p); err != nil {
				failed(st, job.name)
			}
		}
		// The work committed job by job, so the pass moves on past this
		// workspace (rare, so a statement of its own is no cost).
		if save {
			if _, err := c.db.Pool.Exec(ctx, saveSQL, tenant, st.rollupFailed, st.strandedFailed); err == nil {
				saved = true
			}
		}
		return saved
	}
	for _, r := range results {
		if r.err != nil {
			failed(st, r.job.name)
		}
		logJob(tenant, r.job, r.n, r.err)
	}
	return save
}

// logJob logs what a job did in a workspace, once it committed, or that
// it failed.
func logJob(tenant string, job expiryJob, n int64, err error) {
	if err != nil {
		slog.Warn("expiry pass: "+job.name+" failed (retried on the next pass)", "tenant", tenant, "err", err)
	} else if n > 0 && job.done != "" {
		slog.Info(job.done, "tenant", tenant, "count", n)
	}
}

// expireJobAlone runs one job in one workspace in a transaction of its
// own (expireTenant's fallback).
func (c *Core) expireJobAlone(ctx context.Context, tenant string, job expiryJob, p expiryPass) error {
	var n int64
	err := c.db.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		if err := setJobTimeouts(ctx, tx); err != nil {
			return err
		}
		n, err = job.run(ctx, tx, p)
		return err
	})
	if ctx.Err() == nil {
		logJob(tenant, job, n, err)
	}
	return err
}

// setJobTimeouts bounds the expiry pass's transaction in a workspace. Each
// job is bounded (batches, indexes), and this makes sure of it: a job that
// plans badly fails and is retried on the next pass, instead of holding its
// locks, and the oldest snapshot vacuum must keep, for hours (as
// CancelStranded once did).
func setJobTimeouts(ctx context.Context, tx pgx.Tx) error {
	for _, set := range jobTimeouts {
		if _, err := tx.Exec(ctx, set); err != nil {
			return err
		}
	}
	return nil
}

// The expiry pass's bounds in one workspace: any one statement (a job
// past it fails alone, in its savepoint), and the whole transaction (a job
// of many short statements, catching up a subscription far behind, is
// bounded too: past it, the jobs run again one transaction each, each
// with this bound). Whatever fails is retried on the next pass (every 5
// minutes), its locks and its snapshot released.
const (
	jobStatementTimeout   = 30 * time.Second
	jobTransactionTimeout = 2 * time.Minute
)

var jobTimeouts = []string{
	fmt.Sprintf(`SET LOCAL statement_timeout = %d`, jobStatementTimeout.Milliseconds()),
	fmt.Sprintf(`SET LOCAL transaction_timeout = %d`, jobTransactionTimeout.Milliseconds()),
}

// usageEvery is how often the expiry pass rolls up each workspace's
// finished deliveries into usage_daily.
const usageEvery = time.Hour

// usageJob writes the workspace's deliveries finished yesterday and today
// to usage_daily (deliveries.RollUpUsage), with absolute counts, so a
// repeated or failed pass changes nothing.
var usageJob = expiryJob{"rolling up usage", "", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
	return deliveries.RollUpUsage(ctx, tx, time.Now())
}}

// expiryJob is one of the expiry pass's jobs, run in each workspace in a
// savepoint of its own (expireTenant); n is how many things it changed
// (logged as done, unless that is empty).
type expiryJob struct {
	name, done string
	run        func(ctx context.Context, tx pgx.Tx, p expiryPass) (n int64, err error)
}

// expiryPass is what one pass does beyond every pass's work.
type expiryPass struct {
	// strandedAll: CancelStranded checks every ended subscription, not only
	// recently ended ones (strandedFullEvery).
	strandedAll bool
}

// strandedJobName is the CancelStranded job's name.
const strandedJobName = "cancelling stranded webhook deliveries"

var expiryJobs = []expiryJob{
	{"expiring grants", "grants: expired", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		n, err := grants.RevokeExpired(ctx, tx)
		return int64(n), err
	}},
	{"expiring topic subscriptions", "topics: subscriptions expired", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		n, err := appapi.ExpireTopics(ctx, tx)
		return int64(n), err
	}},
	{strandedJobName, "mcp events: deliveries no webhook will send cancelled", func(ctx context.Context, tx pgx.Tx, p expiryPass) (int64, error) {
		return appapi.CancelStranded(ctx, tx, pruneBatch, p.strandedAll)
	}},
	// Only counted (a log metric, wakeshift_codex_stranded): the core can't
	// start a connector on the user's machine.
	{"counting stranded Codex threads", appapi.StrandedLog, func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		return appapi.StrandedCodex(ctx, tx, appapi.StrandedAfter)
	}},
	{"pruning pending additions", "", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		return appapi.PrunePendingAdditions(ctx, tx, pruneBatch)
	}},
	{"cleaning up", "", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		tickets, links, err := appapi.CleanUp(ctx, tx, pruneBatch)
		return tickets + links, err
	}},
	{"pruning ended subscriptions' cursors", "", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		return subscriptions.PruneEndedCursors(ctx, tx, pruneBatch)
	}},
	{"deleting delivered content", "events' content deleted after delivery", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		return deliveries.RedactDelivered(ctx, tx, pruneBatch)
	}},
	{"pruning", "deliveries and events past retention deleted", func(ctx context.Context, tx pgx.Tx, _ expiryPass) (int64, error) {
		nd, ne, err := deliveries.Prune(ctx, tx, deliveries.Retention, pruneBatch)
		return nd + ne, err
	}},
}

// idlePerPass is how many idle workspaces a pass forgets at most, keeping
// it short: 100 every 5 minutes is far more than are made.
const idlePerPass = 100

// appWorkspacesPerPass is how many of deleted apps' own workspaces a pass
// erases at most: 500 every 5 minutes erases an app with 100,000 users in
// under a day, far inside the 30 days promised.
const appWorkspacesPerPass = 500

// appSweepPerPass is how many other workspaces a pass visits at most to
// delete what deleted apps made there: 5,000 every 5 minutes visits 1.4
// million a day.
const appSweepPerPass = 5000

// pruneBatch is how many deliveries (and events) a pass deletes per
// workspace at most, keeping each transaction short: 5,000 every 5 minutes
// is far more than a workspace finishes. The cleanup and pending-addition
// jobs delete in batches of this size (appapi.CleanUp).
const pruneBatch = 5000
