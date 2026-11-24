// Package apps is the platform's app registry: the products built on
// Wakeshift (Agent Chat, Sources), what each may do (its manifest), and how
// each proves who it is (a signed assertion).
//
// Our own apps are registered at deploy time (WAKESHIFT_APPS: id, name, MCP
// URL and an Ed25519 public key) and are live. Anyone signed in can register
// a test app in the console (selfserve.go), which gets a key at once. An app
// then publishes its manifest itself, signed
// with its private key. Every event type in a manifest is in the app's own
// namespace (app.<id>.…), so an app can never ask for another app's events
// or a source's.
package apps

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/internal/orgs"
)

// Config registers an app (from WAKESHIFT_APPS).
type Config struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	MCPURL    string `json:"mcp_url"`    // empty for an app with no MCP server
	PublicKey string `json:"public_key"` // base64 (standard or URL) Ed25519 public key
	// Unlisted is no longer read: the console lists only the apps with
	// grants in a workspace. Kept so a configuration that sets it still
	// parses.
	Unlisted bool `json:"unlisted,omitempty"`
	// WorkspaceCap overrides the app's workspace cap (WorkspaceCap); unset
	// is the default. WAKESHIFT_APPS is where ours is set: Sync writes it
	// on every start, and the admin service doesn't change it.
	WorkspaceCap *int `json:"workspace_cap,omitempty"`
}

// App is a registered app.
type App struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	MCPURL      string    `json:"mcp_url"`
	Manifest    Manifest  `json:"manifest"`
	CreatedAt   time.Time `json:"created_at"`
	// WorkspaceCapOverride is our override of the app's workspace cap
	// (SetWorkspaceCap); nil is the default (WorkspaceCap).
	WorkspaceCapOverride *int `json:"-"`
	// EmitRecipientsOverride is our override of the app's limit on
	// recipients a minute across its multi-recipient emits
	// (EmitRecipientsPerMinute); nil is the default.
	EmitRecipientsOverride *int `json:"-"`
	// SuspendedAt is set while we've suspended the app (Suspend): its
	// keys and publish URLs are refused.
	SuspendedAt *time.Time `json:"-"`
	// KeyID and KeyName are the key that signed the request: its console
	// key ID and its name (set by Authenticate).
	KeyID   string `json:"-"`
	KeyName string `json:"-"`

	owned   bool      // a console app (it has an owner)
	keyRole orgs.Role // the signing key's maker's role when they made it (KeyMay)
}

// KeyMay says whether the key that signed the request may do a through the
// app API: for a console app, only if the role of the member who made or
// registered the key, when they did, allows a (orgs/roles.go), so a key a
// developer made can't do what only owners and admins may (users/forget).
// A key whose maker's role is unknown (they've left; or a key the previous
// revision made during a deploy) may do only what every role may. A
// WAKESHIFT_APPS app has no organization: its operator holds its keys.
func (a App) KeyMay(act orgs.Action) bool {
	if !a.owned {
		return true
	}
	if a.keyRole == "" {
		return orgs.Developer.Can(act)
	}
	return a.keyRole.Can(act)
}

// Manifest is what an app may do.
type Manifest struct {
	Description string `json:"description"`
	// Accounts is who identifies the app's users: "app" (the default and
	// only choice: headless, the app keeps a workspace per user). Validate
	// refuses "wakeshift", account mode.
	Accounts string `json:"accounts,omitempty"`
	// Agent is what the app does for its users' agents.
	Agent *AgentScope `json:"agent,omitempty"`
	// Workspace was account mode's publishing into people's own
	// workspaces: kept only so that Validate refuses it.
	Workspace *json.RawMessage `json:"workspace,omitempty"`
	// Events are the event types clients can subscribe to by name and
	// arguments (eventtypes.go).
	Events []EventType `json:"events,omitempty"`
	// OthersCanWake: the app wakes its users' agents with what other
	// people and their agents do (messages, moves: Agent Chat, the games),
	// so the consent screen says so (grants.AgentConsent).
	OthersCanWake bool `json:"others_can_wake,omitempty"`
	// PreapproveTools are the app's own MCP tools (by name, as its server
	// lists them) that it says agents' programs can run without asking
	// each time: tools that only read, and routine ones an agent calls on
	// every wake that reach only what the agent already joined. Never a
	// tool that deletes, spends, or reaches anyone the user hasn't already
	// chosen. When a user connects an agent program that has exact
	// per-tool rules (Claude Code, Cursor CLI), Wakeshift offers to allow
	// these, by name, in the same question as the install.
	PreapproveTools []string `json:"preapprove_tools,omitempty"`
}

