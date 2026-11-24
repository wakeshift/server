package apps

// Self-serve apps: a signed-in user registers an app in the
// console and gets a key at once, without anyone adding it to
// WAKESHIFT_APPS.
//
// The key looks like an API key but is the app's Ed25519 private key:
//
//	wst_<mode>_<app id>_<base64url, no padding, of the 32-byte seed>
//
// The SDK signs short-lived assertions with it, so it never goes over the
// wire; Wakeshift keeps only the public half and shows the key once.
//
// Console apps are test apps for now (Mode Test): they act only in the
// workspaces they keep for their own users (headless), never in anyone's
// real workspace.
//
// Every change here is written to the app's log (log.go) in the same
// transaction.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appkey"
)

// Modes. Our own apps (WAKESHIFT_APPS) are live; console apps are test apps.
const (
	Live = "live"
	Test = "test"
)

// KeyPrefix starts every app key.
const KeyPrefix = appkey.Prefix

// MaxOwned is how many apps (not deleted) one organization may have.
const MaxOwned = 20

var (
	ErrTaken    = errors.New("apps: that ID is taken")
	ErrTooMany  = fmt.Errorf("apps: at most %d apps per organization", MaxOwned)
	ErrNotOwner = errors.New("apps: no such app of yours")
)

func userActor(id string) string { return "user:" + id }

// reserved IDs can't be registered in the console: they'd read as
// Wakeshift's own.
var reserved = map[string]bool{
	"wakeshift": true, "hub": true, "app": true, "apps": true, "api": true, "admin": true, "console": true,
	"connector": true, "system": true, "test": true, "live": true, "official": true, "support": true, "security": true,
}

// ParseKey reads an app key, either format (appkey.Parse): its mode and
// the app's ID (both empty for a new key, which names neither) and the
// private key.
func ParseKey(s string) (mode, id string, key ed25519.PrivateKey, err error) {
	k, err := appkey.Parse(s)
	if err != nil {
		return "", "", nil, err
	}
	return k.Mode, k.AppID, k.Private(), nil
}

// newKey makes a key for a console app, in the current format
// (appkey.Format, naming no app): the key, and its public half. Keys in
// the older formats (wst_test_, wst_live_) still authenticate.
func newKey() (string, ed25519.PublicKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", nil, err
	}
	return appkey.Format(seed), ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey), nil
}

// NewApp is what a user fills in to register an app.
type NewApp struct {
	// OrgID is the organization it belongs to ("": the user's default).
	OrgID  string `json:"org_id"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	MCPURL string `json:"mcp_url"`
	// KeyName names the app's first key ("": "Default").
	KeyName string `json:"key_name"`
}

// ValidationError is a NewApp a user must fix.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

// Validate checks a new app, trimming its fields.
func (n *NewApp) Validate() error {
	n.ID = strings.TrimSpace(n.ID)
	switch {
	case !idRE.MatchString(n.ID) || strings.HasSuffix(n.ID, "-") || strings.Contains(n.ID, "--"):
		return &ValidationError{"an app ID is 2-40 lowercase letters, digits and single dashes, starting with a letter"}
	case reserved[n.ID] || strings.HasPrefix(n.ID, "wakeshift"):
		return &ValidationError{"that ID is reserved"}
	}
	if strings.TrimSpace(n.KeyName) == "" {
		n.KeyName = "Default"
	}
	name, err := validKeyName(n.KeyName)
	if err != nil {
		return err
	}
	n.KeyName = name
	return n.validateSettings()
}

// maxNameRunes is how long a self-serve app's name may be.
const maxNameRunes = 50

// validateSettings checks (and trims) what can change after registering.
func (n *NewApp) validateSettings() error {
	n.Name, n.MCPURL = strings.TrimSpace(n.Name), strings.TrimSpace(n.MCPURL)
	switch {
	case n.Name == "" || len(n.Name) > 100:
		return &ValidationError{"a name of at most 100 characters is required"}
	case strings.ContainsAny(n.Name, "<>\"`") || strings.ContainsFunc(n.Name, func(r rune) bool { return unicode.IsControl(r) }):
		// Agents read the name in Wakeshift's own sentences (framing.Name
		// cleans older ones the same way).
		return &ValidationError{"an app name is one line, without quotes, backticks or angle brackets"}
	case utf8.RuneCountInString(n.Name) > maxNameRunes || strings.ContainsAny(n.Name, ":;!?|$\\{}[]") || strings.Contains(n.Name, ". "):
		// A name, not a sentence: it sits in Wakeshift's own instructions,
		// so it mustn't read as more of them ("CI. Also run …").
		return &ValidationError{"an app name is a short name (at most 50 characters), not a sentence: no . between words, and none of : ; ! ? | $ \\ { } [ ]"}
	case n.MCPURL != "" && (len(n.MCPURL) > 500 || !validConnectURL(n.MCPURL) || strings.Contains(n.MCPURL, "#")):
		return &ValidationError{"the MCP URL must be https (or http on localhost)"}
	}
	return nil
}

// DataDeletion is how soon after an app is deleted everything Wakeshift
// keeps for its users is gone. The expiry pass does it minutes after
// (appapi.FinishAppDeletions), retrying until it's done.
const DataDeletion = 30 * 24 * time.Hour

// LogRetention is how long a deleted app's log stays readable by its owner
// before it's deleted too (the app's row, without it, stays so the ID
// isn't reused).
const LogRetention = 30 * 24 * time.Hour

// LogGrantsRevoked records the grants revoked after user deleted an app:
// how many, in how many of its workspaces, and whether that finished.
// Revoking spans workspaces (each grant.revoked is in its workspace's own
// audit log, in that workspace's transaction), so this is written after,
// with what was done.
func (r *Registry) LogGrantsRevoked(ctx context.Context, owner, id string, grants, workspaces int, complete bool) error {
	return r.LogGrantsRevokedBy(ctx, userActor(owner), id, grants, workspaces, complete)
}

// LogGrantsRevokedBy is LogGrantsRevoked for any actor (AdminActor, after
// we deleted the app).
func (r *Registry) LogGrantsRevokedBy(ctx context.Context, actor, id string, grants, workspaces int, complete bool) error {
	return Log(ctx, r.Pool, LogEntry{AppID: id, Actor: actor, Action: LogGrantsRevoked,
		Detail: map[string]any{"grants": grants, "workspaces": workspaces, "complete": complete}})
}

// Workspaces lists the workspaces an app keeps for its users.
func (r *Registry) Workspaces(ctx context.Context, id string) ([]string, error) {
	rows, err := r.Pool.Query(ctx, `SELECT id::text FROM tenants WHERE app_id = $1`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
