package coretest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// jobEvent is an event type with arguments org and job (both required
// strings), on topic.
func jobEvent(name, topic string) wakeshift.ManifestEvent {
	return wakeshift.ManifestEvent{
		Name: name, Description: "A job finished.", Topic: topic,
		Arguments: json.RawMessage(`{"type":"object","properties":{"org":{"type":"string"},"job":{"type":"string"}},"required":["org","job"],"additionalProperties":false}`),
		Payload:   json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	}
}

func storedEvents(t *testing.T, c *coretest.Core, app string) []string {
	t.Helper()
	a, err := c.Apps.Get(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range a.Manifest.Events {
		out = append(out, e.Name+" "+e.Topic)
	}
	return out
}

// Two placeholders in one topic segment make one topic from different
// arguments ({org: "acme-prod", job: "42"} and {org: "acme", job:
// "prod-42"} are both jobs/acme-prod-42), so each subscriber would get the
// other's occurrences: refused, on a publish and on a merge.
func TestTwoPlaceholdersInASegmentRefused(t *testing.T) {
	c := coretest.Start(t)
	app := bareApp(t, c, "jobs")
	for _, topic := range []string{"jobs/{org}-{job}", "jobs/{org}.{job}", "jobs/{org}{job}", "jobs/{org}_x_{job}"} {
		err := app.PublishManifest(ctx, wakeshift.Manifest{Events: []wakeshift.ManifestEvent{jobEvent("job.done", topic)}})
		if !wakeshift.IsCode(err, "ambiguous_topic") || !strings.Contains(err.Error(), topic) {
			t.Errorf("publishing %s: %v, want ambiguous_topic naming it", topic, err)
		}
		if code, e := appAPI(t, c, app, "/api/apps/v1/manifest/merge", map[string]any{"events": []wakeshift.ManifestEvent{jobEvent("job.done", topic)}}, nil); code != 400 || e != "ambiguous_topic" {
			t.Errorf("merging %s: %d %s, want 400 ambiguous_topic", topic, code, e)
		}
	}
	if got := storedEvents(t, c, "jobs"); len(got) != 0 {
		t.Fatalf("stored: %q", got)
	}
}

// Two events whose topics can be the same topic would get each other's
// occurrences (bypassing the stricter one's authorize, in the SDKs):
// refused, whether published together or the second merged in later.
func TestOverlappingEventTopicsRefused(t *testing.T) {
	c := coretest.Start(t)
	app := bareApp(t, c, "billing")
	for _, pair := range [][2]string{
		{"acct/{org}/{job}", "acct/{job}/{org}"},
		{"acct/{org}/{job}", "acct/all/{job}"},
		{"acct/{org}/{job}", "{org}/x/{job}"},
		{"acct/{org}/job-{job}", "acct/{org}/{job}-1"},
	} {
		err := app.PublishManifest(ctx, wakeshift.Manifest{Events: []wakeshift.ManifestEvent{jobEvent("invoice.paid", pair[0]), jobEvent("acct.activity", pair[1])}})
		if !wakeshift.IsCode(err, "ambiguous_topic") || !strings.Contains(err.Error(), pair[0]) || !strings.Contains(err.Error(), pair[1]) ||
			!strings.Contains(err.Error(), "invoice.paid") || !strings.Contains(err.Error(), "acct.activity") {
			t.Errorf("publishing %s and %s: %v, want ambiguous_topic naming both", pair[0], pair[1], err)
		}
	}

	// Distinct literal segments are fine, and so are templates of
	// different lengths.
	ok := []wakeshift.ManifestEvent{jobEvent("invoice.paid", "acct/{org}/invoices/{job}"), jobEvent("acct.activity", "acct/{org}/activity/{job}"),
		jobEvent("acct.closed", "acct/{org}/{job}")}
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Events: ok}); err != nil {
		t.Fatalf("distinct topics: %v", err)
	}
	// A merge that adds an event overlapping a stored one is refused, and
	// the stored manifest is kept.
	if code, e := appAPI(t, c, app, "/api/apps/v1/manifest/merge", map[string]any{"events": []wakeshift.ManifestEvent{jobEvent("acct.any", "acct/{org}/x/y/{job}")}}, nil); code != 200 {
		t.Fatalf("merging a longer topic: %d %s", code, e)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/manifest/merge", map[string]any{"events": []wakeshift.ManifestEvent{jobEvent("acct.sneaky", "acct/{org}/invoices/{job}")}}, nil); code != 400 || e != "ambiguous_topic" {
		t.Fatalf("merging an overlapping event: %d %s, want 400 ambiguous_topic", code, e)
	}
	// So is replacing a stored event's topic with one that overlaps another.
	if code, e := appAPI(t, c, app, "/api/apps/v1/manifest/merge", map[string]any{"events": []wakeshift.ManifestEvent{jobEvent("acct.closed", "acct/{org}/activity/{job}")}}, nil); code != 400 || e != "ambiguous_topic" {
		t.Fatalf("merging a changed topic that overlaps: %d %s, want 400 ambiguous_topic", code, e)
	}
	want := []string{"invoice.paid acct/{org}/invoices/{job}", "acct.activity acct/{org}/activity/{job}", "acct.closed acct/{org}/{job}", "acct.any acct/{org}/x/y/{job}"}
	if got := storedEvents(t, c, "billing"); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stored: %q, want %q", got, want)
	}
}