// MaxPreapproveTools is how many tools a manifest's preapprove_tools may
// name.
const MaxPreapproveTools = 20

// ToolNameRE matches a tool name an exact rule can hold in every agent
// program's syntax: no glob, separator or quoting characters. Validate
// also refuses "__" (Claude Code's mcp__<server>__<tool> separator).
var ToolNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// AgentScope: the events an agent may be woken by, and post, through the app.
type AgentScope struct {
	Wake []ScopeEvent `json:"wake"`
	Post []ScopeEvent `json:"post"`
	// WakeLimits pace how often the app may wake the agent, on top of the
	// platform's (DefaultWakeLimits); wake-ups past a limit wait their turn.
	WakeLimits []WakeLimit `json:"wake_limits,omitempty"`
	TTLDays    int         `json:"ttl_days"`
	// RedirectURIs were account mode's Sign in with Wakeshift: kept only
	// so that Validate refuses them.
	RedirectURIs []string `json:"redirect_uris,omitempty"`
	// StartPrompt is what to tell the agent once it's connected ("Join a
	// game of Nightfall"): shown after consent.
	StartPrompt string `json:"start_prompt,omitempty"`
}

// WakeLimit is at most Max wake-ups in any Seconds.
type WakeLimit struct {
	Seconds int `json:"seconds"`
	Max     int `json:"max"`
}

// DefaultWakeLimits apply to every grant: bursts are fine, sustained floods
// aren't.
var DefaultWakeLimits = []WakeLimit{{Seconds: 1, Max: 10}, {Seconds: 300, Max: 450}, {Seconds: 3600, Max: 3600}}

// Limits is every limit on the app waking an agent: the platform's and the
// app's own (which can only add to them).
func (a AgentScope) Limits() []WakeLimit {
	return append(append([]WakeLimit(nil), DefaultWakeLimits...), a.WakeLimits...)
}

// ScopeEvent is an event type or pattern, with the app's description of it.
type ScopeEvent struct {
	Pattern     string `json:"pattern"`
	Description string `json:"description"`
}

