package console_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

type logEntry struct {
	ID     int64
	Kind   string
	Actor  string
	Email  string `json:"actor_email"`
	Action string
	KeyID  string `json:"key_id"`
	Detail map[string]any
}

func appLog(t *testing.T, u *coretest.User, id, query string) (int, []logEntry) {
	t.Helper()
	var page struct{ Entries []logEntry }
	code := u.Do("GET", "/api/v1/developer/apps/"+id+"/log"+query, nil, &page)
	return code, page.Entries
}

func actions(list []logEntry) string {
	var out []string
	for _, e := range list {
		out = append(out, e.Action)
	}
	return strings.Join(out, " ")
}

// What happens to an app is in its own log, newest first: who did it, and
// which key, never the key. Republishing an unchanged manifest isn't
// logged; a key's first use is, once per key. Workspaces' audit logs have
// none of it.
func TestAppLog(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	made := createApp(t, ada, "quizzer", "Quizzer", "")
	app := kit(t, c, made.Key, "")
	for range 2 {
		if err := publish(app); err != nil { // the same manifest, twice
			t.Fatal(err)
		}
	}
	other := []wakeshift.ScopeEvent{{Pattern: "app.quizzer.lobby", Description: "The lobby"}}
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Agent: &wakeshift.AgentScope{Wake: other, Post: quizScope, TTLDays: 60}}); err != nil {
		t.Fatal(err)
	}
	rotated := rotate(t, ada, "quizzer", made.KeyInfo.ID)
	app = kit(t, c, rotated.Key, "")
	ws, err := app.Workspace(ctx, "player-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.AddAgent(ctx, ws, "claude-code", ""); err != nil {
		t.Fatal(err)
	}
	ada.Do("DELETE", "/api/v1/developer/apps/quizzer", nil, nil)

	code, list := appLog(t, ada, "quizzer", "")
	want := "app.grants_revoked app.deleted app.key_first_used app.key_rotated app.manifest_published app.manifest_published app.key_first_used app.created"
	if code != 200 || actions(list) != want {
		t.Fatalf("log %d: %s", code, actions(list))
	}
	revoked, deleted, firstUse2, rot, changed, published, firstUse, created := list[0], list[1], list[2], list[3], list[4], list[5], list[6], list[7]
	if created.Actor == "" || created.Email != "ada@example.com" || created.KeyID != made.KeyInfo.ID || created.Kind != "audit" || created.Detail["name"] != "Quizzer" {
		t.Errorf("created: %+v", created)
	}
	if firstUse.Actor != "app:quizzer" || firstUse.KeyID != made.KeyInfo.ID {
		t.Errorf("first use: %+v", firstUse)
	}
	if published.Detail["first"] != true || published.KeyID != made.KeyInfo.ID {
		t.Errorf("first manifest: %+v", published)
	}
	if fmt.Sprint(changed.Detail["changes"]) != "[wake + app.quizzer.lobby wake - app.quizzer.game.* agent ttl_days: 30 -> 60]" || changed.Detail["first"] != false {
		t.Errorf("changed manifest: %+v", changed.Detail)
	}
	if rot.KeyID != rotated.KeyInfo.ID || rot.Detail["old_key_id"] != made.KeyInfo.ID || rot.Email != "ada@example.com" {
		t.Errorf("rotated: %+v", rot)
	}
	if firstUse2.KeyID != rotated.KeyInfo.ID {
		t.Errorf("the new key's first use: %+v", firstUse2)
	}
	if fmt.Sprint(deleted.Detail["keys_revoked"]) != "["+rotated.KeyInfo.ID+"]" || deleted.Email != "ada@example.com" {
		t.Errorf("deleted: %+v", deleted)
	}
	if revoked.Detail["grants"] != 1.0 || revoked.Detail["workspaces"] != 1.0 || revoked.Detail["complete"] != true {
		t.Errorf("grants revoked: %+v", revoked.Detail)
	}
	// A deleted app is kept without its keys, as the privacy policy
	// says: the revoked one and the live one alike. Its log still names
	// them.
	var keys int
	if err := c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM app_keys WHERE app_id = 'quizzer'`).Scan(&keys); err != nil || keys != 0 {
		t.Errorf("the deleted app keeps %d keys (%v)", keys, err)
	}

	var raw json.RawMessage
	ada.Do("GET", "/api/v1/developer/apps/quizzer/log", nil, &raw)
	if strings.Contains(string(raw), "wst_") {
		t.Fatal("the log shows a key")
	}
	var audit struct{ Entries []auditEntry }
	ada.Do("GET", "/api/v1/audit", nil, &audit)
	for _, e := range audit.Entries {
		if strings.HasPrefix(e.Action, "app.") {
			t.Errorf("the workspace's audit log has %s", e.Action)
		}
	}
}

// Only the app's owner reads its log, deleted or not; anyone else, and an
// app that doesn't exist, are 404.
func TestAppLogIsTheOwners(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	createApp(t, ada, "quizzer", "Quizzer", "")
	c.RegisterApp("agentchat", "Agent Chat", "") // ours: no owner
	bob := c.SignIn("bob@example.com")
	for _, id := range []string{"quizzer", "agentchat", "nope"} {
		if code, list := appLog(t, bob, id, ""); code != 404 || len(list) != 0 {
			t.Errorf("bob reading %s: %d %v", id, code, list)
		}
	}
	ada.Do("DELETE", "/api/v1/developer/apps/quizzer", nil, nil)
	if code, list := appLog(t, ada, "quizzer", ""); code != 200 || actions(list) != "app.deleted app.created" {
		t.Fatalf("ada reading her deleted app's log: %d %s", code, actions(list))
	}
	if code, _ := appLog(t, bob, "quizzer", ""); code != 404 {
		t.Fatalf("bob reading the deleted app's log: %d", code)
	}
}

// The log pages newest first by entry ID.
func TestAppLogPages(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	key := createApp(t, ada, "quizzer", "Quizzer", "").KeyInfo.ID
	for range 4 {
		key = rotate(t, ada, "quizzer", key).KeyInfo.ID
	}
	_, all := appLog(t, ada, "quizzer", "")
	if len(all) != 5 {
		t.Fatalf("log: %s", actions(all))
	}
	var got []logEntry
	query := "?limit=2"
	for {
		code, page := appLog(t, ada, "quizzer", query)
		if code != 200 || len(page) > 2 {
			t.Fatalf("page %s: %d %d", query, code, len(page))
		}
		got = append(got, page...)
		if len(page) < 2 {
			break
		}
		query = fmt.Sprintf("?limit=2&before=%d", page[len(page)-1].ID)
	}
	if len(got) != 5 {
		t.Fatalf("paged %d entries", len(got))
	}
	for i := range got {
		if got[i].ID != all[i].ID || i > 0 && got[i].ID >= got[i-1].ID {
			t.Fatalf("pages out of order: %v", got)
		}
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?before=x", "?before=-1"} {
		if code, _ := appLog(t, ada, "quizzer", q); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
}

// An entry exists exactly when its action committed: an action that fails
// leaves none.
func TestAppLogOnlyWhatCommitted(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	made := createApp(t, ada, "quizzer", "Quizzer", "")
	app := kit(t, c, made.Key, "")
	bob := c.SignIn("bob@example.com")

	// Refused: taking the ID, someone else rotating or deleting it, and a
	// manifest that isn't valid.
	bob.Do("POST", "/api/v1/developer/apps", map[string]string{"id": "quizzer", "name": "Mine"}, nil)
	bob.Do("POST", "/api/v1/developer/apps/quizzer/keys/"+made.KeyInfo.ID+"/rotate", nil, nil)
	bob.Do("DELETE", "/api/v1/developer/apps/quizzer", nil, nil)
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Agent: &wakeshift.AgentScope{Wake: quizScope, TTLDays: 1000}}); !wakeshift.IsCode(err, "bad_manifest") {
		t.Fatalf("a refused manifest: %v", err)
	}
	// That request did sign with the key, so its first use is logged.
	if _, list := appLog(t, ada, "quizzer", ""); actions(list) != "app.key_first_used app.created" {
		t.Fatalf("after refused actions: %s", actions(list))
	}

	// Each change and its entry are one transaction: rolled back, neither
	// is there.
	user := ""
	c.DB.Pool.QueryRow(ctx, `SELECT created_by::text FROM apps WHERE id = 'quizzer'`).Scan(&user)
	boom := errors.New("boom")
	steps := map[string]func(tx pgx.Tx) error{
		"create": func(tx pgx.Tx) error {
			_, _, _, err := c.Apps.Create(ctx, tx, user, "", apps.NewApp{ID: "rolled-back", Name: "R"})
			return err
		},
		"rotate": func(tx pgx.Tx) error {
			_, _, err := c.Apps.RotateKey(ctx, tx, user, "quizzer", made.KeyInfo.ID)
			return err
		},
		"new key": func(tx pgx.Tx) error { _, _, err := c.Apps.CreateKey(ctx, tx, user, "quizzer", "CI"); return err },
		"revoke": func(tx pgx.Tx) error {
			_, err := c.Apps.RevokeKey(ctx, tx, user, "quizzer", made.KeyInfo.ID)
			return err
		},
		"delete": func(tx pgx.Tx) error { _, _, err := c.Apps.Delete(ctx, tx, user, "quizzer"); return err },
	}
	for name, step := range steps {
		err := pgx.BeginFunc(ctx, c.DB.Pool, func(tx pgx.Tx) error {
			if err := step(tx); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var n int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM app_log WHERE app_id = 'rolled-back'`).Scan(&n)
	if _, list := appLog(t, ada, "quizzer", ""); n != 0 || actions(list) != "app.key_first_used app.created" {
		t.Fatalf("after rolled-back changes: %d, %s", n, actions(list))
	}
	if err := publish(app); err != nil {
		t.Fatalf("the key after a rolled-back rotation: %v", err)
	}
}