// Valid templates still route each subscription to its own occurrences: a
// placeholder with fixed text beside it in its segment, several segments,
// and values with - and . in them.
func TestValidTopicTemplatesRoute(t *testing.T) {
	c := coretest.Start(t)
	app := bareApp(t, c, "ci")
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Events: []wakeshift.ManifestEvent{
		jobEvent("job.done", "jobs/org-{org}/{job}.log"), jobEvent("job.failed", "jobs/failed/{org}/{job}"),
	}}); err != nil {
		t.Fatal(err)
	}
	sub := func(user, event, org, job string) {
		t.Helper()
		call := map[string]any{"user": user, "client": "claude-code"}
		if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": event, "arguments": map[string]any{"org": org, "job": job}}, nil); code != 200 {
			t.Fatalf("subscribe %s %s %s/%s: %d %s", user, event, org, job, code, e)
		}
	}
	emit := func(event, org, job string) {
		t.Helper()
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/emit", map[string]any{"name": event, "arguments": map[string]any{"org": org, "job": job},
			"payload": map[string]any{"ok": true}, "text": event}, nil); code != 200 {
			t.Fatalf("emit: %d %s", code, e)
		}
	}
	sub("u1", "job.done", "acme-prod", "42")
	sub("u2", "job.done", "acme", "prod-42")
	sub("u3", "job.failed", "acme", "prod-42")
	u1, u2, u3 := userTenant(t, c, "ci", "u1"), userTenant(t, c, "ci", "u2"), userTenant(t, c, "ci", "u3")
	emit("job.done", "acme", "prod-42")
	waitFor(t, "u2's occurrence delivered", func() bool { return deliveryCount(t, c, u2) == 1 })
	emit("job.failed", "acme-prod", "42")
	time.Sleep(300 * time.Millisecond)
	if n1, n2, n3 := deliveryCount(t, c, u1), deliveryCount(t, c, u2), deliveryCount(t, c, u3); n1 != 0 || n2 != 1 || n3 != 0 {
		t.Fatalf("deliveries u1=%d u2=%d u3=%d, want 0 1 0", n1, n2, n3)
	}
}

// A manifest stored before the rule keeps working: republishing it as it
// is (an app does on every start) is accepted, and logged; adding another
// ambiguous topic to it isn't.
func TestStoredAmbiguousTopicsKept(t *testing.T) {
	c := coretest.Start(t)
	app := bareApp(t, c, "legacy")
	old := []wakeshift.ManifestEvent{jobEvent("job.done", "jobs/{org}-{job}"), jobEvent("invoice.paid", "acct/{org}/{job}"), jobEvent("acct.activity", "acct/{job}/{org}")}
	raw, _ := json.Marshal(map[string]any{"description": "Legacy.", "events": old})
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE apps SET manifest = $2, manifest_updated_at = now() WHERE id = $1`, "legacy", raw); err != nil {
		t.Fatal(err)
	}
	if err := c.Apps.LogAmbiguousTopics(ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Description: "Legacy.", Events: old}); err != nil {
		t.Fatalf("republishing the stored manifest: %v", err)
	}
	if code, e := appAPI(t, c, app, "/api/apps/v1/manifest/merge", map[string]any{"description": "Legacy, again."}, nil); code != 200 {
		t.Fatalf("merging into it: %d %s", code, e)
	}
	// It still subscribes and emits.
	call := map[string]any{"user": "u1", "client": "claude-code"}
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "job.done", "arguments": map[string]any{"org": "acme", "job": "1"}}, nil); code != 200 {
		t.Fatalf("subscribing: %d %s", code, e)
	}
	for _, ev := range []wakeshift.ManifestEvent{jobEvent("job.failed", "jobs/{job}{org}"), jobEvent("acct.closed", "acct/{org}/{job}")} {
		if code, e := appAPI(t, c, app, "/api/apps/v1/manifest/merge", map[string]any{"events": []wakeshift.ManifestEvent{ev}}, nil); code != 400 || e != "ambiguous_topic" {
			t.Errorf("merging %s %s: %d %s, want 400 ambiguous_topic", ev.Name, ev.Topic, code, e)
		}
	}
	// Once the app publishes without one, it can't come back.
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Events: old[1:]}); err != nil {
		t.Fatal(err)
	}
	if err := app.PublishManifest(ctx, wakeshift.Manifest{Events: old}); !wakeshift.IsCode(err, "ambiguous_topic") {
		t.Fatalf("publishing a removed ambiguous topic again: %v", err)
	}
}