var (
	idRE      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)
	patternRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*(\.[a-z0-9][a-z0-9_-]*)*(\.\*)?$`)

	ErrNotFound = errors.New("apps: no such app")
)

const (
	AccountsApp = "app"
	// AccountsWakeshift was account mode (Sign in with Wakeshift: an app
	// with grants in people's own workspaces), now turned off. A manifest can't ask for it.
	AccountsWakeshift = "wakeshift"
)

// errNoAccountMode is the refusal of anything in a manifest that would
// reach people's own workspaces.
func errNoAccountMode(what string) error {
	return fmt.Errorf("%s is no longer available: apps on Wakeshift identify their own users (with their own sign-in), "+
		"in workspaces the app keeps for them", what)
}

// Validate checks a manifest against the app's namespace and limits.
func (m Manifest) Validate(appID string) error {
	return m.validate(appID, nil)
}

// validate is Validate, except that an ambiguous topic (AmbiguousTopics)
// that was already stored (in stored, the manifest being replaced) is
// logged, not refused: an app published before the rule keeps working
// while it republishes the same manifest, but can't add another.
func (m Manifest) validate(appID string, stored *Manifest) error {
	switch {
	case m.Accounts == AccountsWakeshift:
		return errNoAccountMode(`accounts: "wakeshift" (Sign in with Wakeshift)`)
	case m.Accounts != "" && m.Accounts != AccountsApp:
		return errors.New(`accounts is "app" (the default)`)
	case m.Workspace != nil:
		return errNoAccountMode("workspace (publishing into people's own workspaces)")
	case m.Agent != nil && len(m.Agent.RedirectURIs) > 0:
		return errNoAccountMode("agent.redirect_uris (signing in Wakeshift users)")
	}
	ns := "app." + appID + "."
	check := func(what string, list []ScopeEvent) error {
		for _, e := range list {
			if !patternRE.MatchString(e.Pattern) || !strings.HasPrefix(e.Pattern, ns) {
				return fmt.Errorf("%s pattern %q must be in the app's namespace (%s…)", what, e.Pattern, ns)
			}
			if len(e.Description) > 300 {
				return fmt.Errorf("%s pattern %q: description over 300 characters", what, e.Pattern)
			}
		}
		return nil
	}
	if len(m.Description) > 500 {
		return errors.New("description over 500 characters")
	}
	if err := checkEvents(m.Events); err != nil {
		return err
	}
	if err := checkTopicsUnambiguous(appID, m.Events, stored); err != nil {
		return err
	}
	if len(m.PreapproveTools) > MaxPreapproveTools {
		return fmt.Errorf("preapprove_tools: at most %d tools", MaxPreapproveTools)
	}
	for i, t := range m.PreapproveTools {
		if !ToolNameRE.MatchString(t) || strings.Contains(t, "__") {
			return fmt.Errorf("preapprove_tools: %q isn't a tool name an exact rule can hold (letters, digits, _ and -, at most 64, and not __)", t)
		}
		if slices.Contains(m.PreapproveTools[:i], t) {
			return fmt.Errorf("preapprove_tools: %q is listed twice", t)
		}
	}
	if a := m.Agent; a != nil {
		if err := check("wake", a.Wake); err != nil {
			return err
		}
		if err := check("post", a.Post); err != nil {
			return err
		}
		if len(a.StartPrompt) > 120 {
			return errors.New("apps: agent.start_prompt is at most 120 characters")
		}
		if a.TTLDays < 1 || a.TTLDays > 365 {
			return errors.New("ttl_days must be 1-365")
		}
		if len(a.WakeLimits) > 5 {
			return errors.New("at most 5 wake_limits")
		}
		for _, l := range a.WakeLimits {
			if l.Seconds < 1 || l.Seconds > 86400 || l.Max < 1 || l.Max > 100000 {
				return errors.New("wake_limits are {seconds: 1-86400, max: 1-100000}")
			}
		}
	}
	return nil
}

func validConnectURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return u.Scheme == "https" || u.Scheme == "http"
	}
	return u.Scheme == "https"
}

// Registry reads and writes apps. Apps are platform-level (not
// tenant-scoped).
type Registry struct {
	Pool   *pgxpool.Pool
	Issuer string // the core's public URL; assertions are addressed to Issuer + "/api/apps"
}

// Sync registers or updates the configured apps. It never takes over an app
// registered in the console: an ID that's taken fails, loudly, at startup.
// The configured public key is the app's one key: any other is revoked.
func (r *Registry) Sync(ctx context.Context, list []Config) error {
	for _, c := range list {
		if !idRE.MatchString(c.ID) || c.Name == "" {
			return fmt.Errorf("apps: %q needs an id (lowercase) and a name", c.ID)
		}
		key, err := decodeKey(c.PublicKey)
		if err != nil {
			return fmt.Errorf("apps: %s: %w", c.ID, err)
		}
		err = pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
			if c.WorkspaceCap != nil && (*c.WorkspaceCap < 0 || *c.WorkspaceCap > MaxWorkspaceCap) {
				return fmt.Errorf("apps: %s's workspace_cap is 0 to %d", c.ID, MaxWorkspaceCap)
			}
			tag, err := tx.Exec(ctx, `INSERT INTO apps (id, name, mcp_url, workspace_cap) VALUES ($1, $2, nullif($3, ''), $4)
				ON CONFLICT (id) DO UPDATE SET name = excluded.name, mcp_url = excluded.mcp_url, workspace_cap = excluded.workspace_cap
				WHERE apps.org_id IS NULL`, c.ID, c.Name, c.MCPURL, c.WorkspaceCap)
			if err != nil {
				return fmt.Errorf("apps: registering %s: %w", c.ID, err)
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("apps: %s is taken by an app registered in the console; pick another ID", c.ID)
			}
			var deleted bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deleted_key_ids WHERE kid_hash = $1)`, kidHash(appkey.KeyID(key))).Scan(&deleted); err != nil {
				return err
			} else if deleted {
				return fmt.Errorf("apps: %s's public key was a deleted app's; make a new one", c.ID)
			}
			tag, err = tx.Exec(ctx, `INSERT INTO app_keys (app_id, key_id, kid, name, public_key) VALUES ($1, $2, $3, 'WAKESHIFT_APPS', $4)
				ON CONFLICT (public_key) DO UPDATE SET revoked_at = NULL, revoked_by = NULL, kid = excluded.kid WHERE app_keys.app_id = excluded.app_id`,
				c.ID, KeyID(key), appkey.KeyID(key), key)
			if err != nil {
				return fmt.Errorf("apps: registering %s's key: %w", c.ID, err)
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("apps: %s's public key is another app's", c.ID)
			}
			_, err = tx.Exec(ctx, `UPDATE app_keys SET revoked_at = now(), revoked_by = 'config' WHERE app_id = $1 AND public_key <> $2 AND revoked_at IS NULL`, c.ID, key)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func decodeKey(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == ed25519.PublicKeySize {
			return b, nil
		}
	}
	return nil, errors.New("public_key must be a base64 Ed25519 public key")
}

