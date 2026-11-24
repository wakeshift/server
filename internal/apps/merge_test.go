package apps_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/wakeshift/server/internal/apps"
)

func event(name, description string) apps.EventType {
	return apps.EventType{Name: name, Description: description, Topic: "ev/" + name,
		Arguments: json.RawMessage(`{"type":"object","properties":{}}`), Payload: json.RawMessage(`{"type":"object","properties":{}}`)}
}

func names(m apps.Manifest) []string {
	var out []string
	for _, e := range m.Events {
		out = append(out, e.Name+": "+e.Description)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

// A merge (the SDKs' automatic send, from each process that serves the
// app's tools) adds and updates events and sets only the settings it
// carries: two processes sharing a key, each with its own events, never
// remove each other's. Only a full manifest removes an event.
func TestMergeManifestKeepsWhatItOmits(t *testing.T) {
	r, _, d := setupDB(t)
	scope := &apps.AgentScope{TTLDays: 30, StartPrompt: "Join #general"}
	full := apps.Manifest{Description: "Team chat", Agent: scope, OthersCanWake: true, PreapproveTools: []string{"history"},
		Events: []apps.EventType{event("chat.message", "A message."), event("ci.finished", "A CI run finished.")}}
	if err := r.SetManifest(ctx, "agentchat", full); err != nil {
		t.Fatal(err)
	}

	// A process that defines only chat.message (a newer version) and a new
	// event, with no settings.
	got, err := r.MergeManifestSigned(ctx, "agentchat", "", apps.ManifestPatch{
		Events: []apps.EventType{event("chat.message", "A new message."), event("chat.joined", "Someone joined.")}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"chat.message: A new message.", "ci.finished: A CI run finished.", "chat.joined: Someone joined."}
	if !slices.Equal(names(got), want) {
		t.Fatalf("events after a merge: %q, want %q", names(got), want)
	}
	stored, err := r.Get(ctx, "agentchat")
	if err != nil {
		t.Fatal(err)
	}
	m := stored.Manifest
	if !slices.Equal(names(m), want) || m.Description != "Team chat" || !m.OthersCanWake || m.Agent == nil ||
		m.Agent.StartPrompt != "Join #general" || !slices.Equal(m.PreapproveTools, []string{"history"}) {
		t.Fatalf("stored after a merge with no settings: %+v", m)
	}

	// Settings it carries replace the stored ones (the agent scope as a
	// whole); the others stay.
	got, err = r.MergeManifestSigned(ctx, "agentchat", "", apps.ManifestPatch{Description: ptr("Chat for agents"),
		PreapproveTools: &[]string{"history", "read"}, Agent: &apps.AgentScope{TTLDays: 60}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "Chat for agents" || !got.OthersCanWake || got.Agent.TTLDays != 60 || got.Agent.StartPrompt != "" ||
		!slices.Equal(got.PreapproveTools, []string{"history", "read"}) || !slices.Equal(names(got), want) {
		t.Fatalf("after a merge with settings: %+v", got)
	}
	if stored, _ := r.Get(ctx, "agentchat"); stored.Description != "Chat for agents" {
		t.Fatalf("the app's description column: %q", stored.Description)
	}
	// Sent, a false or empty setting clears it.
	got, err = r.MergeManifestSigned(ctx, "agentchat", "", apps.ManifestPatch{OthersCanWake: ptr(false), PreapproveTools: &[]string{}})
	if err != nil || got.OthersCanWake || len(got.PreapproveTools) != 0 {
		t.Fatalf("a merge that clears: %+v %v", got, err)
	}

	// The full manifest, the app's explicit publish, removes what it omits.
	if err := r.SetManifest(ctx, "agentchat", apps.Manifest{Description: "Team chat", Events: []apps.EventType{event("ci.finished", "A CI run finished.")}}); err != nil {
		t.Fatal(err)
	}
	if stored, _ := r.Get(ctx, "agentchat"); !slices.Equal(names(stored.Manifest), []string{"ci.finished: A CI run finished."}) || stored.Manifest.Agent != nil {
		t.Fatalf("after a full publish: %+v", stored.Manifest)
	}

	// The app's log: a merge that changes something says it merged; one
	// that changes nothing isn't logged.
	if _, err := r.MergeManifestSigned(ctx, "agentchat", "kid-1", apps.ManifestPatch{Events: []apps.EventType{event("chat.message", "A message.")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.MergeManifestSigned(ctx, "agentchat", "kid-1", apps.ManifestPatch{Events: []apps.EventType{event("chat.message", "A message.")}}); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Pool.Query(ctx, `SELECT coalesce(key_id, ''), detail FROM app_log WHERE app_id = 'agentchat' AND action = $1 ORDER BY id`, apps.LogManifestPublished)
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		key    string
		detail struct {
			First   bool     `json:"first"`
			Merged  bool     `json:"merged"`
			Changes []string `json:"changes"`
		}
	}
	var log []entry
	for rows.Next() {
		var e entry
		var b []byte
		if err := rows.Scan(&e.key, &b); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &e.detail); err != nil {
			t.Fatal(err)
		}
		log = append(log, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// The first publish, three merges, the full publish and the one merge
	// that changed something.
	if len(log) != 6 {
		t.Fatalf("%d manifest log entries, want 6: %+v", len(log), log)
	}
	merged := []bool{false, true, true, true, false, true}
	for i, e := range log {
		if e.detail.Merged != merged[i] || e.detail.First != (i == 0) {
			t.Errorf("entry %d: %+v", i, e)
		}
	}
	if last := log[5]; last.key != "kid-1" || !slices.Equal(last.detail.Changes, []string{"event chat.message added"}) {
		t.Errorf("the last merge's entry: %+v", last)
	}
}

// A merge is validated as a whole manifest, and refuses an event it names
// twice; nothing is stored.
func TestMergeManifestRefusals(t *testing.T) {
	r, _ := setup(t)
	if err := r.SetManifest(ctx, "agentchat", apps.Manifest{Description: "Team chat", Events: []apps.EventType{event("chat.message", "A message.")}}); err != nil {
		t.Fatal(err)
	}
	workspace := json.RawMessage(`{"publish":[{"pattern":"app.agentchat.x"}],"ttl_days":30}`)
	bad := event("chat.joined", "")
	for name, p := range map[string]apps.ManifestPatch{
		"an event twice":   {Events: []apps.EventType{event("chat.joined", "A."), event("chat.joined", "B.")}},
		"an invalid event": {Events: []apps.EventType{bad}},
		"account mode":     {Accounts: ptr(apps.AccountsWakeshift)},
		"workspace scope":  {Workspace: &workspace},
		"a long TTL":       {Agent: &apps.AgentScope{TTLDays: 1000}},
	} {
		if _, err := r.MergeManifestSigned(ctx, "agentchat", "", p); !errors.Is(err, apps.ErrInvalidManifest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if stored, _ := r.Get(ctx, "agentchat"); !slices.Equal(names(stored.Manifest), []string{"chat.message: A message."}) || stored.Manifest.Description != "Team chat" {
		t.Fatalf("stored after refusals: %+v", stored.Manifest)
	}
	if _, err := r.MergeManifestSigned(ctx, "nobody", "", apps.ManifestPatch{}); !errors.Is(err, apps.ErrNotFound) {
		t.Fatalf("an unknown app: %v", err)
	}
}
