package console_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appkey"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

type keyInfo struct {
	ID         string `json:"key_id"`
	Name       string
	LastUsedAt *string `json:"last_used_at"`
	RevokedAt  *string `json:"revoked_at"`
}

func listKeys(t *testing.T, u *coretest.User, app string) []keyInfo {
	t.Helper()
	var out struct{ Keys []keyInfo }
	if code := u.Do("GET", "/api/v1/developer/apps/"+app+"/keys", nil, &out); code != 200 {
		t.Fatalf("keys: %d", code)
	}
	return out.Keys
}

// An app has several named keys. Each works on its own; revoking one stops
// it at once and leaves the others working. A key never authenticates as
// another app. Making and revoking keys is in the app's log, and keys are
// never listed.
func TestAppKeys(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	made := createApp(t, ada, "quizzer", "Quizzer", "")
	other := createApp(t, ada, "other-app", "Other", "")

	var ci madeApp
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/keys", map[string]string{"name": "CI"}, &ci); code != 200 || ci.KeyInfo.Name != "CI" || !currentKey(ci.Key) {
		t.Fatalf("new key: %d %+v", code, ci.KeyInfo)
	}
	first, second := kit(t, c, made.Key, ""), kit(t, c, ci.Key, "")
	for name, app := range map[string]*wakeshift.Client{"the first key": first, "the second key": second} {
		if err := publish(app); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	keys := listKeys(t, ada, "quizzer")
	if len(keys) != 2 || keys[0].Name != "CI" || keys[1].Name != "Default" || keys[0].LastUsedAt == nil {
		t.Fatalf("keys: %+v", keys)
	}
	var raw json.RawMessage
	ada.Do("GET", "/api/v1/developer/apps/quizzer/keys", nil, &raw)
	if strings.Contains(string(raw), "wst_") {
		t.Fatal("the key list shows a key")
	}

	// Another app's key, claiming to be this app, is refused.
	otherKey, _ := appkey.Parse(other.Key)
	if err := publish(coretest.SDKClient(t, "quizzer", otherKey.Private(), c.URL)); !wakeshift.IsCode(err, "unauthenticated") {
		t.Fatalf("another app's key: %v", err)
	}

	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/keys/"+made.KeyInfo.ID+"/revoke", nil, nil); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if err := publish(first); !wakeshift.IsCode(err, "unauthenticated") {
		t.Fatalf("a revoked key: %v", err)
	}
	if err := publish(second); err != nil {
		t.Fatalf("the other key after a revoke: %v", err)
	}
	// Revoking again changes nothing; a made-up key is 404.
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/keys/"+made.KeyInfo.ID+"/revoke", nil, nil); code != 200 {
		t.Fatalf("revoke again: %d", code)
	}
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/keys/nope/revoke", nil, nil); code != 404 {
		t.Fatalf("revoke a made-up key: %d", code)
	}
	// Another app's key ID isn't this app's.
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/keys/"+other.KeyInfo.ID+"/revoke", nil, nil); code != 404 {
		t.Fatalf("revoke another app's key here: %d", code)
	}
	if err := kit(t, c, other.Key, "").PublishManifest(context.Background(), wakeshift.Manifest{Description: "Other."}); err != nil {
		t.Fatalf("the other app's key: %v", err)
	}

	_, list := appLog(t, ada, "quizzer", "?action=app.key_")
	if actions(list) != "app.key_revoked app.key_first_used app.key_first_used app.key_created" || list[0].KeyID != made.KeyInfo.ID || list[3].Detail["name"] != "CI" {
		t.Fatalf("key log: %s", actions(list))
	}

	// At most 10 keys that aren't revoked.
	for i := 0; ; i++ {
		code := ada.Do("POST", "/api/v1/developer/apps/quizzer/keys", map[string]string{"name": fmt.Sprint("k", i)}, nil)
		if code == 400 {
			if n := len(listKeys(t, ada, "quizzer")); i != 9 || n != 11 {
				t.Fatalf("refused after %d more, with %d keys", i, n)
			}
			break
		}
		if code != 200 || i > 20 {
			t.Fatalf("key %d: %d", i, code)
		}
	}
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/keys", map[string]string{"name": strings.Repeat("x", 61)}, nil); code != 400 {
		t.Fatalf("a long name: %d", code)
	}
}

