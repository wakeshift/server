package grants_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agents"
	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/audit"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/events"
	"github.com/wakeshift/server/internal/grants"
	"github.com/wakeshift/server/internal/streams"
	"github.com/wakeshift/server/internal/subscriptions"
)

var ctx = context.Background()

type fixture struct {
	t      *testing.T
	d      *db.DB
	tenant string
	user   string
	chat   apps.App
}

func setup(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.New(t)
	r := &apps.Registry{Pool: d.Pool}
	key := func() string {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		return base64.StdEncoding.EncodeToString(pub)
	}
	if err := r.Sync(ctx, []apps.Config{
		{ID: "agentchat", Name: "Agent Chat", MCPURL: "https://chat.test/mcp", PublicKey: key()},
		{ID: "sources", Name: "Sources", MCPURL: "https://sources.test/mcp", PublicKey: key()},
	}); err != nil {
		t.Fatal(err)
	}
	chatScope := []apps.ScopeEvent{{Pattern: "app.agentchat.channel.*"}}
	if err := r.SetManifest(ctx, "agentchat", apps.Manifest{Agent: &apps.AgentScope{Wake: chatScope, Post: chatScope, TTLDays: 90}}); err != nil {
		t.Fatal(err)
	}
	// Agent Chat's grants are in a workspace it keeps (the only kind an app
	// gets: Create).
	f := &fixture{t: t, d: d}
	if err := d.Pool.QueryRow(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('test', 'app', 'agentchat', 'ada') RETURNING id`).Scan(&f.tenant); err != nil {
		t.Fatal(err)
	}
	f.chat, _ = r.Get(ctx, "agentchat")
	d.Pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('ada@example.com') RETURNING id`).Scan(&f.user)
	f.tx(func(tx pgx.Tx) error {
		for _, a := range []string{"claude-code", "codex"} {
			if _, err := agents.Create(ctx, tx, a, ""); err != nil {
				return err
			}
		}
		return nil
	})
	return f
}

func (f *fixture) tx(fn func(tx pgx.Tx) error) {
	f.t.Helper()
	if err := f.d.InTenant(ctx, f.tenant, fn); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) grant(app apps.App, agent string) grants.Grant {
	f.t.Helper()
	var g grants.Grant
	f.tx(func(tx pgx.Tx) (err error) {
		g, err = grants.Create(ctx, tx, grants.New{App: app, AgentID: agent})
		return err
	})
	return g
}

func titles(risks []grants.Risk) string {
	var out []string
	for _, r := range risks {
		out = append(out, r.Title+": "+r.Detail)
	}
	return strings.Join(out, "\n")
}

// The consent's risks: a peer on the same app can post where the agent
// listens, and messages can lead it to act.
func TestConsentShowsPeers(t *testing.T) {
	f := setup(t)
	f.grant(f.chat, "codex")
	var risks []grants.Risk
	f.tx(func(tx pgx.Tx) (err error) {
		risks, _, err = grants.AgentConsent(ctx, tx, f.chat, "claude-code")
		return err
	})
	text := titles(risks)
	for _, want := range []string{"Other agents can wake claude-code", "codex also use Agent Chat", "can run commands"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestDryRunCountsRecentEvents(t *testing.T) {
	f := setup(t)
	f.tx(func(tx pgx.Tx) error {
		for _, typ := range []string{"app.agentchat.channel.general", "app.agentchat.channel.pr-1", "app.sources.slack.message"} {
			if _, _, err := events.Append(ctx, tx, events.Event{Type: typ, Source: "test", From: events.Party{Kind: "hub", ID: "hub"}, Trust: events.Internal}); err != nil {
				return err
			}
		}
		return nil
	})
	var dry grants.DryRun
	f.tx(func(tx pgx.Tx) (err error) {
		_, dry, err = grants.AgentConsent(ctx, tx, f.chat, "claude-code")
		return err
	})
	if dry.Events != 2 || dry.Days != 7 {
		t.Fatalf("dry run %+v", dry)
	}
}

func TestOneLiveGrantPerAppAndAgent(t *testing.T) {
	f := setup(t)
	f.grant(f.chat, "claude-code")
	err := f.d.InTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		_, err := grants.Create(ctx, tx, grants.New{App: f.chat, AgentID: "claude-code"})
		return err
	})
	if err != grants.ErrExists {
		t.Fatalf("second live grant: %v", err)
	}
}