const columns = `id, name, description, coalesce(mcp_url, ''), manifest, created_at, org_id IS NOT NULL, workspace_cap, suspended_at, emit_recipients_per_minute`

func scan(row pgx.Row) (App, error) {
	var a App
	var m []byte
	err := row.Scan(&a.ID, &a.Name, &a.Description, &a.MCPURL, &m, &a.CreatedAt, &a.owned, &a.WorkspaceCapOverride, &a.SuspendedAt, &a.EmitRecipientsOverride)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(m, &a.Manifest)
	}
	return a, err
}

// Get returns an app; a deleted one is ErrNotFound.
func (r *Registry) Get(ctx context.Context, id string) (App, error) {
	return scan(r.Pool.QueryRow(ctx, `SELECT `+columns+` FROM apps WHERE id = $1 AND deleted_at IS NULL`, id))
}

// SetManifest stores an app's manifest after validating it.
func (r *Registry) SetManifest(ctx context.Context, id string, m Manifest) error {
	return r.SetManifestSigned(ctx, id, "", m)
}

// ErrInvalidManifest is what a manifest's validation errors are (see
// errors.Is); their text says what to fix. Other errors from SetManifest
// are internal.
var ErrInvalidManifest = errors.New("apps: invalid manifest")

type invalidManifest struct{ error }

func (e invalidManifest) Unwrap() error      { return e.error }
func (invalidManifest) Is(target error) bool { return target == ErrInvalidManifest }

// SetManifestSigned is SetManifest for a request keyID signed (the log
// names the key).
func (r *Registry) SetManifestSigned(ctx context.Context, id, keyID string, m Manifest) error {
	_, _, err := r.PublishManifest(ctx, id, keyID, m, nil)
	return err
}

// checkTopicsUnambiguous refuses the first of events' ambiguous topics
// that stored (if any) didn't already have, and logs those it did.
func checkTopicsUnambiguous(appID string, events []EventType, stored *Manifest) error {
	found := AmbiguousTopics(events)
	if len(found) == 0 {
		return nil
	}
	had := map[string]bool{}
	if stored != nil {
		for _, a := range AmbiguousTopics(stored.Events) {
			had[a.key()] = true
		}
	}
	for _, a := range found {
		if !had[a.key()] {
			return a
		}
	}
	for _, a := range found {
		slog.Warn("apps: a stored manifest has an ambiguous topic (kept, since it was stored before the rule)",
			"app", appID, "event", a.Event, "topic", a.Topic, "other_event", a.Other, "other_topic", a.OtherTopic)
	}
	return nil
}

// LogAmbiguousTopics logs every stored manifest's ambiguous topics
// (AmbiguousTopics), at startup: manifests stored before the rule are
// kept, and this says which they are.
func (r *Registry) LogAmbiguousTopics(ctx context.Context) error {
	rows, err := r.Pool.Query(ctx, `SELECT id, manifest FROM apps WHERE deleted_at IS NULL AND manifest IS NOT NULL ORDER BY id`)
	if err != nil {
		return err
	}
	type stored struct {
		ID       string
		Manifest []byte
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[stored])
	if err != nil {
		return err
	}
	for _, a := range list {
		var m Manifest
		if json.Unmarshal(a.Manifest, &m) != nil {
			continue
		}
		for _, p := range AmbiguousTopics(m.Events) {
			slog.Warn("apps: a stored manifest has an ambiguous topic (kept, since it was stored before the rule; a new one is refused)",
				"app", a.ID, "event", p.Event, "topic", p.Topic, "other_event", p.Other, "other_topic", p.OtherTopic)
		}
	}
	return nil
}

