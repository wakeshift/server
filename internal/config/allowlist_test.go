package config

import "testing"

func TestParseAllowlist(t *testing.T) {
	for in, want := range map[string]map[string]bool{
		"":             {"poker": false},
		"off":          {"poker": false},
		"all":          {"poker": true, "cards": true},
		"poker":        {"poker": true, "cards": false},
		"poker, cards": {"poker": true, "cards": true, "chess": false},
		" rehearsal ":  {"rehearsal": true},
	} {
		a, err := ParseAllowlist(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		for app, ok := range want {
			if a.Allows(app) != ok {
				t.Errorf("%q allows %s: %v, want %v", in, app, a.Allows(app), ok)
			}
		}
	}
	for _, bad := range []string{"on", "Poker", "poker,,cards", "a", "poker;cards"} {
		if _, err := ParseAllowlist(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