// Revoking a grant undoes what was created under it.
func TestRevokeCascades(t *testing.T) {
	f := setup(t)
	g := f.grant(f.chat, "claude-code")
	var sub subscriptions.Subscription
	f.tx(func(tx pgx.Tx) (err error) {
		sub, err = subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "app.agentchat.channel.general", CreatedBy: "app:agentchat", GrantID: g.ID})
		if err != nil {
			return err
		}
		s := &streams.Server{Key: []byte(strings.Repeat("k", 32)), PublicURL: "https://wakeshift.test"}
		_, _, err = s.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: "claude-code", CreatedBy: "app:agentchat", GrantID: g.ID, TTL: time.Hour})
		return err
	})
	f.tx(func(tx pgx.Tx) error {
		_, err := grants.Revoke(ctx, tx, g.ID, "user:test")
		return err
	})
	f.tx(func(tx pgx.Tx) error {
		s, _ := subscriptions.Get(ctx, tx, sub.ID)
		if s.Status != subscriptions.Canceled {
			t.Errorf("subscription %s", s.Status)
		}
		var live int
		tx.QueryRow(ctx, `SELECT count(*) FROM stream_links WHERE revoked_at IS NULL`).Scan(&live)
		if live != 0 {
			t.Errorf("%d links still live", live)
		}
		if _, err := grants.Live(ctx, tx, "agentchat", "claude-code"); err != grants.ErrNotFound {
			t.Errorf("grant still live: %v", err)
		}
		return nil
	})
}

// An expired grant gives way to a new consent, and the sweep revokes it
// (with what it created) once it's past its date.
func TestExpiredGrants(t *testing.T) {
	f := setup(t)
	g := f.grant(f.chat, "claude-code")
	expire := func(id string) {
		f.tx(func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE grants SET expires_at = now() - interval '1 minute' WHERE id = $1`, id)
			return err
		})
	}
	expire(g.ID)
	var again grants.Grant
	f.tx(func(tx pgx.Tx) (err error) {
		again, _, err = grants.Ensure(ctx, tx, grants.New{App: f.chat, AgentID: "claude-code"})
		return err
	})
	if again.ID == "" || again.ID == g.ID {
		t.Fatalf("re-consent after expiry: %+v", again)
	}

	var sub subscriptions.Subscription
	f.tx(func(tx pgx.Tx) (err error) {
		sub, err = subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "app.agentchat.channel.general", CreatedBy: "app:agentchat", GrantID: again.ID})
		return err
	})
	expire(again.ID)
	f.tx(func(tx pgx.Tx) error {
		n, err := grants.RevokeExpired(ctx, tx)
		if n != 1 {
			t.Errorf("expired %d", n)
		}
		s, _ := subscriptions.Get(ctx, tx, sub.ID)
		if s.Status != subscriptions.Canceled {
			t.Errorf("subscription of an expired grant: %s", s.Status)
		}
		return err
	})
}

// Revoking an already-revoked grant (a stale row in the UI) leaves the
// agent's newer grant and what it made alone.
func TestRevokingAnOldGrantAgain(t *testing.T) {
	f := setup(t)
	old := f.grant(f.chat, "claude-code")
	f.tx(func(tx pgx.Tx) error { _, err := grants.Revoke(ctx, tx, old.ID, "user:test"); return err })
	g := f.grant(f.chat, "claude-code")
	var sub subscriptions.Subscription
	f.tx(func(tx pgx.Tx) (err error) {
		sub, err = subscriptions.Create(ctx, tx, subscriptions.New{AgentID: "claude-code", EventType: "app.agentchat.channel.general", CreatedBy: "app:agentchat", GrantID: g.ID})
		return err
	})
	f.tx(func(tx pgx.Tx) error {
		if _, err := grants.Revoke(ctx, tx, old.ID, "user:test"); err != nil {
			return err
		}
		if s, _ := subscriptions.Get(ctx, tx, sub.ID); s.Status != subscriptions.Active {
			t.Errorf("revoking the old grant again ended the new one's subscription: %s", s.Status)
		}
		if _, err := grants.Live(ctx, tx, "agentchat", "claude-code"); err != nil {
			t.Errorf("the new grant: %v", err)
		}
		return nil
	})
}

func (f *fixture) audited() []audit.Item {
	f.t.Helper()
	var out []audit.Item
	f.tx(func(tx pgx.Tx) (err error) {
		out, err = audit.List(ctx, tx, 0, audit.MaxLimit)
		slices.Reverse(out) // oldest first
		return err
	})
	return out
}

// Consents, revocations and expiries are audited, in the transaction that
// makes them: a consent that rolls back leaves no entry.
func TestGrantsAreAudited(t *testing.T) {
	f := setup(t)
	shown := grants.Shown{Risks: []grants.Risk{{Level: "caution", Title: "Can run commands"}}, DryRun: grants.DryRun{Days: 7, Events: 3}}
	var g grants.Grant
	f.tx(func(tx pgx.Tx) (err error) {
		g, err = grants.Create(ctx, tx, grants.New{App: f.chat, AgentID: "claude-code", Shown: shown})
		return err
	})
	f.tx(func(tx pgx.Tx) (err error) {
		_, err = grants.Revoke(ctx, tx, g.ID, audit.User(f.user))
		return err
	})
	f.tx(func(tx pgx.Tx) (err error) { // again: nothing more happens, nothing more recorded
		_, err = grants.Revoke(ctx, tx, g.ID, audit.User(f.user))
		return err
	})
	failed := errors.New("rolled back")
	if err := f.d.InTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		if _, err := grants.Create(ctx, tx, grants.New{App: f.chat, AgentID: "codex"}); err != nil {
			return err
		}
		return failed
	}); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	expiring := f.grant(f.chat, "codex")
	f.tx(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE grants SET expires_at = now() WHERE id = $1`, expiring.ID); err != nil {
			return err
		}
		_, err := grants.RevokeExpired(ctx, tx)
		return err
	})

	got := f.audited()
	var lines []string
	for _, it := range got {
		lines = append(lines, it.Action+" "+it.Actor+" "+it.AppID+"/"+it.AgentID)
	}
	want := []string{
		"grant.created app:agentchat agentchat/claude-code",
		"grant.revoked user:" + f.user + " agentchat/claude-code",
		"grant.created app:agentchat agentchat/codex",
		"grant.expired hub agentchat/codex",
	}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("audit log:\n%s", strings.Join(lines, "\n"))
	}
	// The consent entry points at what the screen showed (kept once, in the
	// grant); the revocation says what it undid.
	var recorded grants.Shown
	json.Unmarshal(got[0].Shown, &recorded)
	if got[0].GrantID != g.ID || !reflect.DeepEqual(recorded, shown) || strings.Contains(string(got[0].Detail), "Can run commands") {
		t.Fatalf("grant.created: %+v (shown %s)", got[0], got[0].Shown)
	}
	var undid map[string]any
	json.Unmarshal(got[1].Detail, &undid)
	if undid["subscriptions_canceled"] != float64(0) || got[1].Shown != nil {
		t.Fatalf("grant.revoked: %s", got[1].Detail)
	}
}