// ManifestPatch is a manifest to merge into the stored one
// (MergeManifestSigned): what an SDK sends automatically from a server
// that serves the app's tools. A field it leaves out (or sends as null)
// keeps what's stored; one it sends replaces it, the agent scope as a
// whole. Its events are merged by name: a stored event it names is
// replaced, a new one is added, and one it doesn't name is kept. Only a
// full manifest (SetManifest, PUT /manifest) removes an event or clears a
// setting.
type ManifestPatch struct {
	Description     *string          `json:"description,omitempty"`
	Accounts        *string          `json:"accounts,omitempty"`
	Agent           *AgentScope      `json:"agent,omitempty"`
	Workspace       *json.RawMessage `json:"workspace,omitempty"`
	Events          []EventType      `json:"events,omitempty"`
	OthersCanWake   *bool            `json:"others_can_wake,omitempty"`
	PreapproveTools *[]string        `json:"preapprove_tools,omitempty"`
}

// Apply is m with p merged into it. An event p names twice is an error.
func (p ManifestPatch) Apply(m Manifest) (Manifest, error) {
	if p.Description != nil {
		m.Description = *p.Description
	}
	if p.Accounts != nil {
		m.Accounts = *p.Accounts
	}
	if p.Agent != nil {
		m.Agent = p.Agent
	}
	if p.Workspace != nil {
		m.Workspace = p.Workspace
	}
	if p.OthersCanWake != nil {
		m.OthersCanWake = *p.OthersCanWake
	}
	if p.PreapproveTools != nil {
		m.PreapproveTools = slices.Clone(*p.PreapproveTools)
	}
	m.Events = slices.Clone(m.Events)
	for i, e := range p.Events {
		if slices.ContainsFunc(p.Events[:i], func(o EventType) bool { return o.Name == e.Name }) {
			return Manifest{}, fmt.Errorf("event %q is declared twice", e.Name)
		}
		if j := slices.IndexFunc(m.Events, func(o EventType) bool { return o.Name == e.Name }); j >= 0 {
			m.Events[j] = e
		} else {
			m.Events = append(m.Events, e)
		}
	}
	return m, nil
}

// MergeManifestSigned merges p into app id's stored manifest (see
// ManifestPatch), for a request keyID signed, and returns the manifest it
// stored. It validates the merged manifest, as SetManifest does.
func (r *Registry) MergeManifestSigned(ctx context.Context, id, keyID string, p ManifestPatch) (Manifest, error) {
	m, _, err := r.MergeManifest(ctx, id, keyID, p, nil)
	return m, err
}

// SubscribeChange is an event whose SubscribePolicy a manifest changed:
// from and to are "tools" or "any"; Ended is how many subscriptions
// closing it ended (the SubscribeHook's count).
type SubscribeChange struct {
	Event string `json:"event"`
	From  string `json:"from"`
	To    string `json:"to"`
	Ended int    `json:"ended"`
}

// Closes says the change makes the event tools-only.
func (c SubscribeChange) Closes() bool { return c.To == SubscribeTools }

// SubscribeHook acts on a manifest's SubscribeChanges inside the
// transaction that stores it, before it commits: ending what a close
// makes illegitimate and recording each change (appapi), setting each
// change's Ended. The transaction is outside any workspace; a hook that
// sets app.tenant_id clears it before returning.
type SubscribeHook func(ctx context.Context, tx pgx.Tx, app string, changes []SubscribeChange) error

// PublishManifest stores m as app id's manifest, for a request keyID
// signed, and returns it as stored (each event's subscribe setting filled
// in from the stored one where m leaves it out) with the subscribe
// settings it changed, which hook (if any) acts on in the same
// transaction.
func (r *Registry) PublishManifest(ctx context.Context, id, keyID string, m Manifest, hook SubscribeHook) (Manifest, []SubscribeChange, error) {
	return r.storeManifest(ctx, id, keyID, false, func(Manifest) (Manifest, error) { return m, nil }, hook)
}

// MergeManifest is MergeManifestSigned, with PublishManifest's changes
// and hook.
func (r *Registry) MergeManifest(ctx context.Context, id, keyID string, p ManifestPatch, hook SubscribeHook) (Manifest, []SubscribeChange, error) {
	return r.storeManifest(ctx, id, keyID, true, p.Apply, hook)
}

