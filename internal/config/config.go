// Package config reads the server's configuration from the environment.
package config

import (
	"cmp"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wakeshift/server/internal/accounts"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/npmtag"
)

// Config is the server's configuration.
//
// Environment:
//
//	DATABASE_URL          Postgres connection string (required). The role must
//	                      not be a superuser or bypass row-level security, or
//	                      be a member of a role that is or does.
//	WAKESHIFT_MIGRATE_DATABASE_URL
//	                      optional: the role that owns the tables and runs
//	                      migrations. When set, DATABASE_URL's role is only
//	                      granted reads and writes.
//	WAKESHIFT_ADDR        listen address (default :$PORT, else :8080)
//	WAKESHIFT_PUBLIC_URL  externally reachable base URL, e.g. https://wakeshift.dev
//	                      (required: links, cookies and sign-in depend on it)
//	WAKESHIFT_SITE_URL    the public site's URL, where the console host
//	                      redirects /llms.txt, /llms-full.txt and /AGENTS.md
//	                      (default: WAKESHIFT_PUBLIC_URL without "app.", so
//	                      https://app.wakeshift.ai's is https://wakeshift.ai;
//	                      none for a console not at app.<site>: those paths
//	                      are 404s). The console's Docs links go to
//	                      $WAKESHIFT_SITE_URL/docs, or to Wakeshift's own
//	                      docs (https://wakeshift.ai/docs) without one.
//	WAKESHIFT_TERMS_URL, WAKESHIFT_TERMS_VERSION
//	                      the Terms of Service people accept by signing up:
//	                      the sign-in and join pages say signing in agrees to
//	                      them, and each new account records the version
//	                      (any label, such as the Terms' date) and when.
//	                      Set both or neither. Neither: no Terms, no line on
//	                      the sign-in page and no version recorded, except
//	                      on Wakeshift's hosted service (WAKESHIFT_PUBLIC_URL
//	                      exactly https://app.wakeshift.ai or
//	                      https://app.staging.wakeshift.ai), whose defaults
//	                      are its own site's /terms and /privacy
//	                      (wakeshift.ai's, staging.wakeshift.ai's) and
//	                      accounts.TermsVersion.
//	WAKESHIFT_PRIVACY_URL  the Privacy Policy linked next to the Terms (needs
//	                      WAKESHIFT_TERMS_URL)
//	WAKESHIFT_SUPPORT_EMAIL  where app owners are told to write about a
//	                      suspension or a limit, in API errors and the
//	                      console (default support@wakeshift.ai)
//	WAKESHIFT_LOG_FORMAT  "json" (default) or "text"
//	WAKESHIFT_OTLP_LOG_LEVEL
//	                      the lowest level of log line exported to OTLP,
//	                      when OTEL_EXPORTER_OTLP_ENDPOINT is set
//	                      (internal/telemetry): debug, info (default), warn
//	                      or error. stderr gets every line from info up
//	                      regardless. OTEL_LOGS_EXPORTER=none exports none.
//	WAKESHIFT_SECRET_KEY  at least 32 random bytes (required). Keys for
//	                      each purpose (console sessions, stream links) are
//	                      derived from it; changing it signs everyone out
//	                      and invalidates stream links. The Makefile's
//	                      published key (DevSecretKey) is refused unless
//	                      WAKESHIFT_PUBLIC_URL is localhost and WAKESHIFT_ADDR
//	                      is on loopback.
//	WAKESHIFT_GOOGLE_CLIENT_ID, WAKESHIFT_GOOGLE_CLIENT_SECRET
//	                      an OAuth client for "Sign in with Google"; its
//	                      redirect URI is $WAKESHIFT_PUBLIC_URL/auth/google/callback
//	WAKESHIFT_GITHUB_CLIENT_ID, WAKESHIFT_GITHUB_CLIENT_SECRET
//	                      an OAuth app for "Sign in with GitHub"; its
//	                      callback URL is $WAKESHIFT_PUBLIC_URL/auth/github/callback
//	WAKESHIFT_ALLOWED_SIGNUPS  comma-separated emails and @domains who may
//	                      create an account whatever the sign-up cap (existing
//	                      users can always sign in). On a self-hosted server
//	                      (not Wakeshift's hosted service, by
//	                      WAKESHIFT_PUBLIC_URL as for the Terms) only these
//	                      make accounts, unless WAKESHIFT_OPEN_SIGNUPS=1
//	WAKESHIFT_OPEN_SIGNUPS  "1" opens a self-hosted server's sign-up as the
//	                      hosted service's is: anyone while there are fewer
//	                      accounts than the cap (signup_cap), invite codes
//	                      and organization invites past it, and the allow
//	                      list always. Unset, sign-up is closed: only the
//	                      allow list (and people the operator invites off
//	                      the waitlist) get accounts; everyone else waits
//	                      on the waitlist
//	WAKESHIFT_INVITES_PER_USER  how many people each user can invite (default 5)
//	WAKESHIFT_APPS        JSON list of apps to register: [{"id", "name", "mcp_url"
//	                      (optional), "public_key" (base64 Ed25519)}]: our own,
//	                      live apps (anyone can register a test app in the
//	                      console)
//	WAKESHIFT_CONNECTOR_DIR  the connector for Codex, built per platform
//	                      (served at /download/codex/*; optional,
//	                      set in the image; go build ./cmd/wakeshift-connector
//	                      builds one)
//	WAKESHIFT_INSTALLER_URL  where install commands fetch the /install
//	                      scripts instead of this server: a release of the
//	                      installers built for this Wakeshift, e.g.
//	                      https://github.com/wakeshift/connectors/releases/latest/download
//	                      (https; optional; signed installers)
//	WAKESHIFT_AGENT_PLUGINS  the marketplace plugin install offers name
//	                      (Codex's, Copilot's): an absolute path or
//	                      owner/repo; for a local core and the bench (default:
//	                      wakeshift/agent-plugins for production and staging,
//	                      Wakeshift's installer for any other)
//	WAKESHIFT_OPENCODE_PLUGIN  which version of Wakeshift's npm plugin for
//	                      opencode (@wakeshift/opencode) the install offers
//	                      give: a version (1.5.0), a dist-tag (staging) whose
//	                      version is looked up on npm every 15 minutes, or
//	                      "off" for Wakeshift's installer. The package must
//	                      trust this Wakeshift. Default: "latest" when
//	                      WAKESHIFT_PUBLIC_URL is https://app.wakeshift.ai
//	                      (the issuer production's packages trust), else off
//	WAKESHIFT_PI_PLUGIN   the same for Wakeshift's npm extension for pi
//	                      (@wakeshift/pi)
//	WAKESHIFT_OPENCLAW_PLUGIN  the same for Wakeshift's npm plugin for OpenClaw
//	                      (@wakeshift/openclaw). OpenClaw's offer sets the
//	                      plugin's issuer first for a Wakeshift other than
//	                      production, so production's packages work there.
//	WAKESHIFT_ANTIGRAVITY_PLUGIN  where Antigravity CLI's plugin offers install
//	                      from on any other Wakeshift: a checkout rendered for
//	                      it (an absolute path) or a GitHub URL; for a local
//	                      core and the bench (production and staging always
//	                      offer github.com/wakeshift/antigravity-plugin)
//	WAKESHIFT_CONNECT_PACKAGE  the same for @wakeshift/connect, which Cursor's
//	                      offers run (npx -y @wakeshift/connect@<version>
//	                      cursor) in place of Wakeshift's installer
//	WAKESHIFT_MAX_STREAMS  the most streams and long polls one instance holds
//	                      at once (default DefaultMaxStreams, 900; 0: no
//	                      cap); past it, new ones get 503 with Retry-After,
//	                      and one app's links take at most part of it
//	                      (streams.AppShare). Set it under the platform's
//	                      per-instance request limit (Cloud Run's concurrency)
//	                      and the process's open-file limit.
//	WAKESHIFT_DEV_SIGN_IN  "1" allows signing in with just an email and the
//	                      development sign-in token, only when
//	                      WAKESHIFT_PUBLIC_URL is a localhost URL and
//	                      WAKESHIFT_ADDR is on loopback (refused otherwise),
//	                      and only from a loopback connection that no proxy
//	                      forwarded (a request with any X-Forwarded-*
//	                      header, X-Real-IP or Forwarded is refused).
//	                      Logged at startup when on.
//	WAKESHIFT_DEV_SIGN_IN_TOKEN  the token dev sign-in must present (at
//	                      least 16 characters), for scripts that start a
//	                      server and sign in (the bench). Unset: a random
//	                      one is made at startup and printed once to stderr,
//	                      with a sign-in link that carries it.
//	WAKESHIFT_CONNECTOR_MIN_VERSIONS  the oldest connector versions served,
//	                      e.g. "wakeshift=2026.10.20,wakeshift:codex=2026.10.21.1200,
//	                      wakeshift-hermes=2026.10.20" (internal/connectorversion);
//	                      older ones get 426 update_required and their agents
//	                      are asked to have the user update. Default: none,
//	                      every version is served.
//	WAKESHIFT_ADMIN_DB_USER  the admin service's database login (on Cloud
//	                      SQL, its IAM user): granted the admin service's read
//	                      role, wakeshift_admin_read, at startup
//	WAKESHIFT_ADMIN_SERVICE_ACCOUNT, WAKESHIFT_ADMIN_IAP_AUDIENCE
//	                      the internal admin API (internal/adminapi): the admin
//	                      service's account (its ID tokens' email) and its IAP
//	                      backend's audience (/projects/<n>/global/backendServices/<id>).
//	                      With WAKESHIFT_ADMIN_EMAILS, who may act through it.
//	                      Any unset: the API is off (404).
//	WAKESHIFT_RESEND_KEY  the Resend API key for organization invite email, or
//	WAKESHIFT_RESEND_KEY_SECRET  a Secret Manager secret (projects/…/secrets/…)
//	                      holding it, read at runtime. Neither (or no version
//	                      yet): the console shows invite links to copy.
//	WAKESHIFT_MAIL_FROM   the From of invite email, on a domain verified in
//	                      Resend (default "Wakeshift <invites@wakeshift.ai>",
//	                      which only Wakeshift's own Resend account can send
//	                      from: any other server sending invites sets its own)
//	WAKESHIFT_ADMIN_EMAILS  comma-separated emails who may act through the
//	                      internal admin API, matched exactly (lowercase, as
//	                      IAP's assertions carry them)
//	WAKESHIFT_PROFILER    "1" sends CPU, heap and goroutine profiles to Cloud
//	                      Profiler (cmd/wakeshift), in the project the
//	                      instance runs in. Default off. Profiles hold stack
//	                      samples and the service, revision, zone and
//	                      instance labels, nothing from requests. Only a
//	                      build with the profiler tag sends them
//	                      (staging's); without it, this logs a warning.
//	WAKESHIFT_SUBSCRIBE_VOUCHERS  "1" hands native MCP Events clients
//	                      (ChatGPT) subscribe vouchers for tools-only events,
//	                      "0" doesn't. Unset:
//	                      on when WAKESHIFT_PUBLIC_URL is a localhost URL
//	                      (development), off otherwise. Wakeshift's hosted
//	                      service sets "1". Off, every request carrying a voucher is refused
//	                      and none is handed out.
//	WAKESHIFT_MCP_EVENTS_PUSH  "1" serves MCP Events push (events/stream):
//	                      POST
//	                      /api/apps/v1/events/stream answers, and the manifest
//	                      response reports the feature "push", so SDKs list
//	                      it; "0" doesn't (the route answers 501
//	                      unsupported). Unset: on when WAKESHIFT_PUBLIC_URL is
//	                      a localhost URL (development), off otherwise.
//	WAKESHIFT_GROK_WEBHOOKS  "1" wakes Grok Bot through its own Automations
//	                      webhook, set up once per bot, "0" doesn't: Grok Bot
//	                      waits in the foreground only, the setup and proof
//	                      routes answer 404, and the deliverer skips Cursor
//	                      destinations. Unset: on when WAKESHIFT_PUBLIC_URL
//	                      is a localhost URL (development), off otherwise.
//	WAKESHIFT_GROK_LINK_TTL  how long a webhook setup link works (default 24h)
//	WAKESHIFT_GROK_SPACING   the least time between two POSTs to one Grok
//	                      Bot's webhook (default 20s)
//	WAKESHIFT_GROK_POSTS_PER_HOUR, WAKESHIFT_GROK_POSTS_PER_DAY  caps on POSTs
//	                      to one webhook (default 60 and 500): each starts a
//	                      run on the user's usage
//	WAKESHIFT_GROK_BATCH_EVENTS, WAKESHIFT_GROK_BATCH_BYTES  the most events
//	                      and bytes in one POST (default 20 and 32768)
//	WAKESHIFT_GROK_RATE_LIMITS  "1" turns on the request limits on Grok
//	                      Bot's setup page's routes (per address, per link,
//	                      per user), "0" or unset
//	                      leaves them off until there's data. Off, no client
//	                      address is read, and WAKESHIFT_PROXY_HOPS isn't
//	                      needed for them. On, WAKESHIFT_PROXY_HOPS must be
//	                      set, to the right number: too few and every client
//	                      counts as a proxy's address, so one client's failed
//	                      checks lock everyone out; too many and clients pick
//	                      their own address with X-Forwarded-For, escaping
//	                      the limits.
//	WAKESHIFT_PROXY_HOPS  how many trusted proxies are in front, each
//	                      appending to X-Forwarded-For, for limits per
//	                      client address (behind Google's load balancer: 2;
//	                      default 0, the connection's address; required, 0
//	                      included, with WAKESHIFT_GROK_RATE_LIMITS on)
type Config struct {
	Addr        string
	DatabaseURL string
	// MigrateDatabaseURL, if set, owns the tables and migrates them.
	MigrateDatabaseURL string
	PublicURL          string
	// SiteURL is the public site's URL (empty: none known).
	SiteURL string
	// The Terms new accounts accept and the Privacy Policy beside them
	// (empty: none), and where app owners write.
	TermsURL, TermsVersion, PrivacyURL string
	SupportEmail                       string
	LogFormat                          string
	// OTLPLogLevel is the lowest level telemetry exports logs at.
	OTLPLogLevel slog.Level

	SecretKey []byte

	GoogleClientID     string
	GoogleClientSecret string
	GitHubClientID     string
	GitHubClientSecret string
	AllowedSignups     string
	// OpenSignups (WAKESHIFT_OPEN_SIGNUPS=1) opens a self-hosted server's
	// sign-up to the cap, invite codes and organization invites; without
	// it only the allow list makes accounts (SignupsClosed).
	OpenSignups    bool
	InvitesPerUser int
	// Organization invite email (Resend): the key or its secret, and From.
	ResendKey, ResendKeySecret, MailFrom string
	DevSignIn                            bool
	// DevSignInToken is what dev sign-in must present: from
	// WAKESHIFT_DEV_SIGN_IN_TOKEN, or made at startup (DevSignInTokenMade)
	// for the server to print once.
	DevSignInToken     string
	DevSignInTokenMade bool
	ConnectorDir       string
	InstallerURL       string
	AgentPlugins       string
	OpencodePlugin     string
	PiPlugin           string
	OpenclawPlugin     string
	ConnectPackage     string
	MaxStreams         int
	Apps               []apps.Config
	// AntigravityPlugin, if set, is where Antigravity CLI's plugin offers
	// install from on a Wakeshift other than production and staging
	// (harnesses.SetPluginRepo).
	AntigravityPlugin string
	// Connectors are the oldest connector versions served.
	Connectors connectorversion.Minimums
	// AdminDBUser is the admin service's database login, granted its read
	// role at startup.
	AdminDBUser string
	// The internal admin API: the admin service's account and IAP
	// audience (with AdminEmails).
	AdminServiceAccount string
	AdminIAPAudience    string
	// AdminEmails may act through the internal admin API.
	AdminEmails []string
	// Profiler sends profiles to Cloud Profiler.
	Profiler bool
	// SubscribeVouchers hands native clients subscribe vouchers.
	SubscribeVouchers bool
	// RedeemByToolCall lets Claude Code's hooks redeem a call's ticket by
	// the tool call's ID (off by default).
	RedeemByToolCall bool
	// InlineSessionSteps gives Claude Code's install offer the session-only
	// steps inline, as before, instead of a one-time URL fetched once the
	// user picks that option. Inline unless
	// WAKESHIFT_CLAUDE_SESSION_STEPS=deferred: an environment turns the
	// new offer on in its own configuration, so a release
	// never turns it on by itself.
	InlineSessionSteps bool
	// CodexElicit is which apps' Codex conversations may get the install
	// offer as a client request (an MCP elicitation form the app's SDK shows
	// in Codex's own dialog):
	// WAKESHIFT_CODEX_ELICIT, "off" (the default), "all", or a
	// comma-separated list of app IDs. An environment turns it on
	// in its own configuration, so a release never does by itself.
	CodexElicit Allowlist
	// MCPEventsPush serves MCP Events push (events/stream).
	MCPEventsPush bool
	// ProxyHops is how many trusted proxies are in front, for limits per
	// client address (agentkit.ClientAddr).
	ProxyHops int
	// GrokWebhooks wakes Grok Bot through its Automations webhook; Grok, its
	// settings (zero: the defaults).
	GrokWebhooks bool
	Grok         GrokSettings
}