// An app gets grants only in a workspace it keeps: never in a person's own
// (account mode is turned off), even ours, and never in another app's.
func TestGrantsOnlyInTheAppsOwnWorkspaces(t *testing.T) {
	f := setup(t)
	var other string
	if err := f.d.Pool.QueryRow(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('other', 'app', 'sources', 'ada') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	for name, tenant := range map[string]string{"a person's own": dbtest.NewTenant(t, f.d), "another app's": other} {
		err := f.d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			if _, err := agents.Create(ctx, tx, "claude-code", ""); err != nil {
				return err
			}
			_, err := grants.Create(ctx, tx, grants.New{App: f.chat, AgentID: "claude-code"})
			return err
		})
		if !errors.Is(err, grants.ErrWrongMode) {
			t.Errorf("%s workspace: %v", name, err)
		}
		f.d.InTenant(ctx, tenant, func(tx pgx.Tx) error {
			if list, _ := grants.List(ctx, tx); len(list) != 0 {
				t.Errorf("%s workspace has %d grants", name, len(list))
			}
			return nil
		})
	}
	if g := f.grant(f.chat, "claude-code"); g.ID == "" {
		t.Fatal("no grant in its own workspace")
	}
}

// OthersCanWake keeps the disclosure groups gave once a manifest names no
// types to post to: agents of people you don't know can wake yours, and
// the peers line. Without it (and without Post or Groups), neither shows.
func TestConsentOthersCanWake(t *testing.T) {
	f := setup(t)
	f.grant(f.chat, "codex")
	consent := func(m apps.Manifest) string {
		t.Helper()
		app := f.chat
		app.Manifest = m
		var risks []grants.Risk
		f.tx(func(tx pgx.Tx) (err error) {
			risks, _, err = grants.AgentConsent(ctx, tx, app, "claude-code")
			return err
		})
		return titles(risks)
	}
	scope := &apps.AgentScope{TTLDays: 90}
	with := consent(apps.Manifest{Agent: scope, OthersCanWake: true})
	for _, want := range []string{"Agents of people you don't know can wake claude-code", "passes on to claude-code", "Other agents can wake claude-code", "codex also use Agent Chat"} {
		if !strings.Contains(with, want) {
			t.Errorf("with OthersCanWake, missing %q in:\n%s", want, with)
		}
	}
	if strings.Contains(with, "groups") {
		t.Errorf("OthersCanWake's text mentions groups:\n%s", with)
	}
	without := consent(apps.Manifest{Agent: scope})
	for _, not := range []string{"people you don't know", "Other agents can wake"} {
		if strings.Contains(without, not) {
			t.Errorf("without OthersCanWake, %q in:\n%s", not, without)
		}
	}
}

// The expiry pass revokes at most 200 expired grants at a time, the longest
// expired first, so its transaction stays bounded; the next pass takes the
// rest.
func TestRevokeExpiredInBatches(t *testing.T) {
	f := setup(t)
	f.tx(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) SELECT current_tenant(), 'agent-' || i FROM generate_series(0, 200) i`); err != nil {
			return err
		}
		for i := range 201 {
			dbtest.Grant(t, tx, "agentchat", fmt.Sprintf("agent-%d", i), -time.Duration(i+1)*time.Minute)
		}
		for _, want := range []int{200, 1, 0} {
			n, err := grants.RevokeExpired(ctx, tx)
			if err != nil {
				return err
			}
			if n != want {
				t.Fatalf("revoked %d, want %d", n, want)
			}
		}
		return nil
	})
}