// storeManifest stores the manifest next makes of the stored one, once
// it's valid, and logs what changed. merged says next merged into the
// stored manifest rather than replacing it (the log entry says so).
//
// Subscribe settings are sticky (applySubscribe): one an event leaves out
// is the stored one. A setting that differs from the stored one (an event
// never given one is "any") is a SubscribeChange, for an event the app
// had before; a new event's first setting changes nothing.
func (r *Registry) storeManifest(ctx context.Context, id, keyID string, merged bool, next func(Manifest) (Manifest, error), hook SubscribeHook) (Manifest, []SubscribeChange, error) {
	var m Manifest
	var changes []SubscribeChange
	err := pgx.BeginFunc(ctx, r.Pool, func(tx pgx.Tx) error {
		var oldJSON, policyJSON []byte
		var published *time.Time
		err := tx.QueryRow(ctx, `SELECT manifest, manifest_updated_at, event_subscribe FROM apps WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`,
			id).Scan(&oldJSON, &published, &policyJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var old Manifest
		if err := json.Unmarshal(oldJSON, &old); err != nil {
			return err
		}
		policy := map[string]string{}
		if err := json.Unmarshal(policyJSON, &policy); err != nil {
			return err
		}
		if m, err = next(old); err != nil {
			return invalidManifest{err}
		}
		if err := m.validate(id, &old); err != nil {
			return invalidManifest{err}
		}
		changes = applySubscribe(&m, old, policy)
		b, _ := json.Marshal(m)
		pb, _ := json.Marshal(policy)
		if _, err := tx.Exec(ctx, `UPDATE apps SET manifest = $2, description = $3, manifest_updated_at = now(), event_subscribe = $4 WHERE id = $1`,
			id, b, m.Description, pb); err != nil {
			return err
		}
		if hook != nil && len(changes) > 0 {
			if err := hook(ctx, tx, id, changes); err != nil {
				return err
			}
		}
		// The app's log: the first manifest, and every one that changes
		// something (apps republish on every start; unchanged ones aren't
		// logged).
		logged := manifestChanges(old, m)
		if published != nil && len(logged) == 0 {
			return nil
		}
		detail := map[string]any{"first": published == nil, "changes": logged}
		if merged {
			detail["merged"] = true
		}
		return Log(ctx, tx, LogEntry{AppID: id, Actor: "app:" + id, Action: LogManifestPublished, KeyID: keyID, Detail: detail})
	})
	if changes == nil {
		changes = []SubscribeChange{}
	}
	return m, changes, err
}

// applySubscribe makes m's subscribe settings sticky against policy (every
// setting the app's manifests have given, by event, which it updates): an
// event that leaves it out gets the stored one. It returns the settings
// that changed for events the app had before (in old, or given a setting
// once).
func applySubscribe(m *Manifest, old Manifest, policy map[string]string) []SubscribeChange {
	var changes []SubscribeChange
	m.Events = slices.Clone(m.Events)
	for i := range m.Events {
		e := &m.Events[i]
		stored, given := policy[e.Name]
		if e.SubscribePolicy == "" {
			e.SubscribePolicy = stored
			continue
		}
		_, err := old.EventType(e.Name)
		existed := err == nil || given
		if from := effectivePolicy(stored); existed && from != e.SubscribePolicy {
			changes = append(changes, SubscribeChange{Event: e.Name, From: from, To: e.SubscribePolicy})
		}
		policy[e.Name] = e.SubscribePolicy
	}
	return changes
}

// Assertion is what an app signs to prove who it is: a short-lived JWT
// (typ "app+jwt") addressed to the core, with iss = sub = its ID or, from
// SDKs whose key doesn't name the app, iss = sub = the key's kid. Current
// SDKs put the kid in the header too.
type Assertion struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
	ID       string `json:"jti"`
}

// AssertionAudience is where app assertions are addressed.
func (r *Registry) AssertionAudience() string { return strings.TrimSuffix(r.Issuer, "/") + "/api/apps" }

// maxAssertionLife bounds how long an assertion is valid, which also bounds
// how long its ID must be remembered to refuse a replay.
const maxAssertionLife = 5 * time.Minute

// ErrUnauthenticated is what every refusal of an app's assertion is (see
// errors.Is). A refusal's text is fixed and safe to send back; any other
// error from Authenticate is internal (the database, say): log it, and
// answer 500 without its text.
var ErrUnauthenticated = errors.New("apps: unauthenticated")

// ErrSuspended refuses a suspended app's assertions and publish URLs, and
// new keys and publish URLs for it: 403 suspended, answered with
// SuspendedMessage (which names the server's support address), not this
// text.
var ErrSuspended = errors.New("this app is suspended")

type refusal string

func (r refusal) Error() string      { return string(r) }
func (refusal) Is(target error) bool { return target == ErrUnauthenticated }

