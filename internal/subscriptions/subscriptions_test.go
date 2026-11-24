package subscriptions

import (
	"testing"

	"github.com/wakeshift/server/internal/events"
)

// Topic filters: data fields compare as canonical JSON, $once doesn't
// filter, and an event for one subscription ($subscription) reaches only
// it, whatever its data filters.
func TestTopicMatching(t *testing.T) {
	sub := Subscription{ID: "s1", Status: Active, EventType: "topics.ci",
		Filters: map[string]string{"$topic:ci/x": "1", "$data.branch": `"main"`, OnceFilter: "true"}}
	ev := func(data string, attrs map[string]string) events.Event {
		if attrs == nil {
			attrs = map[string]string{}
		}
		attrs["$topic:ci/x"] = "1"
		e := events.Event{Seq: 1, Type: "topics.ci", Attrs: attrs, From: events.Party{Kind: "app", ID: "ci"}}
		if data != "" {
			e.Data = []byte(data)
		}
		return e
	}
	for _, c := range []struct {
		name string
		e    events.Event
		want bool
	}{
		{"matching data", ev(`{"branch":"main"}`, nil), true},
		{"other data", ev(`{"branch":"dev"}`, nil), false},
		{"no data", ev(``, nil), false},
		{"a number where a string is wanted", ev(`{"branch":1}`, nil), false},
		{"for this subscription, whatever its data", ev(`{"branch":"dev"}`, map[string]string{ForAttr: "s1"}), true},
		{"for another subscription", ev(`{"branch":"main"}`, map[string]string{ForAttr: "s2"}), false},
	} {
		if got := sub.Matches(c.e); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
	other := ev(`{"branch":"main"}`, nil)
	delete(other.Attrs, "$topic:ci/x")
	if sub.Matches(other) {
		t.Error("an event on another topic matched")
	}
	for raw, want := range map[string]string{`"a"`: `"a"`, `1.0`: `1`, `true`: `true`, `null`: `null`, `"<"`: "\"\\u003c\""} {
		if got, ok := CanonicalJSON([]byte(raw)); !ok || got != want {
			t.Errorf("CanonicalJSON(%s) = %q %v", raw, got, ok)
		}
	}
	for _, raw := range []string{`{}`, `[]`, ``, `nope`} {
		if _, ok := CanonicalJSON([]byte(raw)); ok {
			t.Errorf("CanonicalJSON(%s) comparable", raw)
		}
	}
}

// A membership's old $wake filter, left on a subscription, is a key no
// event carries: whatever its value, the subscription matches nothing, not
// even a mention or a send to it.
func TestOldWakeFiltersMatchNothing(t *testing.T) {
	ev := func(mentions []string, attrs map[string]string) events.Event {
		return events.Event{Seq: 1, Type: "app.g.room", Mentions: mentions, Attrs: attrs, From: events.Party{Kind: "app", ID: "g"}}
	}
	for _, wake := range []string{"all", "mentions", "none"} {
		sub := Subscription{ID: "s1", Status: Active, AgentID: "claude", EventType: "app.g.room", Filters: map[string]string{"$wake": wake}}
		for _, e := range []events.Event{ev(nil, nil), ev([]string{"claude"}, nil), ev(nil, map[string]string{ForAttr: "s1"})} {
			if sub.Matches(e) {
				t.Errorf("$wake %s matched mentions %v, attrs %v", wake, e.Mentions, e.Attrs)
			}
		}
	}
	if !(Subscription{ID: "s1", Status: Active, AgentID: "claude", EventType: "app.g.room"}).Matches(ev(nil, nil)) {
		t.Error("without the filter, it doesn't match")
	}
}
