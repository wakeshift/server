package apps

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// jobEvent is an event type with string arguments a and b (both required),
// on topic.
func jobEvent(name, topic string) EventType {
	return EventType{
		Name:        name,
		Description: "A job.",
		Arguments:   json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a","b"]}`),
		Payload:     json.RawMessage(`{"type":"object"}`),
		Topic:       topic,
	}
}

// Two placeholders in one segment render one topic from different
// arguments: the collision the rule refuses.
func TestTwoPlaceholdersInASegmentCollide(t *testing.T) {
	e := jobEvent("job.done", "jobs/{a}-{b}")
	x, err := e.Target(json.RawMessage(`{"a":"acme-prod","b":"42"}`))
	if err != nil {
		t.Fatal(err)
	}
	y, err := e.Target(json.RawMessage(`{"a":"acme","b":"prod-42"}`))
	if err != nil {
		t.Fatal(err)
	}
	if x != y {
		t.Fatalf("expected the collision: %q, %q", x, y)
	}
	var amb AmbiguousTopicError
	if err := (Manifest{Events: []EventType{e}}).Validate("jobs"); !errors.As(err, &amb) || amb.Topic != "jobs/{a}-{b}" || !strings.Contains(err.Error(), "jobs/{a}-{b}") {
		t.Fatalf("an ambiguous template: %v", err)
	}
}

func TestAmbiguousTopics(t *testing.T) {
	for _, topic := range []string{"jobs/{a}-{b}", "jobs/{a}.{b}", "jobs/{a}_{b}", "jobs/{a}{b}", "{a}x{b}", "j-{a}-{b}-k/x", "jobs/{a}-{a}"} {
		if got := AmbiguousTopics([]EventType{jobEvent("job.done", topic)}); len(got) != 1 || got[0].Other != "" {
			t.Errorf("%s: %+v, want it refused on its own", topic, got)
		}
	}
	for _, topic := range []string{"jobs/{a}/{b}", "jobs/job-{a}/{b}.log", "{a}/{b}", "ev/job.done/{a}/{b}", "jobs/{a}/{a}", "jobs"} {
		if got := AmbiguousTopics([]EventType{jobEvent("job.done", topic)}); len(got) != 0 {
			t.Errorf("%s: %+v, want it accepted", topic, got)
		}
	}

	pairs := []struct {
		a, b    string
		overlap bool
	}{
		{"acct/{a}", "acct/{b}", true},
		{"acct/{a}", "acct/all", true},
		{"acct/{a}", "{a}/x", true},
		{"{a}", "jobs", true},
		{"acct/x-{a}", "acct/{a}-y", true},
		{"acct/ab{a}", "acct/{a}ba", true},
		{"acct/x-{a}", "acct/x-", false}, // a value is at least one character
		{"acct/x-{a}", "acct/x-1", true},
		{"acct/x-{a}", "acct/y-{a}", false},
		{"acct/{a}-x", "acct/{a}-y", false},
		{"acct/{a}", "acct/{a}/{b}", false},
		{"acct/{a}", "accts/{a}", false},
		{"ev/invoice.paid/{a}", "ev/acct.activity/{a}", false},
		{"ev/a/{a}", "ev/{a}/{b}", true},
	}
	for _, p := range pairs {
		got := AmbiguousTopics([]EventType{jobEvent("one", p.a), jobEvent("two", p.b)})
		if (len(got) == 1 && got[0].Other == "two") != p.overlap || len(got) > 1 {
			t.Errorf("%s and %s: %+v, want overlap %v", p.a, p.b, got, p.overlap)
		}
	}
	// A personal event's topics are under the subscriber's u/p…/, so it
	// meets only other personal events.
	shared, mine := jobEvent("one", "acct/{a}"), jobEvent("two", "acct/{a}")
	mine.Personal = true
	if got := AmbiguousTopics([]EventType{shared, mine}); len(got) != 0 {
		t.Errorf("a personal and a shared event: %+v", got)
	}
	shared.Personal = true
	if got := AmbiguousTopics([]EventType{shared, mine}); len(got) != 1 {
		t.Errorf("two personal events on one template: %+v", got)
	}
}

// segmentsOverlap is exact for one-placeholder segments: it agrees with a
// search of every rendering, over a small alphabet and values long enough
// to reach any overlap these segments can have.
func TestSegmentsOverlapExhaustive(t *testing.T) {
	lits := []string{"", "x", "-", "x-", "-x", "xx", "--"}
	var values []string
	var grow func(string)
	grow = func(s string) {
		if s != "" {
			values = append(values, s)
		}
		if len(s) < 6 {
			grow(s + "x")
			grow(s + "-")
		}
	}
	grow("")
	type seg struct {
		shape topicShape
		texts map[string]bool
	}
	var segs []seg
	for _, pre := range lits {
		if pre != "" {
			segs = append(segs, seg{topicShape{pre: pre}, map[string]bool{pre: true}})
		}
		for _, suf := range lits {
			texts := map[string]bool{}
			for _, v := range values {
				texts[pre+v+suf] = true
			}
			segs = append(segs, seg{topicShape{pre: pre, suf: suf, holes: 1}, texts})
		}
	}
	for _, a := range segs {
		for _, b := range segs {
			want := false
			for s := range a.texts {
				if b.texts[s] {
					want = true
					break
				}
			}
			if got := segmentsOverlap(a.shape, b.shape); got != want {
				t.Fatalf("%+v and %+v: overlap %v, want %v", a.shape, b.shape, got, want)
			}
		}
	}
}
