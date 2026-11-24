package console_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/coretest"
)

// buildEvents is a shared event with an argument and a personal one.
func buildEvents() apps.Manifest {
	return apps.Manifest{Events: []apps.EventType{{
		Name:        "build.done",
		Description: "A build finished.",
		Arguments:   json.RawMessage(`{"type":"object","properties":{"repo":{"type":"string"}},"required":["repo"],"additionalProperties":false}`),
		Payload:     json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"}},"required":["status"]}`),
		Topic:       "builds/{repo}",
	}, {
		Name:        "build.mine",
		Description: "Your build finished.",
		Arguments:   json.RawMessage(`{"type":"object","properties":{"repo":{"type":"string"}},"required":["repo"],"additionalProperties":false}`),
		Payload:     json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"}},"required":["status"]}`),
		Topic:       "mine/{repo}",
		Personal:    true,
	}}}
}

// The console makes no-code publish URLs on events: the developer picks an
// event, fills its arguments (and a user, for a personal event), and a POST
// to the URL emits it. The list shows the event, its arguments and its user;
// what the event doesn't accept is refused, and only the app's organization
// can make one.
func TestAppEventPublishURLs(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	createApp(t, ada, "quizzer", "Quizzer", "")
	if err := c.Apps.SetManifest(ctx, "quizzer", buildEvents()); err != nil {
		t.Fatal(err)
	}
	const p = "/api/v1/developer/apps/quizzer/publish-urls"
	var shared, mine struct{ ID, Event, Topic, URL string }
	if code := ada.Do("POST", p, map[string]any{"event": "build.done", "arguments": map[string]any{"repo": "api"}}, &shared); code != 200 ||
		shared.Event != "build.done" || !strings.Contains(shared.URL, "/v1/publish/wsp_") {
		t.Fatalf("a shared event's: %d %+v", code, shared)
	}
	if code := ada.Do("POST", p, map[string]any{"event": "build.mine", "arguments": map[string]any{"repo": "api"}, "user": "u1"}, &mine); code != 200 || mine.Topic != "" {
		t.Fatalf("a personal event's: %d %+v", code, mine)
	}
	for name, body := range map[string]map[string]any{
		"unknown event":       {"event": "build.nope", "arguments": map[string]any{"repo": "api"}},
		"missing argument":    {"event": "build.done", "arguments": map[string]any{}},
		"personal, no user":   {"event": "build.mine", "arguments": map[string]any{"repo": "api"}},
		"shared, with a user": {"event": "build.done", "arguments": map[string]any{"repo": "api"}, "user": "u1"},
		"event and topic":     {"event": "build.done", "arguments": map[string]any{"repo": "api"}, "topic": "ci/x"},
	} {
		if code := ada.Do("POST", p, body, nil); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	// Someone outside the app's organization can't make one.
	bob := c.SignIn("bob@example.com")
	if code := bob.Do("POST", p, map[string]any{"event": "build.done", "arguments": map[string]any{"repo": "api"}}, nil); code != 404 {
		t.Fatalf("an outsider: %d", code)
	}

	post := func(u, body string) int {
		resp, err := http.Post(u, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(shared.URL, `{"status": "green"}`); code != 202 {
		t.Fatalf("publishing: %d", code)
	}
	if code := post(shared.URL, `{"nope": 1}`); code != 400 {
		t.Fatalf("a payload the schema refuses: %d", code)
	}
	var topics []string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT topics FROM topic_events WHERE publish_url = $1::uuid`, shared.ID).Scan(&topics); err != nil || len(topics) != 1 || topics[0] != "builds/api" {
		t.Fatalf("the emit: %v %v", topics, err)
	}

	var list struct {
		PublishURLs []struct {
			ID, Topic, Event, User string
			Arguments              map[string]any
		} `json:"publish_urls"`
		Events []struct {
			Name      string
			Personal  bool
			Arguments []struct {
				Name, Type string
				Required   bool
			}
		}
	}
	var raw json.RawMessage
	ada.Do("GET", p, nil, &raw)
	if strings.Contains(string(raw), "wsp_") {
		t.Fatal("the list shows a token")
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Events) != 2 || list.Events[1].Name != "build.mine" || !list.Events[1].Personal || len(list.Events[0].Arguments) != 1 ||
		list.Events[0].Arguments[0].Name != "repo" || list.Events[0].Arguments[0].Type != "string" || !list.Events[0].Arguments[0].Required {
		t.Fatalf("the events offered: %+v", list.Events)
	}
	seen := 0
	for _, u := range list.PublishURLs {
		switch u.ID {
		case shared.ID:
			seen++
			if u.Event != "build.done" || u.Arguments["repo"] != "api" || u.User != "" {
				t.Errorf("the shared event's: %+v", u)
			}
		case mine.ID:
			seen++
			if u.Event != "build.mine" || u.Arguments["repo"] != "api" || u.User != "u1" || strings.Contains(u.Topic, "u/p") {
				t.Errorf("the personal event's: %+v", u)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("listed: %s", raw)
	}
	// The topic form is gone.
	if code := ada.Do("POST", p, map[string]string{"topic": "ci/acme"}, nil); code != 400 {
		t.Fatalf("a topic's: %d", code)
	}
}

// The console sets, rotates and removes a publish URL's signing secret,
// for members of the app's organization only. The list and every response
// show its fingerprint, never the secret.
func TestAppPublishURLSigningSecret(t *testing.T) {
	c := coretest.Start(t)
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	createApp(t, ada, "quizzer", "Quizzer", "")
	if err := c.Apps.SetManifest(ctx, "quizzer", buildEvents()); err != nil {
		t.Fatal(err)
	}
	const p = "/api/v1/developer/apps/quizzer/publish-urls"
	secret := "whsec_" + strings.Repeat("Q", 32)
	secret2 := "whsec_" + strings.Repeat("R", 32)
	var raw json.RawMessage
	var responses []string
	if code := ada.Do("POST", p, map[string]any{"event": "build.done", "arguments": map[string]any{"repo": "api"}, "signing_secret": secret}, &raw); code != 200 {
		t.Fatalf("made with a secret: %d %s", code, raw)
	}
	responses = append(responses, string(raw))
	var made struct {
		ID      string
		Signing *struct{ Fingerprint string }
	}
	json.Unmarshal(raw, &made)
	if made.Signing == nil || made.Signing.Fingerprint == "" {
		t.Fatalf("made: %s", raw)
	}
	if code := ada.Do("POST", p, map[string]any{"event": "build.done", "arguments": map[string]any{"repo": "web"}, "signing_secret": "nope"}, nil); code != 400 {
		t.Fatalf("a bad secret: %d", code)
	}
	sp := p + "/" + made.ID + "/signing-secret"
	raw = nil
	if code := ada.Do("PUT", sp, map[string]any{"secret": secret2}, &raw); code != 200 || !strings.Contains(string(raw), "previous_valid_until") {
		t.Fatalf("rotating: %d %s", code, raw)
	}
	responses = append(responses, string(raw))

	list := func() string {
		var raw json.RawMessage
		if code := ada.Do("GET", p, nil, &raw); code != 200 {
			t.Fatalf("list: %d", code)
		}
		responses = append(responses, string(raw))
		return string(raw)
	}
	if l := list(); !strings.Contains(l, `"signing":{"fingerprint"`) || !strings.Contains(l, "previous_valid_until") {
		t.Fatalf("the list: %s", l)
	}

	// Someone outside the app's organization can't change it.
	bob := c.SignIn("bob@example.com")
	if code := bob.Do("PUT", sp, map[string]any{"secret": secret}, nil); code != 404 {
		t.Fatalf("an outsider setting: %d", code)
	}
	if code := bob.Do("DELETE", sp, nil, nil); code != 404 {
		t.Fatalf("an outsider removing: %d", code)
	}

	var removed struct{ Removed bool }
	if code := ada.Do("DELETE", sp, nil, &removed); code != 200 || !removed.Removed {
		t.Fatalf("removing: %d %+v", code, removed)
	}
	if l := list(); strings.Contains(l, `"signing"`) {
		t.Fatalf("the list after removing: %s", l)
	}
	if code := ada.Do("PUT", p+"/00000000-0000-0000-0000-000000000000/signing-secret", map[string]any{"secret": secret}, nil); code != 404 {
		t.Fatalf("an unknown URL: %d", code)
	}

	var actor string
	if err := c.DB.Pool.QueryRow(ctx, `SELECT actor FROM app_log WHERE app_id = 'quizzer' AND action = 'app.publish_url_secret_removed'`).Scan(&actor); err != nil || !strings.HasPrefix(actor, "user:") {
		t.Fatalf("the removal's log entry: %q %v", actor, err)
	}
	for _, r := range responses {
		if strings.Contains(r, secret[6:]) || strings.Contains(r, secret2[6:]) {
			t.Fatalf("a response has the secret: %s", r)
		}
	}
}
