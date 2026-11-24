package appapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// POST /manifest/merge, the SDKs' automatic send, merges into the stored
// manifest; PUT /manifest, an explicit publish (and every 0.4.0 SDK's
// automatic send), still replaces it.
func TestManifestMergeAndReplace(t *testing.T) {
	f := setup(t)
	ev := func(name string) map[string]any {
		return map[string]any{"name": name, "description": "An event.", "topic": "ev/" + name,
			"arguments": map[string]any{"type": "object", "properties": map[string]any{}},
			"payload":   map[string]any{"type": "object", "properties": map[string]any{}}}
	}
	type stored struct {
		Description   string `json:"description"`
		OthersCanWake bool   `json:"others_can_wake"`
		Events        []struct{ Name string }
	}
	namesOf := func(s stored) string {
		var out []string
		for _, e := range s.Events {
			out = append(out, e.Name)
		}
		return strings.Join(out, ",")
	}
	if code := f.call(http.DefaultClient, "PUT", "/api/apps/v1/manifest", f.assertion(),
		map[string]any{"description": "Your Slack", "others_can_wake": true, "events": []any{ev("chat.message"), ev("ci.finished")}}, nil); code != 200 {
		t.Fatalf("publish: %d", code)
	}
	var got stored
	if code := f.call(http.DefaultClient, "POST", "/api/apps/v1/manifest/merge", f.assertion(),
		map[string]any{"events": []any{ev("chat.joined")}}, &got); code != 200 ||
		namesOf(got) != "chat.message,ci.finished,chat.joined" || got.Description != "Your Slack" || !got.OthersCanWake {
		t.Fatalf("merge: %d %+v", code, got)
	}
	for name, body := range map[string]any{
		"an unknown field": map[string]any{"events": []any{}, "replace": true},
		"an event twice":   map[string]any{"events": []any{ev("chat.joined"), ev("chat.joined")}},
	} {
		var out struct {
			Error struct{ Code, Message string }
		}
		if code := f.call(http.DefaultClient, "POST", "/api/apps/v1/manifest/merge", f.assertion(), body, &out); code != 400 {
			t.Errorf("%s: %d %+v", name, code, out)
		}
	}
	if code := f.call(http.DefaultClient, "POST", "/api/apps/v1/manifest/merge", "not-an-assertion", map[string]any{}, nil); code != 401 {
		t.Fatalf("merge without an assertion: %d", code)
	}
	if code := f.call(http.DefaultClient, "PUT", "/api/apps/v1/manifest", f.assertion(),
		map[string]any{"description": "Your Slack", "events": []any{ev("chat.joined")}}, nil); code != 200 {
		t.Fatalf("replace: %d", code)
	}
	got = stored{}
	if code := f.call(http.DefaultClient, "GET", "/api/apps/v1/manifest", f.assertion(), nil, &got); code != 200 || namesOf(got) != "chat.joined" || got.OthersCanWake {
		t.Fatalf("after a replace: %d %+v", code, got)
	}
}
