package console_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connector"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/version"
)

// The app's overview shows the connector versions its conversations use,
// per harness, marking those Wakeshift refuses; its activity shows the
// connector each delivery was handed to.
func TestAppConnectorVersions(t *testing.T) {
	was := version.Version
	version.Version = "2026.10.20.1435+abc1234"
	defer func() { version.Version = was }()
	c := coretest.StartWith(t, func(o *core.Options) {
		o.Connectors = connectorversion.Minimums{"wakeshift:cursor": "2026.10.21"}
	})
	ctx := context.Background()
	ada := c.SignIn("ada@example.com")
	made := createApp(t, ada, "quizzer", "Quizzer", "")
	app := noted(t, c, made.Key)

	const thread = "019a14bd-bbbc-7b70-a723-0abb9f383333"
	sub, err := coretest.Subscribe(app, coretest.Caller{User: "player-1", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}},
		"game/1", coretest.SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := sub.Addition.Meta[beacon.MetaKey].(string)
	var claims beacon.Claims
	if parts := strings.Split(tok, "."); len(parts) == 3 {
		b, _ := base64.RawURLEncoding.DecodeString(parts[1])
		json.Unmarshal(b, &claims)
	}
	link, err := connector.Redeem(ctx, nil, "codex", claims, thread)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coretest.EmitNote(app, []string{"game/1"}, coretest.Note{Text: "your move"}); err != nil {
		t.Fatal(err)
	}
	p := &connector.HTTPPoller{Harness: "codex", Issuers: []string{c.URL}}
	if b, err := p.Poll(ctx, link, 5*time.Second, false); err != nil || len(b.Deliveries) != 1 {
		t.Fatalf("poll: %+v %v", b, err)
	}
	// A Cursor conversation whose hooks are older than Wakeshift serves.
	version.Version = "2026.10.19.0000+old"
	// Its agent passes its conversation ID, which a beacon in text needs.
	cur, err := coretest.Subscribe(app, coretest.Caller{User: "player-2", Client: "Cursor", Conversation: "cursor-conv-1"}, "game/1", coretest.SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if tok = beacon.InText(cur.Addition.Text); tok == "" {
		t.Fatalf("no beacon for Cursor: %s", cur.Addition.Text)
	} else {
		b, _ := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
		claims = beacon.Claims{}
		json.Unmarshal(b, &claims)
	}
	if _, err := connector.RedeemCall(ctx, nil, "cursor", claims, "cursor-conv-1", ""); err == nil {
		t.Fatal("an outdated Cursor hook connected")
	}

	var overview struct {
		Connectors []struct {
			Harness, Connector, Version string
			Conversations               int
			Outdated                    bool
		}
	}
	if code := ada.Do("GET", "/api/v1/developer/apps/quizzer/overview", nil, &overview); code != 200 || len(overview.Connectors) != 2 {
		t.Fatalf("overview: %d %+v", code, overview)
	}
	if got := overview.Connectors[0]; got.Harness != "codex" || got.Connector != "wakeshift" || got.Version != "2026.10.20.1435+abc1234" || got.Conversations != 1 || got.Outdated {
		t.Fatalf("codex: %+v", got)
	}
	if got := overview.Connectors[1]; got.Harness != "cursor" || got.Version != "2026.10.19.0000+old" || !got.Outdated {
		t.Fatalf("cursor: %+v", got)
	}

	var act struct {
		Activity []struct{ Kind, Text, Connector string }
	}
	ada.Do("GET", "/api/v1/developer/apps/quizzer/activity?status=waiting", nil, &act)
	found := false
	for _, e := range act.Activity {
		if e.Kind == "delivery" && e.Connector == "wakeshift/2026.10.20.1435+abc1234 codex" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no delivery handed to the connector: %+v", act.Activity)
	}
}
