package grants

import (
	"strings"
	"testing"

	"github.com/wakeshift/server/internal/apps"
)

func TestAgentWarning(t *testing.T) {
	app := apps.App{Name: "Nightfall"}
	for name, c := range map[string]struct {
		risks []Risk
		level string
		has   string
	}{
		"strangers":       {[]Risk{{Key: "peers", Summary: "y can post where x listens."}, {Key: "strangers", Summary: "People you don't know can send x messages."}}, "caution", "People you don't know"},
		"else a reminder": {[]Risk{{Key: "peers", Summary: "y can post where x listens."}, {Key: "act"}, {Key: "expiry"}}, "info", "Messages from Nightfall can lead x to act"},
	} {
		w := AgentWarning(app, "x", c.risks)
		if w.Level != c.level || !strings.Contains(w.Text, c.has) {
			t.Errorf("%s: %+v", name, w)
		}
	}
	if got := Pacing(apps.DefaultWakeLimits); got != "10 a second, 450 per 5 minutes and 3,600 an hour" {
		t.Errorf("pacing %q", got)
	}
}
