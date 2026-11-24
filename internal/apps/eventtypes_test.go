package apps

import (
	"encoding/json"
	"strings"
	"testing"
)

func chatMessage() EventType {
	return EventType{
		Name:        "chat.message",
		Description: "A message in a channel.",
		Arguments:   json.RawMessage(`{"type":"object","properties":{"channel":{"type":"string"},"from":{"type":"string"}},"required":["channel"],"additionalProperties":false}`),
		Payload:     json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"from":{"type":"string"}},"required":["text"]}`),
		Topic:       "channels/{channel}",
		Filter:      map[string]string{"from": "{from}"},
	}
}

func TestEventTypesValidate(t *testing.T) {
	ok := Manifest{Events: []EventType{chatMessage()}}
	if err := ok.Validate("chat"); err != nil {
		t.Fatalf("valid manifest refused: %v", err)
	}
	bad := map[string]func(*EventType){
		"reserved name":        func(e *EventType) { e.Name = "wakeshift.message" },
		"bad name":             func(e *EventType) { e.Name = "Chat Message" },
		"no description":       func(e *EventType) { e.Description = "" },
		"no topic":             func(e *EventType) { e.Topic = "" },
		"private topic":        func(e *EventType) { e.Topic = "u/{channel}" },
		"empty segment":        func(e *EventType) { e.Topic = "channels//{channel}" },
		"unknown placeholder":  func(e *EventType) { e.Topic = "channels/{room}" },
		"optional in topic":    func(e *EventType) { e.Topic = "channels/{from}" },
		"no arguments schema":  func(e *EventType) { e.Arguments = nil },
		"no payload schema":    func(e *EventType) { e.Payload = nil },
		"arguments not object": func(e *EventType) { e.Arguments = json.RawMessage(`{"type":"string"}`) },
		"not a schema":         func(e *EventType) { e.Payload = json.RawMessage(`{"type":12}`) },
		"not JSON":             func(e *EventType) { e.Payload = json.RawMessage(`{`) },
		"remote ref":           func(e *EventType) { e.Payload = json.RawMessage(`{"$ref":"https://example.com/s.json"}`) },
		"huge schema": func(e *EventType) {
			e.Payload = json.RawMessage(`{"description":"` + strings.Repeat("x", maxEventSchema) + `"}`)
		},
		"filter literal": func(e *EventType) { e.Filter = map[string]string{"from": "ada"} },
		"filter on a field the payload doesn't have": func(e *EventType) { e.Filter = map[string]string{"sender": "{from}"} },
		"filter unknown arg":                         func(e *EventType) { e.Filter = map[string]string{"from": "{who}"} },
		"object arg in template": func(e *EventType) {
			e.Arguments = json.RawMessage(`{"type":"object","properties":{"channel":{"type":"object"}},"required":["channel"]}`)
		},
		// The subscriber's prefix has to fit within a topic's 200 characters.
		"personal topic too long": func(e *EventType) {
			e.Personal = true
			e.Topic = strings.Repeat("a", 200-personalPrefix-len("/{channel}")+1) + "/{channel}"
		},
	}
	for name, change := range bad {
		e := chatMessage()
		change(&e)
		if err := (Manifest{Events: []EventType{e}}).Validate("chat"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (Manifest{Events: []EventType{chatMessage(), chatMessage()}}).Validate("chat"); err == nil {
		t.Error("a duplicate event name was accepted")
	}
	// A personal topic at the limit is fine, and its subscriber's topic is a
	// valid topic of at most 200 characters.
	e := chatMessage()
	e.Personal = true
	e.Topic = strings.Repeat("a", 200-personalPrefix-len("/{channel}")) + "/{channel}"
	if err := (Manifest{Events: []EventType{e}}).Validate("chat"); err != nil {
		t.Fatalf("a personal topic at the limit: %v", err)
	}
	if p := PersonalTopic("x", "guest:abc"); len(p)-len("x") != personalPrefix || !strings.HasPrefix(p, "u/p") {
		t.Fatalf("PersonalTopic's prefix is %d characters, not %d: %q", len(p)-len("x"), personalPrefix, p)
	}
}

func TestEventSubscribe(t *testing.T) {
	e := chatMessage()
	a, err := e.Subscribe(json.RawMessage(`{"from": "ada", "channel": "eng"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Subscribe(json.RawMessage(` {"channel":"eng","from":"ada"} `))
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Arguments) != `{"channel":"eng","from":"ada"}` || a.Key != b.Key {
		t.Fatalf("not canonical: %s %s / %s", a.Arguments, a.Key, b.Key)
	}
	if a.Topic != "channels/eng" || string(a.Filter["from"]) != `"ada"` {
		t.Fatalf("topic %q filter %v", a.Topic, a.Filter)
	}
	c, err := e.Subscribe(json.RawMessage(`{"channel":"eng"}`))
	if err != nil || len(c.Filter) != 0 || c.Key == a.Key {
		t.Fatalf("an optional argument left out: %v %v", c.Filter, err)
	}
	for _, args := range []string{`[]`, `{"channel":"Eng"}`, `{"channel":"a/b"}`, `{}`, `{"channel":"eng","extra":1}`, `{"channel":3}`, `not json`} {
		if _, err := e.Subscribe(json.RawMessage(args)); err == nil {
			t.Errorf("arguments %s accepted", args)
		} else if _, ok := err.(EventArgsError); !ok {
			t.Errorf("arguments %s: %T, want EventArgsError", args, err)
		}
	}
}