// GrokSettings are Grok Bot's webhook wake's settings (WAKESHIFT_GROK_*).
type GrokSettings struct {
	LinkTTL, Spacing                         time.Duration
	PerHour, PerDay, BatchEvents, BatchBytes int
	// RateLimits: WAKESHIFT_GROK_RATE_LIMITS.
	RateLimits bool
}

// DefaultMaxStreams is WAKESHIFT_MAX_STREAMS when it isn't set: what the
// hosted service runs with, so a server nobody sized
// still refuses streams (503, Retry-After) before it runs out of memory or
// file descriptors, and one app's share (streams.AppShare) means something.
const DefaultMaxStreams = 900

// FromEnv reads the configuration from the environment.
func FromEnv() (Config, error) {
	c := Config{
		Addr:               cmp.Or(os.Getenv("WAKESHIFT_ADDR"), ":"+cmp.Or(os.Getenv("PORT"), "8080")),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		MigrateDatabaseURL: os.Getenv("WAKESHIFT_MIGRATE_DATABASE_URL"),
		PublicURL:          strings.TrimSuffix(os.Getenv("WAKESHIFT_PUBLIC_URL"), "/"), // issuers compare exactly
		SiteURL:            strings.TrimSuffix(os.Getenv("WAKESHIFT_SITE_URL"), "/"),
		LogFormat:          cmp.Or(os.Getenv("WAKESHIFT_LOG_FORMAT"), "json"),

		SecretKey: []byte(os.Getenv("WAKESHIFT_SECRET_KEY")),

		GoogleClientID:      os.Getenv("WAKESHIFT_GOOGLE_CLIENT_ID"),
		GoogleClientSecret:  os.Getenv("WAKESHIFT_GOOGLE_CLIENT_SECRET"),
		GitHubClientID:      os.Getenv("WAKESHIFT_GITHUB_CLIENT_ID"),
		GitHubClientSecret:  os.Getenv("WAKESHIFT_GITHUB_CLIENT_SECRET"),
		AllowedSignups:      os.Getenv("WAKESHIFT_ALLOWED_SIGNUPS"),
		OpenSignups:         os.Getenv("WAKESHIFT_OPEN_SIGNUPS") == "1",
		DevSignIn:           os.Getenv("WAKESHIFT_DEV_SIGN_IN") == "1",
		ConnectorDir:        os.Getenv("WAKESHIFT_CONNECTOR_DIR"),
		InstallerURL:        os.Getenv("WAKESHIFT_INSTALLER_URL"),
		AgentPlugins:        os.Getenv("WAKESHIFT_AGENT_PLUGINS"),
		OpencodePlugin:      os.Getenv("WAKESHIFT_OPENCODE_PLUGIN"),
		PiPlugin:            os.Getenv("WAKESHIFT_PI_PLUGIN"),
		OpenclawPlugin:      os.Getenv("WAKESHIFT_OPENCLAW_PLUGIN"),
		AntigravityPlugin:   os.Getenv("WAKESHIFT_ANTIGRAVITY_PLUGIN"),
		ConnectPackage:      os.Getenv("WAKESHIFT_CONNECT_PACKAGE"),
		AdminDBUser:         os.Getenv("WAKESHIFT_ADMIN_DB_USER"),
		AdminServiceAccount: os.Getenv("WAKESHIFT_ADMIN_SERVICE_ACCOUNT"),
		AdminIAPAudience:    os.Getenv("WAKESHIFT_ADMIN_IAP_AUDIENCE"),
		ResendKey:           os.Getenv("WAKESHIFT_RESEND_KEY"),
		ResendKeySecret:     os.Getenv("WAKESHIFT_RESEND_KEY_SECRET"),
		MailFrom:            cmp.Or(os.Getenv("WAKESHIFT_MAIL_FROM"), "Wakeshift <invites@wakeshift.ai>"),
		TermsURL:            os.Getenv("WAKESHIFT_TERMS_URL"),
		TermsVersion:        strings.TrimSpace(os.Getenv("WAKESHIFT_TERMS_VERSION")),
		PrivacyURL:          os.Getenv("WAKESHIFT_PRIVACY_URL"),
		SupportEmail:        cmp.Or(strings.TrimSpace(os.Getenv("WAKESHIFT_SUPPORT_EMAIL")), apps.DefaultSupportEmail),
	}
	for _, e := range strings.Split(os.Getenv("WAKESHIFT_ADMIN_EMAILS"), ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if e != strings.ToLower(e) || !strings.Contains(e, "@") {
			return c, errors.New("WAKESHIFT_ADMIN_EMAILS must be lowercase emails, as accounts store them")
		}
		c.AdminEmails = append(c.AdminEmails, e)
	}
	if c.AdminServiceAccount != "" && (strings.ToLower(c.AdminServiceAccount) != c.AdminServiceAccount || !strings.HasSuffix(c.AdminServiceAccount, ".gserviceaccount.com")) {
		return c, errors.New("WAKESHIFT_ADMIN_SERVICE_ACCOUNT is the admin service's account's email (…@….iam.gserviceaccount.com)")
	}
	if c.AdminIAPAudience != "" && (!strings.HasPrefix(c.AdminIAPAudience, "/projects/") || !strings.Contains(c.AdminIAPAudience, "/global/backendServices/")) {
		return c, errors.New("WAKESHIFT_ADMIN_IAP_AUDIENCE is /projects/<number>/global/backendServices/<id>")
	}
	if v := os.Getenv("WAKESHIFT_APPS"); v != "" {
		if err := json.Unmarshal([]byte(v), &c.Apps); err != nil {
			return c, errors.New("WAKESHIFT_APPS must be a JSON list of apps: " + err.Error())
		}
	}
	if v := os.Getenv("WAKESHIFT_OTLP_LOG_LEVEL"); v != "" {
		if err := c.OTLPLogLevel.UnmarshalText([]byte(v)); err != nil {
			return c, errors.New("WAKESHIFT_OTLP_LOG_LEVEL must be debug, info, warn or error")
		}
	}
	if v := os.Getenv("WAKESHIFT_INVITES_PER_USER"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 1000 {
			return c, errors.New("WAKESHIFT_INVITES_PER_USER must be 0-1000")
		}
		c.InvitesPerUser = n
	}
	switch os.Getenv("WAKESHIFT_PROFILER") {
	case "", "0":
	case "1":
		c.Profiler = true
	default:
		return c, errors.New(`WAKESHIFT_PROFILER must be "1" (on) or unset`)
	}
	switch os.Getenv("WAKESHIFT_SUBSCRIBE_VOUCHERS") {
	case "1":
		c.SubscribeVouchers = true
	case "0":
	case "":
		c.SubscribeVouchers = localURL(c.PublicURL)
	default:
		return c, errors.New(`WAKESHIFT_SUBSCRIBE_VOUCHERS must be "1" (on) or "0" (off); unset is on only on localhost`)
	}
	switch os.Getenv("WAKESHIFT_REDEEM_BY_TOOL_CALL") {
	case "", "0":
	case "1":
		c.RedeemByToolCall = true
	default:
		return c, errors.New(`WAKESHIFT_REDEEM_BY_TOOL_CALL must be "1" (on) or unset`)
	}
	switch os.Getenv("WAKESHIFT_CLAUDE_SESSION_STEPS") {
	case "deferred":
	case "", "inline":
		c.InlineSessionSteps = true
	default:
		return c, errors.New(`WAKESHIFT_CLAUDE_SESSION_STEPS must be "deferred" or "inline" (the default)`)
	}
	if al, err := ParseAllowlist(os.Getenv("WAKESHIFT_CODEX_ELICIT")); err != nil {
		return c, errors.New("WAKESHIFT_CODEX_ELICIT: " + err.Error())
	} else {
		c.CodexElicit = al
	}
	switch os.Getenv("WAKESHIFT_MCP_EVENTS_PUSH") {
	case "1":
		c.MCPEventsPush = true
	case "0":
	case "":
		c.MCPEventsPush = localURL(c.PublicURL)
	default:
		return c, errors.New(`WAKESHIFT_MCP_EVENTS_PUSH must be "1" (on) or "0" (off); unset is on only on localhost`)
	}
	switch os.Getenv("WAKESHIFT_GROK_WEBHOOKS") {
	case "1":
		c.GrokWebhooks = true
	case "0":
	case "":
		c.GrokWebhooks = localURL(c.PublicURL)
	default:
		return c, errors.New(`WAKESHIFT_GROK_WEBHOOKS must be "1" (on) or "0" (off); unset is on only on localhost`)
	}
	switch os.Getenv("WAKESHIFT_GROK_RATE_LIMITS") {
	case "1":
		c.Grok.RateLimits = true
	case "0", "":
	default:
		return c, errors.New(`WAKESHIFT_GROK_RATE_LIMITS must be "1" (on) or "0" (off, the default)`)
	}
	for _, d := range []struct {
		env string
		to  *time.Duration
	}{{"WAKESHIFT_GROK_LINK_TTL", &c.Grok.LinkTTL}, {"WAKESHIFT_GROK_SPACING", &c.Grok.Spacing}} {
		if v := os.Getenv(d.env); v != "" {
			t, err := time.ParseDuration(v)
			if err != nil || t <= 0 {
				return c, errors.New(d.env + " must be a positive duration, such as 24h or 20s")
			}
			*d.to = t
		}
	}
	for _, n := range []struct {
		env string
		to  *int
	}{{"WAKESHIFT_GROK_POSTS_PER_HOUR", &c.Grok.PerHour}, {"WAKESHIFT_GROK_POSTS_PER_DAY", &c.Grok.PerDay},
		{"WAKESHIFT_GROK_BATCH_EVENTS", &c.Grok.BatchEvents}, {"WAKESHIFT_GROK_BATCH_BYTES", &c.Grok.BatchBytes}} {
		if v := os.Getenv(n.env); v != "" {
			k, err := strconv.Atoi(v)
			if err != nil || k <= 0 {
				return c, errors.New(n.env + " must be a positive number")
			}
			*n.to = k
		}
	}
	if v := os.Getenv("WAKESHIFT_PROXY_HOPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, errors.New("WAKESHIFT_PROXY_HOPS must be a number of proxies (0 or more)")
		}
		c.ProxyHops = n
	} else if c.Grok.RateLimits {
		// The default (0, the connection's address) behind a proxy would
		// count every client as the proxy, and one client's failed checks
		// would lock everyone out: the operator says how many there are.
		return c, errors.New("WAKESHIFT_GROK_RATE_LIMITS=1 needs WAKESHIFT_PROXY_HOPS: the number of trusted proxies in front that append to X-Forwarded-For " +
			"(0 if clients connect directly, 2 behind Google's load balancer)")
	}
	c.MaxStreams = DefaultMaxStreams
	if v := os.Getenv("WAKESHIFT_MAX_STREAMS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, errors.New("WAKESHIFT_MAX_STREAMS must be a number of streams (0 for no cap)")
		}
		c.MaxStreams = n
	}
	if v := os.Getenv("WAKESHIFT_CONNECTOR_MIN_VERSIONS"); v != "" {
		m, err := connectorversion.ParseMinimums(v)
		if err != nil {
			return c, errors.New("WAKESHIFT_CONNECTOR_MIN_VERSIONS: " + err.Error())
		}
		c.Connectors = m
	}
	if c.InstallerURL != "" {
		u, err := url.Parse(c.InstallerURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return c, errors.New("WAKESHIFT_INSTALLER_URL must be an https URL with no query, e.g. https://github.com/wakeshift/connectors/releases/latest/download")
		}
	}
	if c.AntigravityPlugin != "" && !harnesses.ValidPluginRepo(c.AntigravityPlugin) {
		return c, errors.New("WAKESHIFT_ANTIGRAVITY_PLUGIN must be a GitHub repository's https URL (optionally /tree/<branch>) or an absolute path to a folder")
	}
	if c.AgentPlugins != "" && !harnesses.ValidAgentPlugins(c.AgentPlugins) {
		return c, errors.New("WAKESHIFT_AGENT_PLUGINS must be an absolute path or owner/repo, e.g. /tmp/agent-plugins")
	}
	if v := c.OpencodePlugin; v != "" && v != "off" && !harnesses.ValidPackageVersion(v) && !npmtag.TagRE.MatchString(v) {
		return c, errors.New("WAKESHIFT_OPENCODE_PLUGIN must be a version of @wakeshift/opencode (1.5.0), a dist-tag (staging) or off")
	}
	if v := c.PiPlugin; v != "" && v != "off" && !harnesses.ValidPackageVersion(v) && !npmtag.TagRE.MatchString(v) {
		return c, errors.New("WAKESHIFT_PI_PLUGIN must be a version of @wakeshift/pi (1.5.0), a dist-tag (staging) or off")
	}
	if v := c.OpenclawPlugin; v != "" && v != "off" && !harnesses.ValidPackageVersion(v) && !npmtag.TagRE.MatchString(v) {
		return c, errors.New("WAKESHIFT_OPENCLAW_PLUGIN must be a version of @wakeshift/openclaw (1.5.0), a dist-tag (staging) or off")
	}
	if v := c.ConnectPackage; v != "" && v != "off" && !harnesses.ValidPackageVersion(v) && !npmtag.TagRE.MatchString(v) {
		return c, errors.New("WAKESHIFT_CONNECT_PACKAGE must be a version of @wakeshift/connect (1.5.0), a dist-tag (staging) or off")
	}
	switch {
	case c.DatabaseURL == "":
		return c, errors.New("DATABASE_URL is required")
	case !validPublicURL(c.PublicURL):
		return c, errors.New("WAKESHIFT_PUBLIC_URL is required: an absolute URL with no path, https except on localhost")
	case c.SiteURL != "" && !validPublicURL(c.SiteURL):
		return c, errors.New("WAKESHIFT_SITE_URL must be an absolute URL with no path, https except on localhost")
	case c.DevSignIn && os.Getenv("K_SERVICE") != "":
		return c, errors.New("WAKESHIFT_DEV_SIGN_IN can't be used on Cloud Run")
	case c.DevSignIn && !loopbackAddr(c.Addr):
		return c, errors.New("WAKESHIFT_DEV_SIGN_IN needs WAKESHIFT_ADDR on loopback, e.g. 127.0.0.1:8370 (the default listens on every interface)")
	case c.DevSignIn && os.Getenv("WAKESHIFT_DEV_SIGN_IN_TOKEN") != "" && len(os.Getenv("WAKESHIFT_DEV_SIGN_IN_TOKEN")) < 16:
		return c, errors.New("WAKESHIFT_DEV_SIGN_IN_TOKEN must be at least 16 characters (unset: one is made at startup)")
	case len(c.SecretKey) < 32:
		return c, errors.New("WAKESHIFT_SECRET_KEY is required and must be at least 32 bytes")
	case string(c.SecretKey) == DevSecretKey && (!localURL(c.PublicURL) || !loopbackAddr(c.Addr)):
		return c, errors.New("WAKESHIFT_SECRET_KEY is the published development key, allowed only with a localhost WAKESHIFT_PUBLIC_URL and WAKESHIFT_ADDR on loopback (e.g. 127.0.0.1:8370); else generate a random one (e.g. openssl rand -base64 48)")
	case (c.GoogleClientID == "") != (c.GoogleClientSecret == ""):
		return c, errors.New("set both WAKESHIFT_GOOGLE_CLIENT_ID and WAKESHIFT_GOOGLE_CLIENT_SECRET, or neither")
	case (c.GitHubClientID == "") != (c.GitHubClientSecret == ""):
		return c, errors.New("set both WAKESHIFT_GITHUB_CLIENT_ID and WAKESHIFT_GITHUB_CLIENT_SECRET, or neither")
	}
	if c.SiteURL == "" {
		c.SiteURL = SiteFor(c.PublicURL)
	}
	if c.DevSignIn {
		c.DevSignInToken = os.Getenv("WAKESHIFT_DEV_SIGN_IN_TOKEN")
		if c.DevSignInToken == "" {
			b := make([]byte, 24)
			rand.Read(b) // never fails (crypto/rand)
			c.DevSignInToken, c.DevSignInTokenMade = hex.EncodeToString(b), true
		}
	}
	switch {
	case (c.TermsURL == "") != (c.TermsVersion == ""):
		return c, errors.New("set both WAKESHIFT_TERMS_URL and WAKESHIFT_TERMS_VERSION, or neither")
	case c.TermsURL != "" && !validLinkURL(c.TermsURL):
		return c, errors.New("WAKESHIFT_TERMS_URL must be an absolute URL, https except on localhost")
	case c.PrivacyURL != "" && c.TermsURL == "":
		return c, errors.New("WAKESHIFT_PRIVACY_URL is shown with the Terms: set WAKESHIFT_TERMS_URL and WAKESHIFT_TERMS_VERSION too")
	case c.PrivacyURL != "" && !validLinkURL(c.PrivacyURL):
		return c, errors.New("WAKESHIFT_PRIVACY_URL must be an absolute URL, https except on localhost")
	case len(c.TermsVersion) > 100:
		return c, errors.New("WAKESHIFT_TERMS_VERSION is a short label, such as the Terms' date")
	case !validEmail(c.SupportEmail):
		return c, errors.New("WAKESHIFT_SUPPORT_EMAIL must be an email address")
	}
	if site, ok := hostedSites[c.PublicURL]; ok && c.TermsURL == "" {
		// Wakeshift's hosted service (production, staging): its own Terms,
		// on its own site whatever WAKESHIFT_SITE_URL says.
		c.TermsURL, c.PrivacyURL, c.TermsVersion = site+"/terms", site+"/privacy", accounts.TermsVersion
	}
	return c, nil
}

