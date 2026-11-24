package apps

import (
	"fmt"
	"strings"
	"testing"
)

// What a manifest publish changed, as the app log shows it.
func TestManifestChanges(t *testing.T) {
	ev := func(p, d string) ScopeEvent { return ScopeEvent{Pattern: p, Description: d} }
	m := Manifest{Description: "Quiz", Agent: &AgentScope{Wake: []ScopeEvent{ev("app.q.a", "A")}, TTLDays: 30}}
	if got := manifestChanges(m, m); len(got) != 0 {
		t.Fatalf("unchanged: %v", got)
	}
	if got := strings.Join(manifestChanges(Manifest{}, m), "; "); got != "description changed; agent scope added; wake + app.q.a; agent ttl_days: 0 -> 30" {
		t.Fatalf("first: %s", got)
	}
	n := Manifest{Description: "Quiz",
		Agent: &AgentScope{Wake: []ScopeEvent{ev("app.q.a", "Changed"), ev("app.q.b", "")}, Post: []ScopeEvent{ev("app.q.c", "")}, TTLDays: 7}}
	want := "wake ~ app.q.a (description); wake + app.q.b; post + app.q.c; agent ttl_days: 30 -> 7"
	if got := strings.Join(manifestChanges(m, n), "; "); got != want {
		t.Fatalf("changed:\n got %s\nwant %s", got, want)
	}
	if got := manifestChanges(n, Manifest{Description: "Quiz"}); strings.Join(got, "; ") != "agent scope removed" {
		t.Fatalf("removed: %v", got)
	}
	var many []ScopeEvent
	for i := range 30 {
		many = append(many, ev(fmt.Sprintf("app.q.e%d", i), ""))
	}
	got := manifestChanges(Manifest{Agent: &AgentScope{}}, Manifest{Agent: &AgentScope{Wake: many}})
	if len(got) != maxChanges+1 || got[maxChanges] != "and 10 more" {
		t.Fatalf("many: %d %v", len(got), got[len(got)-1])
	}
}

// The tools an app offers to pre-approve are checked (names an exact rule
// can hold, at most 20, none twice) and their changes are in its log.
func TestPreapproveTools(t *testing.T) {
	if err := (Manifest{PreapproveTools: []string{"history", "list_channels", "post", "wait_for_messages", "a-b"}}).Validate("chat"); err != nil {
		t.Fatal(err)
	}
	many := make([]string, MaxPreapproveTools+1)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	for name, l := range map[string][]string{
		"a glob": {"post", "*"}, "a server prefix": {"chat:post"}, "a dot": {"a.b"}, "a space": {"a b"}, "empty": {""},
		"too long": {strings.Repeat("a", 65)}, "twice": {"post", "post"}, "too many": many, "a double underscore": {"a__b"},
	} {
		err := (Manifest{PreapproveTools: l}).Validate("chat")
		if err == nil || !strings.Contains(err.Error(), "preapprove_tools") {
			t.Errorf("%s: %v", name, err)
		}
	}
	got := manifestChanges(Manifest{}, Manifest{PreapproveTools: []string{"history", "post"}})
	if len(got) != 1 || got[0] != "preapprove tools: none -> history, post" {
		t.Fatalf("%q", got)
	}
	if got := manifestChanges(Manifest{PreapproveTools: []string{"post"}}, Manifest{PreapproveTools: []string{"post"}}); len(got) != 0 {
		t.Fatalf("unchanged: %q", got)
	}
}

// Turning OthersCanWake on or off is in the app's log, as Groups is: it
// changes what the consent screen tells people.
func TestManifestChangesOthersCanWake(t *testing.T) {
	if got := manifestChanges(Manifest{}, Manifest{OthersCanWake: true}); len(got) != 1 || got[0] != "others can wake: false -> true" {
		t.Fatalf("on: %q", got)
	}
	if got := manifestChanges(Manifest{OthersCanWake: true}, Manifest{}); len(got) != 1 || got[0] != "others can wake: true -> false" {
		t.Fatalf("off: %q", got)
	}
}