// A number argument is in its canonical form in the topic, as in the key:
// a subscription made with 1.0 is on the topic an emit of 1 goes to.
func TestNumberArgumentTopic(t *testing.T) {
	e := EventType{
		Name:      "ci.run",
		Arguments: json.RawMessage(`{"type":"object","properties":{"pr":{"type":"integer"}},"required":["pr"]}`),
		Payload:   json.RawMessage(`{"type":"object"}`),
		Topic:     "ci/{pr}",
	}
	want, err := e.Target(json.RawMessage(`{"pr":1}`))
	if err != nil || want != "ci/1" {
		t.Fatalf("Target(1) = %q, %v", want, err)
	}
	for _, args := range []string{`{"pr":1.0}`, `{"pr":1e0}`, `{"pr":10e-1}`} {
		sub, err := e.Subscribe(json.RawMessage(args))
		if err != nil || sub.Topic != want {
			t.Errorf("Subscribe(%s) topic %q, %v; want %q", args, sub.Topic, err, want)
		}
		if got, err := e.Target(json.RawMessage(args)); err != nil || got != want {
			t.Errorf("Target(%s) = %q, %v; want %q", args, got, err, want)
		}
	}
}

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		`{"b":1,"a":[1.0, 1e0, -0, "x"]}`: `{"a":[1,1,0,"x"],"b":1}`,
		`{"z":{"y":true,"x":null}}`:       `{"z":{"x":null,"y":true}}`,
		`"éé"`:                            `"éé"`,
	} {
		got, err := Canonical(json.RawMessage(in))
		if err != nil || string(got) != want {
			t.Errorf("Canonical(%s) = %s, %v; want %s", in, got, err, want)
		}
	}
	// Large integers stay exact, so different ones never share a key.
	a, _ := Canonical(json.RawMessage(`{"n":9007199254740992}`))
	b, _ := Canonical(json.RawMessage(`{"n":9007199254740993}`))
	if string(a) == string(b) || string(b) != `{"n":9007199254740993}` {
		t.Errorf("large integers: %s, %s", a, b)
	}
	if got, _ := Canonical(json.RawMessage(`[-0, 7]`)); string(got) != "[0,7]" {
		t.Errorf("integers: %s", got)
	}
	// And so do decimals: equal values meet, different ones don't.
	for in, want := range map[string]string{`[0.10, 1.5e1, 25e-1, -0.0]`: `[0.1,15,2.5,0]`, `[9007199254740992.1]`: `[9007199254740992.1]`} {
		if got, err := Canonical(json.RawMessage(in)); err != nil || string(got) != want {
			t.Errorf("Canonical(%s) = %s, %v; want %s", in, got, err, want)
		}
	}
	x, _ := Canonical(json.RawMessage(`9007199254740992.1`))
	y, _ := Canonical(json.RawMessage(`9007199254740992.2`))
	if string(x) == string(y) {
		t.Error("two different decimals share a form")
	}
	if _, err := Canonical(json.RawMessage(`1e400`)); err == nil {
		t.Error("an out-of-range number was accepted")
	}
	for _, trailing := range []string{`{} {}`, `{}x`, `{} 1`, `[1] [2]`} {
		if _, err := Canonical(json.RawMessage(trailing)); err == nil {
			t.Errorf("trailing data accepted: %s", trailing)
		}
	}
}

func TestEventPayload(t *testing.T) {
	e := chatMessage()
	if err := e.CheckPayload(json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckPayload(json.RawMessage(`{"from":"ada"}`)); err == nil {
		t.Fatal("a payload without its required field was accepted")
	}
}

func TestEventTypeChangesLogged(t *testing.T) {
	old := Manifest{Events: []EventType{chatMessage()}}
	changed := chatMessage()
	changed.Topic = "rooms/{channel}"
	got := manifestChanges(old, Manifest{Events: []EventType{changed, {Name: "chat.joined"}}})
	want := []string{"event chat.message changed", "event chat.joined added"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("changes %q, want %q", got, want)
	}
	if got := manifestChanges(old, Manifest{}); len(got) != 1 || got[0] != "event chat.message removed" {
		t.Fatalf("removal: %q", got)
	}
}