// Every app page and action is the owner's: anyone else gets 404, as for an
// app that doesn't exist, and changes nothing.
func TestAppPagesAreTheOwners(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	made := createApp(t, ada, "quizzer", "Quizzer", "")
	app := noted(t, c, made.Key)
	sub, err := coretest.Subscribe(app, coretest.Caller{User: "player-1", Client: "claude-code"}, "game/1", coretest.SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	url, err := coretest.NotePublishURL(app, "game/1")
	if err != nil {
		t.Fatal(err)
	}
	bob := c.SignIn("bob@example.com")
	createApp(t, bob, "bobs-app", "Bob's", "")
	k := made.KeyInfo.ID
	for _, app := range []string{"quizzer", "no-such-app"} {
		p := "/api/v1/developer/apps/" + app
		for _, r := range [][3]string{
			{"GET", p, ""},
			{"PATCH", p, `{"name": "Taken"}`},
			{"DELETE", p, ""},
			{"GET", p + "/log", ""},
			{"GET", p + "/keys", ""},
			{"POST", p + "/keys", `{"name": "mine"}`},
			{"POST", p + "/keys/" + k + "/revoke", ""},
			{"POST", p + "/keys/" + k + "/rotate", ""},
			{"GET", p + "/overview", ""},
			{"GET", p + "/subscriptions", ""},
			{"POST", p + "/subscriptions/" + sub.ID + "/end", ""},
			{"GET", p + "/activity", ""},
			{"GET", p + "/users", ""},
			{"POST", p + "/users/forget", `{"user": "player-1"}`},
			{"GET", p + "/publish-urls", ""},
			{"POST", p + "/publish-urls", `{"event": "note.two", "arguments": {"a": "game", "b": "1"}}`},
			{"POST", p + "/publish-urls/" + url.ID + "/revoke", ""},
		} {
			var body any
			if r[2] != "" {
				json.Unmarshal([]byte(r[2]), &body)
			}
			if code := bob.Do(r[0], r[1], body, nil); code != 404 {
				t.Errorf("bob: %s %s: %d", r[0], r[1], code)
			}
		}
	}
	// Nothing changed: the key, the subscription, the user and the URL are
	// as they were.
	if err := publish(app); err != nil {
		t.Fatalf("ada's key: %v", err)
	}
	if !c.NoteLive(t, sub.ID) {
		t.Fatal("the subscription ended")
	}
	resp, err := http.Post(url.URL, "application/json", bytes.NewReader([]byte(`{"x": 1}`)))
	if err != nil || resp.StatusCode != 202 {
		t.Fatalf("the publish URL: %v %v", resp.Status, err)
	}
	resp.Body.Close()
	_, list := appLog(t, ada, "quizzer", "")
	for _, e := range list {
		if strings.HasPrefix(e.Actor, "user:") && e.Email != "ada@example.com" {
			t.Errorf("someone else is in ada's log: %+v", e)
		}
	}
	var own madeApp
	if code := ada.Do("GET", "/api/v1/developer/apps/quizzer", nil, &own.App); code != 200 || own.App.Name != "Quizzer" {
		t.Fatalf("ada's app: %d %+v", code, own.App)
	}
}

type subscriptionsPage struct {
	Subscriptions []listedSub
	NextCursor    string `json:"next_cursor"`
}

type listedSub struct {
	ID, Event, User, Destination string
	Arguments                    map[string]any
	Once                         bool
	Conversation                 *struct {
		Harness   string
		Connected bool
	}
}

// topic is the note's topic the subscription's arguments name
// (coretest.NoteApp): u/<user>/<a> for a personal note.
func (s listedSub) topic() string {
	if s.Event == "note.mine" {
		return "u/" + s.User + "/" + fmt.Sprint(s.Arguments["a"])
	}
	var segs []string
	for _, k := range []string{"a", "b", "c", "d"} {
		if v, ok := s.Arguments[k]; ok {
			segs = append(segs, fmt.Sprint(v))
		}
	}
	return strings.Join(segs, "/")
}

type activityPage struct {
	Activity []struct {
		ID, Kind, Status, Text, User string
		Topics                       []string
		Data                         map[string]any
		Matched                      *int
		Conversation                 *struct{ Harness string }
	}
	NextCursor string `json:"next_cursor"`
}

// The app's pages show what happens in the workspaces it keeps for its
// users: its subscriptions, publishes and deliveries, and its users. The
// owner can end a subscription and forget a user; both are in the app's log
// and the workspace's audit log, and work as the app's own calls do.
func TestAppPages(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	made := createApp(t, ada, "quizzer", "Quizzer", "")
	app := noted(t, c, made.Key)

	// Before anything happens: empty pages, not errors.
	var empty subscriptionsPage
	if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/subscriptions", nil, &empty); code != 200 || len(empty.Subscriptions) != 0 {
		t.Fatalf("no subscriptions: %d %+v", code, empty)
	}

	subs := map[string]coretest.NoteSub{}
	for _, u := range []string{"player-1", "player-2", "player-3"} {
		s, err := coretest.Subscribe(app, coretest.Caller{User: u, Client: "claude-code"}, "game/1", coretest.SubOpts{Filter: map[string]any{"round": 2}})
		if err != nil {
			t.Fatal(err)
		}
		subs[u] = s
	}
	if _, err := coretest.Subscribe(app, coretest.Caller{User: "player-1", Client: "claude-code"}, "u/player-1/dm", coretest.SubOpts{Once: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := coretest.EmitNote(app, []string{"game/1"}, coretest.Note{Text: "Round 2 <starts>", Data: map[string]any{"round": 2}}); err != nil {
		t.Fatal(err)
	}
	// The pipeline makes the deliveries after the publish returns: let it,
	// so the next publish is newer than all of them.
	for deadline := time.Now().Add(5 * time.Second); ; {
		var act activityPage
		ada.Do("GET", "/api/v1/developer/apps/quizzer/activity?status=waiting", nil, &act)
		if len(act.Activity) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deliveries: %+v", act.Activity)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := coretest.EmitNote(app, []string{"game/2"}, coretest.Note{Text: "Nobody listens"}); err != nil {
		t.Fatal(err)
	}

	// Subscriptions, newest first, a page at a time.
	var all []string
	cursor := ""
	for range 10 {
		var page subscriptionsPage
		if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/subscriptions?limit=1&cursor="+cursor, nil, &page); code != 200 || len(page.Subscriptions) > 1 {
			t.Fatalf("subscriptions page: %d %+v", code, page)
		}
		for _, s := range page.Subscriptions {
			all = append(all, s.topic()+" "+s.User)
			if s.Conversation == nil || s.Conversation.Harness != "claude-code" || s.Destination != "conversation" {
				t.Errorf("subscription %s has conversation %+v", s.ID, s.Conversation)
			}
			if s.topic() == "game/1" && fmt.Sprint(s.Arguments["round"]) != "2" {
				t.Errorf("arguments %v", s.Arguments)
			}
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if strings.Join(all, ", ") != "u/player-1/dm player-1, game/1 player-3, game/1 player-2, game/1 player-1" {
		t.Fatalf("subscriptions: %s", strings.Join(all, ", "))
	}

	// Activity: the publishes and the deliveries they made, filterable.
	var act activityPage
	if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/activity", nil, &act); code != 200 {
		t.Fatalf("activity: %d", code)
	}
	var kinds []string
	for _, e := range act.Activity {
		kinds = append(kinds, e.Kind+":"+e.Status)
	}
	if got := strings.Join(kinds, " "); !strings.HasPrefix(got, "publish:unmatched") || strings.Count(got, "delivery:") != 3 || !strings.Contains(got, "publish:published") {
		t.Fatalf("activity: %s", got)
	}
	ada.Do("GET", "/api/v1/developer/apps/quizzer/activity?user=player-2", nil, &act)
	if len(act.Activity) != 1 || act.Activity[0].User != "player-2" || act.Activity[0].Text != "Round 2 <starts>" || act.Activity[0].Data["round"] != 2.0 ||
		act.Activity[0].Conversation == nil || act.Activity[0].Conversation.Harness != "claude-code" {
		t.Fatalf("player-2's activity: %+v", act.Activity)
	}
	// Conversations are named by a public ID, never by their handle, which
	// in a user's workspace carries the session key's token.
	for _, path := range []string{"subscriptions", "activity"} {
		var raw json.RawMessage
		if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/"+path, nil, &raw); code != 200 || !strings.Contains(string(raw), `"id":"conv_`) {
			t.Fatalf("%s: %d %s", path, code, raw)
		}
		for u, s := range subs {
			if tok := strings.TrimPrefix(s.Session, "wsc_"); tok == s.Session || strings.Contains(string(raw), tok) {
				t.Fatalf("%s shows %s's session key: %s", path, u, raw)
			}
		}
	}
	ada.Do("GET", "/api/v1/developer/apps/quizzer/activity?status=unmatched", nil, &act)
	if len(act.Activity) != 1 || act.Activity[0].Topics[0] != "game/2" || *act.Activity[0].Matched != 0 {
		t.Fatalf("unmatched: %+v", act.Activity)
	}
	ada.Do("GET", "/api/v1/developer/apps/quizzer/activity?topic=game/2", nil, &act)
	if len(act.Activity) != 1 {
		t.Fatalf("game/2: %+v", act.Activity)
	}
	// A topic filter finds the deliveries a publish to it made, as
	// publish writes them, and nothing else.
	ada.Do("GET", "/api/v1/developer/apps/quizzer/activity?topic=game/1&status=waiting", nil, &act)
	if len(act.Activity) != 3 {
		t.Fatalf("game/1 deliveries: %+v", act.Activity)
	}
	for _, e := range act.Activity {
		if e.Kind != "delivery" || len(e.Topics) != 1 || e.Topics[0] != "game/1" || e.Text != "Round 2 <starts>" {
			t.Fatalf("game/1 delivery: %+v", e)
		}
	}
	ada.Do("GET", "/api/v1/developer/apps/quizzer/activity?topic=game/1&status=woke", nil, &act)
	if len(act.Activity) != 0 {
		t.Fatalf("woke, with nothing connected: %+v", act.Activity)
	}
	var seen []string
	cursor = ""
	for range 10 {
		var page activityPage
		if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/activity?limit=2&cursor="+cursor, nil, &page); code != 200 || len(page.Activity) > 2 {
			t.Fatalf("activity page: %d", code)
		}
		for _, e := range page.Activity {
			seen = append(seen, e.ID)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paged %d of 5 activities", len(seen))
	}

	var overview struct {
		Users, Subscriptions int
		Publishes            int `json:"publishes_24h"`
	}
	if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/overview", nil, &overview); code != 200 || overview.Users != 3 || overview.Subscriptions != 4 || overview.Publishes != 2 {
		t.Fatalf("overview: %d %+v", code, overview)
	}

	// Ending one subscription from the console.
	var ended struct{ Ended bool }
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/subscriptions/"+subs["player-2"].ID+"/end", nil, &ended); code != 200 || !ended.Ended {
		t.Fatalf("end: %d %+v", code, ended)
	}
	if c.NoteLive(t, subs["player-2"].ID) {
		t.Fatal("still live after ending")
	}
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/subscriptions/ts_nope/end", nil, nil); code != 404 {
		t.Fatalf("ending a made-up subscription: %d", code)
	}
	var user string
	c.DB.Pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email = 'ada@example.com'`).Scan(&user)
	var ws string
	c.DB.Pool.QueryRow(ctx, `SELECT id::text FROM tenants WHERE app_id = 'quizzer' AND external_id = 'player-2'`).Scan(&ws)
	var audited int
	c.DB.InTenant(ctx, ws, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'event_subscription.unsubscribed' AND actor = $1`, "user:"+user).Scan(&audited)
	})
	if audited != 1 {
		t.Fatalf("the workspace's audit log has %d entries for ending it", audited)
	}

	// Forgetting a user.
	var users struct {
		Users []struct {
			User                         string
			Subscriptions, Conversations int
		}
	}
	ada.Do("GET", "/api/v1/developer/apps/quizzer/users", nil, &users)
	if len(users.Users) != 3 || users.Users[2].User != "player-1" || users.Users[2].Subscriptions != 2 || users.Users[2].Conversations != 2 {
		t.Fatalf("users: %+v", users.Users)
	}
	var forgot struct{ Forgotten bool }
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/users/forget", map[string]string{"user": "player-1"}, &forgot); code != 200 || !forgot.Forgotten {
		t.Fatalf("forget: %d %+v", code, forgot)
	}
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/users/forget", map[string]string{"user": "~codex:x"}, nil); code != 400 {
		t.Fatalf("forgetting a conversation's workspace: %d", code)
	}
	ada.Do("GET", "/api/v1/developer/apps/quizzer/users", nil, &users)
	if len(users.Users) != 2 {
		t.Fatalf("users after forgetting: %+v", users.Users)
	}
	_, list := appLog(t, ada, "quizzer", "")
	if actions(list[:2]) != "app.user_forgotten app.subscription_ended" || list[0].Email != "ada@example.com" || list[1].Detail["event"] != "note.two" {
		t.Fatalf("log: %s", actions(list))
	}
	var raw json.RawMessage
	ada.Do("GET", "/api/v1/developer/apps/quizzer/log", nil, &raw)
	if strings.Contains(string(raw), "player-1") {
		t.Fatal("the log names a forgotten user")
	}

	for _, q := range []string{"?limit=0", "?limit=101", "?cursor=!!", "?cursor=bm9wZQ"} {
		for _, p := range []string{"subscriptions", "activity", "users"} {
			if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/"+p+q, nil, nil); code != 400 {
				t.Errorf("%s%s: %d", p, q, code)
			}
		}
	}
	if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/activity?status=nope", nil, nil); code != 400 {
		t.Errorf("a made-up status: %d", code)
	}
	// "cancelled" is still accepted for a release, as "canceled".
	for _, s := range []string{"canceled", "cancelled"} {
		if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/activity?status="+s, nil, nil); code != 200 {
			t.Errorf("status %s: %d", s, code)
		}
	}
}

// Publish URLs made in the console are shown once and work like the app's
// own; revoking one stops it. Making and revoking them is logged, from the
// console or the app.
func TestAppPublishURLs(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	made := createApp(t, ada, "quizzer", "Quizzer", "")
	app := noted(t, c, made.Key)
	var made2 struct{ ID, Topic, URL string }
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/publish-urls", map[string]any{"event": "note.two", "arguments": map[string]any{"a": "ci", "b": "acme"}}, &made2); code != 200 || !strings.Contains(made2.URL, "/v1/publish/wsp_") {
		t.Fatalf("create: %d %+v", code, made2)
	}
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/publish-urls", map[string]any{"event": "note.two", "arguments": map[string]any{"a": "Not a topic!", "b": "x"}}, nil); code != 400 {
		t.Fatalf("bad arguments: %d", code)
	}
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/publish-urls", map[string]string{"topic": "ci/acme"}, nil); code != 400 {
		t.Fatalf("a topic's: %d", code)
	}
	fromApp, err := coretest.NotePublishURL(app, "ci/other")
	if err != nil {
		t.Fatal(err)
	}
	post := func(u string) int {
		resp, err := http.Post(u, "application/json", strings.NewReader(`{"ok": true}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(made2.URL); code != 202 {
		t.Fatalf("publishing: %d", code)
	}
	var raw json.RawMessage
	ada.Do("GET", "/api/v1/developer/apps/quizzer/publish-urls", nil, &raw)
	if strings.Contains(string(raw), "wsp_") || !strings.Contains(string(raw), made2.ID) || !strings.Contains(string(raw), fromApp.ID) {
		t.Fatalf("list: %s", raw)
	}
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/publish-urls/"+made2.ID+"/revoke", nil, nil); code != 200 {
		t.Fatalf("revoke: %d", code)
	}
	if code := post(made2.URL); code != 410 {
		t.Fatalf("a revoked publish URL: %d", code)
	}
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/publish-urls/00000000-0000-0000-0000-000000000000/revoke", nil, nil); code != 404 {
		t.Fatalf("a made-up publish URL: %d", code)
	}
	_, list := appLog(t, ada, "quizzer", "?action=app.publish_url_")
	if actions(list) != "app.publish_url_revoked app.publish_url_created app.publish_url_created" ||
		list[0].Email != "ada@example.com" || list[1].Actor != "app:quizzer" || list[2].Detail["event"] != "note.two" {
		t.Fatalf("log: %+v", list)
	}
	if strings.Contains(fmt.Sprint(list), "wsp_") {
		t.Fatal("the log shows a publish URL's token")
	}
}

// Settings: the name and MCP URL change, and the log says so.
func TestAppSettings(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	noted(t, c, createApp(t, ada, "quizzer", "Quizzer", "").Key)
	var out madeApp
	if code := ada.Do("PATCH", "/api/v1/developer/apps/quizzer", map[string]string{"name": "Quiz Night", "mcp_url": "https://quiz.test/mcp"}, &out.App); code != 200 ||
		out.App.Name != "Quiz Night" || out.App.MCPURL != "https://quiz.test/mcp" {
		t.Fatalf("update: %d %+v", code, out.App)
	}
	for _, bad := range []map[string]string{{"name": ""}, {"name": "a <b>"}, {"mcp_url": "ftp://x"}} {
		if code := ada.Do("PATCH", "/api/v1/developer/apps/quizzer", bad, nil); code != 400 {
			t.Errorf("%v: %d", bad, code)
		}
	}
	_, list := appLog(t, ada, "quizzer", "?action=app.updated")
	if len(list) != 1 || fmt.Sprint(list[0].Detail["changes"]) != "[name mcp_url]" || list[0].Detail["old_name"] != "Quizzer" {
		t.Fatalf("log: %+v", list)
	}
	var u struct{ ID, URL string }
	ada.Do("POST", "/api/v1/developer/apps/quizzer/publish-urls", map[string]any{"event": "note.two", "arguments": map[string]any{"a": "ci", "b": "x"}}, &u)
	ada.Do("DELETE", "/api/v1/developer/apps/quizzer", nil, nil)
	// Its publish URLs stopped with it, and there's nothing left to change.
	resp, err := http.Post(u.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 410 {
		t.Fatalf("a deleted app's publish URL: %d", resp.StatusCode)
	}
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/publish-urls/"+u.ID+"/revoke", nil, nil); code != 409 {
		t.Fatalf("revoking a deleted app's publish URL: %d", code)
	}
	if code := ada.Do("PATCH", "/api/v1/developer/apps/quizzer", map[string]string{"name": "Again"}, nil); code != 409 {
		t.Fatalf("renaming a deleted app: %d", code)
	}
	if code := ada.Do("POST", "/api/v1/developer/apps/quizzer/keys", map[string]string{"name": "Again"}, nil); code != 409 {
		t.Fatalf("a key for a deleted app: %d", code)
	}
}

// Pages merged across many workspaces come out whole and in order, newest
// first, however the page size falls across them.
func TestSubscriptionPagesAcrossWorkspaces(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	app := noted(t, c, createApp(t, ada, "quizzer", "Quizzer", "").Key)
	for i := range 12 {
		for _, topic := range []string{"game/a", "game/b"} {
			if _, err := coretest.Subscribe(app, coretest.Caller{User: fmt.Sprint("p", i), Client: "claude-code"}, topic, coretest.SubOpts{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	var all []string
	cursor := ""
	for range 20 {
		var page subscriptionsPage
		if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/subscriptions?limit=5&cursor="+cursor, nil, &page); code != 200 || len(page.Subscriptions) > 5 {
			t.Fatalf("page: %d", code)
		}
		for _, s := range page.Subscriptions {
			all = append(all, s.User+" "+s.topic())
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if len(all) != 24 || all[0] != "p11 game/b" || all[23] != "p0 game/a" {
		t.Fatalf("%d subscriptions: %v", len(all), all)
	}
	seen := map[string]bool{}
	for _, s := range all {
		if seen[s] {
			t.Fatalf("%s twice", s)
		}
		seen[s] = true
	}
}

// Connected counts every connected conversation, not just the most recent
// few, in the overview and on the users page.
func TestConnectedCountsEverySession(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	createApp(t, ada, "quizzer", "Quizzer", "")
	var ws string
	if err := c.DB.Pool.QueryRow(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('Quizzer user', 'app', 'quizzer', 'busy') RETURNING id::text`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	err := c.DB.InTenant(ctx, ws, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'claude-code')`); err != nil {
			return err
		}
		// 70 sessions, each with a fresh lease, plus 10 idle ones more
		// recently active.
		if _, err := tx.Exec(ctx, `INSERT INTO sessions (tenant_id, agent_id, handle, created_by, last_active_at)
			SELECT current_tenant(), 'claude-code', 'claude-code#' || n, 'app:quizzer', now() - make_interval(secs => n) FROM generate_series(1, 80) n`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO stream_leases (tenant_id, agent_id, target, holder, renewed_at)
			SELECT current_tenant(), 'claude-code', id::text, 'stream-test', now() FROM sessions WHERE last_active_at < now() - interval '10 seconds'`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// And a conversation without a user, in its own workspace.
	var own string
	if err := c.DB.Pool.QueryRow(ctx, `INSERT INTO tenants (name, kind, app_id, external_id) VALUES ('Quizzer conversation', 'app', 'quizzer', '~conversation:one') RETURNING id::text`).Scan(&own); err != nil {
		t.Fatal(err)
	}
	if err := c.DB.InTenant(ctx, own, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (tenant_id, id) VALUES (current_tenant(), 'codex')`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO sessions (tenant_id, agent_id, handle, created_by) VALUES (current_tenant(), 'codex', 'codex@conversation', 'app:quizzer')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var overview struct {
		Connected     int
		ByHarness     map[string]int `json:"connected_by_harness"`
		Users         int
		NoUser        int `json:"conversation_workspaces"`
		Conversations int
	}
	ada.Do("GET", "/api/v1/developer/apps/quizzer/overview", nil, &overview)
	if overview.Connected != 70 || overview.ByHarness["claude-code"] != 70 || overview.Users != 1 || overview.NoUser != 1 || overview.Conversations != 81 {
		t.Fatalf("overview: %+v", overview)
	}
	var users struct {
		Users []struct{ Connected, Conversations int }
	}
	ada.Do("GET", "/api/v1/developer/apps/quizzer/users", nil, &users)
	// Newest first: the conversation's own workspace, then the user's.
	if len(users.Users) != 2 || users.Users[0].Conversations != 1 || users.Users[1].Connected != 70 || users.Users[1].Conversations != 80 {
		t.Fatalf("users: %+v", users.Users)
	}
}

// A page reads at most 200 of the app's workspaces, and says so; the users
// list pages through all of them.
func TestAppPagesAreBounded(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	createApp(t, ada, "quizzer", "Quizzer", "")
	if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO tenants (name, kind, app_id, external_id)
		SELECT 'Quizzer user', 'app', 'quizzer', 'user-' || n FROM generate_series(1, 201) n`); err != nil {
		t.Fatal(err)
	}
	// A conversation's own workspace older than all of them, beyond the
	// 200 read, still counts as a conversation.
	if _, err := c.DB.Pool.Exec(ctx, `INSERT INTO tenants (name, kind, app_id, external_id, created_at)
		VALUES ('Quizzer conversation', 'app', 'quizzer', '~conversation:old', now() - interval '1 day')`); err != nil {
		t.Fatal(err)
	}
	var overview struct {
		Users         int
		Conversations int
		Scanned       struct {
			Scanned int `json:"workspaces_scanned"`
			Total   int `json:"workspaces_total"`
		}
	}
	if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/overview", nil, &overview); code != 200 || overview.Users != 201 || overview.Conversations != 1 ||
		overview.Scanned.Scanned != 200 || overview.Scanned.Total != 202 {
		t.Fatalf("overview: %d %+v", code, overview)
	}
	seen := map[string]bool{}
	cursor := ""
	for range 5 {
		var page struct {
			Users      []struct{ User string }
			NextCursor string `json:"next_cursor"`
		}
		if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/users?limit=100&cursor="+cursor, nil, &page); code != 200 || len(page.Users) > 100 {
			t.Fatalf("users page: %d %d", code, len(page.Users))
		}
		for _, u := range page.Users {
			seen[u.User] = true
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if len(seen) != 202 { // the 201 users and "" for the conversation
		t.Fatalf("paged %d of 202 workspaces", len(seen))
	}
}

// Keys can be renamed: one app_log entry with the old and new names, in
// the same transaction; a revoked key keeps its name (409), the same name
// changes nothing, and a bad name is refused. A new app's first key can be
// named.
func TestRenameKeys(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	var made madeApp
	if code := ada.Do("POST", "/api/v1/developer/apps", map[string]string{"id": "quizzer", "name": "Quizzer", "key_name": "Production"}, &made); code != 200 || made.KeyInfo.Name != "Production" {
		t.Fatalf("new app with a key name: %d %+v", code, made.KeyInfo)
	}
	if code := ada.Do("POST", "/api/v1/developer/apps", map[string]string{"id": "other-app", "name": "Other", "key_name": strings.Repeat("x", 61)}, nil); code != 400 {
		t.Fatalf("a bad first key name: %d", code)
	}
	if other := createApp(t, ada, "other-app", "Other", ""); other.KeyInfo.Name != "Default" {
		t.Fatalf("no key name: %q", other.KeyInfo.Name)
	}
	renames := func() int {
		var n int
		c.DB.Pool.QueryRow(context.Background(), `SELECT count(*) FROM app_log WHERE app_id = 'quizzer' AND action = 'app.key_renamed'`).Scan(&n)
		return n
	}
	path := "/api/v1/developer/apps/quizzer/keys/" + made.KeyInfo.ID
	var out madeApp
	if code := ada.Do("PATCH", path, map[string]string{"name": "  Laptop "}, &out); code != 200 || out.KeyInfo.Name != "Laptop" || renames() != 1 {
		t.Fatalf("rename: %d %+v, %d entries", code, out.KeyInfo, renames())
	}
	var detail map[string]string
	var keyID string
	c.DB.Pool.QueryRow(context.Background(), `SELECT key_id, detail FROM app_log WHERE app_id = 'quizzer' AND action = 'app.key_renamed'`).Scan(&keyID, &detail)
	if keyID != made.KeyInfo.ID || detail["old_name"] != "Production" || detail["name"] != "Laptop" {
		t.Fatalf("the log: %s %v", keyID, detail)
	}
	if code := ada.Do("PATCH", path, map[string]string{"name": "Laptop"}, nil); code != 200 || renames() != 1 {
		t.Fatalf("the same name: %d, %d entries", code, renames())
	}
	if code := ada.Do("PATCH", path, map[string]string{"name": "two\nlines"}, nil); code != 400 {
		t.Fatalf("a bad name: %d", code)
	}
	if code := ada.Do("PATCH", "/api/v1/developer/apps/quizzer/keys/nope", map[string]string{"name": "x"}, nil); code != 404 {
		t.Fatalf("no such key: %d", code)
	}
	if code := ada.Do("PATCH", "/api/v1/developer/apps/other-app/keys/"+made.KeyInfo.ID, map[string]string{"name": "x"}, nil); code != 404 {
		t.Fatalf("another app's key: %d", code)
	}
	if keys := listKeys(t, ada, "quizzer"); len(keys) != 1 || keys[0].Name != "Laptop" {
		t.Fatalf("keys: %+v", keys)
	}
	ada.Do("POST", path+"/revoke", nil, nil)
	var e apiError
	if code := ada.Do("PATCH", path, map[string]string{"name": "Old"}, &e); code != 409 || e.Error.Code != "revoked" || renames() != 1 {
		t.Fatalf("a revoked key: %d %+v", code, e)
	}
	eve := c.SignIn("eve@example.com")
	if code := eve.Do("PATCH", path, map[string]string{"name": "mine"}, nil); code != 404 {
		t.Fatalf("a non-member: %d", code)
	}
}