// Authenticate verifies an app assertion and returns the app, with the key
// that signed it. Keys are read on every request, so a revoked key fails
// at once. Refusals are ErrUnauthenticated, or ErrSuspended for a
// suspended app's valid assertion, or ErrWaitlisted for a valid assertion
// signed by a key held on the waitlist (pending.go).
//
// An assertion with a kid in its header is verified with that one key,
// found across every app's keys (app_keys_kid), and its issuer and
// subject must both be the kid or both the key's app's ID. One without (an
// older SDK's) is verified as before: by any of the issuer's keys. Both
// paths stay, permanently: old keys and old SDKs keep working.
func (r *Registry) Authenticate(ctx context.Context, token string, now time.Time) (App, error) {
	// The key depends on the header's kid or the claimed issuer, so read
	// them first (unverified), then verify the signature with that key.
	header, claims, ok := unverified(token)
	if !ok {
		return App{}, refusal("invalid assertion")
	}
	// What the token alone can show wrong is refused before any database
	// work, so a made-up or stale token costs no query. These refusals
	// depend only on what the caller sent (and the time), so answering
	// them before the signature is checked tells it nothing about the
	// apps or keys here.
	if err := r.checkClaims(header, claims, now); err != nil {
		return App{}, err
	}
	app, signer, err := r.signer(ctx, token, header, claims, now)
	if err != nil {
		return App{}, err
	}
	// Checked once the signature is: a suspended app's keys all stop at
	// once, and nobody without one of its keys learns it's suspended.
	if app.SuspendedAt != nil {
		return App{}, ErrSuspended
	}
	// signer verified the signature over the header and claims read above.
	a, kid := claims, appkey.KeyID(signer.public)
	if !(a.Issuer == app.ID && a.Subject == app.ID) && !(header.Kid != "" && a.Issuer == kid && a.Subject == kid) {
		return App{}, refusal("assertion issuer and subject must be the app")
	}
	first, err := r.firstUse(ctx, app.ID, a.ID, time.Unix(a.Expires, 0))
	if err != nil {
		return App{}, fmt.Errorf("apps: recording an assertion's ID: %w", err)
	}
	if !first {
		return App{}, refusal("assertion already used")
	}
	app.KeyID, app.KeyName, app.keyRole = signer.id, signer.name, signer.role
	if err := r.markKeyUsed(ctx, app, signer, now); err != nil {
		// The request is fine; the next one tries again.
		slog.Error("apps: recording a key's use", "app", app.ID, "err", err)
	}
	if err := r.fillKID(ctx, signer); err != nil {
		slog.Error("apps: recording a key's kid", "app", app.ID, "err", err)
	}
	return app, nil
}

// signer finds the app and the one key that must have signed token, its
// signature verified: by the header's kid, or else by the claimed issuer.
// Unknown kids and app IDs, revoked keys and deleted apps all read as a bad
// signature does, so the API can't be used to list app IDs or probe for
// keys. Any other error is internal: callers log it and answer 500 without
// it.
func (r *Registry) signer(ctx context.Context, token string, h jws.Header, claims Assertion, now time.Time) (App, activeKey, error) {
	if h.Kid != "" {
		app, k, found, err := r.signerByKID(ctx, token, h.Kid)
		switch {
		case found || err != nil:
			return app, k, err
		case claims.Issuer == h.Kid:
			// A key that names no app: a key held on
			// the waitlist (pending.go) is told so; otherwise, if it isn't
			// here, it's most likely another console's.
			if err := r.pendingKey(ctx, token, h.Kid, now); err != nil {
				return App{}, k, err
			}
			return App{}, k, r.unknownKey()
		}
		// An old key the previous revision made during a deploy has no
		// kid yet (fillKID writes it): found by its app, below.
	}
	return r.signerByIssuer(ctx, token, h.Kid, claims.Issuer)
}

// signerByKID is signer for the key whose kid is kid, across every app:
// found is false if there's none (not revoked).
func (r *Registry) signerByKID(ctx context.Context, token, kid string) (App, activeKey, bool, error) {
	k, ok, err := r.keyByKID(ctx, kid)
	if err != nil {
		return App{}, k, false, fmt.Errorf("apps: reading a key: %w", err)
	}
	if !ok {
		return App{}, k, false, nil
	}
	if !signedBy(token, k) {
		return App{}, k, true, refusal("invalid assertion")
	}
	app, err := r.Get(ctx, k.app)
	if errors.Is(err, ErrNotFound) {
		return App{}, k, true, refusal("invalid assertion")
	}
	if err != nil {
		return App{}, k, true, fmt.Errorf("apps: reading the app: %w", err)
	}
	return app, k, true, nil
}