// Hosted says whether this is Wakeshift's hosted service (production or
// staging), by its public URL, as for its Terms (hostedSites).
func (c Config) Hosted() bool {
	_, ok := hostedSites[c.PublicURL]
	return ok
}

// SignupsClosed says whether only the allow list makes new accounts:
// on a self-hosted server, unless WAKESHIFT_OPEN_SIGNUPS=1. The hosted
// service keeps its sign-up cap, invites and allow list
// (docs/self-hosting.md, "Who can sign up").
func (c Config) SignupsClosed() bool { return !c.Hosted() && !c.OpenSignups }

// hostedSites are Wakeshift's hosted service's consoles (production's and
// staging's, no other host) and the sites whose Terms and Privacy Policy
// they show by default.
var hostedSites = map[string]string{
	"https://app.wakeshift.ai":         "https://wakeshift.ai",
	"https://app.staging.wakeshift.ai": "https://staging.wakeshift.ai",
}

// validLinkURL is an absolute URL a page may link to: https, or http on
// localhost.
func validLinkURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && localURL(raw))
}

func validEmail(e string) bool {
	at := strings.LastIndex(e, "@")
	return at > 0 && at < len(e)-1 && !strings.ContainsAny(e, " \t\r\n<>\"(),;:")
}

// Key derives the key for one purpose ("console-session", "stream-link")
// from the secret key, so a key leaked or misused in one place can't forge
// anything in another.
func (c Config) Key(purpose string) []byte {
	m := hmac.New(sha256.New, c.SecretKey)
	m.Write([]byte("wakeshift/" + purpose))
	return m.Sum(nil)
}

// DevSecretKey is the Makefile's development key. It's public, so the
// server refuses it anywhere but localhost.
const DevSecretKey = "dev-secret-key-at-least-32-bytes-long"

// SiteFor is the public site's URL for a console's: its host without
// "app." (https://app.wakeshift.ai's is https://wakeshift.ai), or "" for a
// console that isn't at app.<site>, such as a local one.
func SiteFor(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	host, ok := strings.CutPrefix(u.Host, "app.")
	if !ok || host == "" || strings.HasPrefix(host, ":") {
		return ""
	}
	return u.Scheme + "://" + host
}

func validPublicURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && localURL(raw))
}

// loopbackAddr reports whether a listen address (WAKESHIFT_ADDR) binds to
// loopback only: localhost or a loopback IP, never all interfaces (":8080").
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func localURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