// signerByIssuer is signer for app id's keys: any that isn't revoked, or
// with a kid, only the key it names.
func (r *Registry) signerByIssuer(ctx context.Context, token, kid, id string) (App, activeKey, error) {
	app, err := r.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return App{}, activeKey{}, refusal("invalid assertion")
	}
	if err != nil {
		return App{}, activeKey{}, fmt.Errorf("apps: reading the app: %w", err)
	}
	keys, err := r.activeKeys(ctx, app.ID)
	if err != nil {
		return App{}, activeKey{}, fmt.Errorf("apps: reading the app's keys: %w", err)
	}
	for _, k := range keys { // a deleted app has none
		if (kid == "" || kid == appkey.KeyID(k.public)) && signedBy(token, k) {
			return app, k, nil
		}
	}
	return App{}, activeKey{}, refusal("invalid assertion")
}

// signedBy reports whether k signed token (an app+jwt).
func signedBy(token string, k activeKey) bool {
	_, err := jws.Verify(token, "app+jwt", func(jws.Header) ed25519.PublicKey { return k.public }, &Assertion{})
	return err == nil
}

// unknownKey refuses an assertion whose kid, the only thing naming its
// app, isn't any key here: a key from another console (staging's, say)
// sent to this one, since a new key defaults to production. It reveals
// nothing about what keys exist.
func (r *Registry) unknownKey() error {
	host := r.Issuer
	if u, err := url.Parse(r.Issuer); err == nil && u.Host != "" {
		host = u.Host
	}
	return refusal("invalid assertion: this key isn't one of " + host + "'s; if it came from another console, set WAKESHIFT_CORE to its address")
}

// checkClaims refuses an assertion for what its header and claims show
// without its key: not an app+jwt signed with EdDSA, a signature that
// can't be one, an issuer that isn't its subject (Authenticate wants both
// the app or both the kid), another audience, expired or too long-lived,
// or no usable jti.
func (r *Registry) checkClaims(h jws.Header, a Assertion, now time.Time) error {
	switch {
	case h.Alg != "EdDSA" || h.Typ != "app+jwt":
		return refusal("invalid assertion")
	case a.Issuer != a.Subject:
		return refusal("assertion issuer and subject must be the app")
	case a.Audience != r.AssertionAudience():
		return refusal("assertion is for another audience")
	case now.Unix() >= a.Expires || a.Expires-a.IssuedAt > int64(maxAssertionLife.Seconds()) || a.IssuedAt > now.Add(time.Minute).Unix():
		return refusal("assertion expired or too long-lived")
	case a.ID == "":
		return refusal("assertion needs a jti")
	case len(a.ID) > 200:
		return refusal("assertion's jti is too long")
	}
	return nil
}

// unverified reads a token's header and claims without verifying them, and
// refuses one whose signature isn't an Ed25519 signature's size.
func unverified(token string) (jws.Header, Assertion, bool) {
	var h jws.Header
	var a Assertion
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return h, a, false
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(hb, &h) != nil || len(h.Kid) > 100 {
		return h, a, false
	}
	if sig, err := base64.RawURLEncoding.DecodeString(parts[2]); err != nil || len(sig) != ed25519.SignatureSize {
		return h, a, false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	return h, a, err == nil && json.Unmarshal(b, &a) == nil && a.Issuer != ""
}

// firstUse records an assertion ID until the assertion expires, reporting
// whether it was new. It's in the database, so every instance refuses a
// replay; the expiry pass deletes expired IDs (DeleteExpiredAssertionIDs).
func (r *Registry) firstUse(ctx context.Context, app, id string, exp time.Time) (bool, error) {
	tag, err := r.Pool.Exec(ctx, `INSERT INTO app_assertion_ids (app_id, jti, expires_at) VALUES ($1, $2, $3)
		ON CONFLICT (app_id, jti) DO NOTHING`, app, id, exp)
	return tag.RowsAffected() == 1, err
}

// DeleteExpiredAssertionIDs forgets assertion IDs whose assertions have
// expired (they're refused as expired anyway), at most limit of them.
func DeleteExpiredAssertionIDs(ctx context.Context, pool *pgxpool.Pool, limit int) (int64, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM app_assertion_ids WHERE ctid IN
		(SELECT ctid FROM app_assertion_ids WHERE expires_at < now() - interval '1 minute' LIMIT $1)`, limit)
	return tag.RowsAffected(), err
}

// ValidID says whether id is a valid app ID.
func ValidID(id string) bool { return idRE.MatchString(id) }
