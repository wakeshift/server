package coretest_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/coretest"
)

// logLines are the JSON log lines with msg, as maps.
func logLines(t *testing.T, logs *syncBuffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(logs.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// Each subscribe logs whether its result carries a beacon for the
// conversation's hooks, and why not; each redeem logs its outcome, the
// app, the harness and the connector. Neither logs the beacon, its ticket,
// the conversation or a session key.
func TestBeaconAndRedeemLogs(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	c := coretest.Start(t)
	app := noteApp(t, c, "ci")

	plain, err := subscribe(app, caller{User: "p1", Client: "my-client"}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	hermes, err := subscribe(app, caller{User: "h1", Client: "mcp", Tool: "watch"}, "ci/acme/web", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	lines := logLines(t, &logs, appapi.BeaconLog)
	if len(lines) < 2 {
		t.Fatalf("beacon lines: %v", lines)
	}
	got := map[string]map[string]any{}
	for _, l := range lines {
		if l["for"] == "subscribe" {
			got[l["client"].(string)] = l
		}
	}
	if l := got["my-client"]; l == nil || l["issued"] != false || l["reason"] != "unrecognized" || l["app"] != "ci" || l["level"] != "INFO" {
		t.Fatalf("unrecognized: %v", l)
	}
	if l := got["mcp"]; l == nil || l["issued"] != true || l["reason"] != "" || l["harness"] != "hermes" || l["via"] != "client" || l["tool"] != "watch" {
		t.Fatalf("hermes: %v", l)
	}

	tok, _ := hermes.Addition.Meta[beacon.MetaKey].(string)
	cl, err := beacon.VerifyBound(tok, beacon.Binding{Conversation: "hermes-session", Unbound: true}, []string{c.URL}, coreKeys(t, c), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	redeem := func(ticket, thread string) int {
		b, _ := json.Marshal(map[string]string{"ticket": ticket, "thread": thread})
		req, _ := http.NewRequest("POST", cl.Redeem, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(connectorversion.Header, "wakeshift-hermes/2026.10.31 hermes")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := redeem(cl.Ticket, "hermes-session"); code != 200 {
		t.Fatalf("redeem: %d", code)
	}
	if code := redeem(cl.Ticket, "hermes-session"); code != 404 { // used
		t.Fatalf("again: %d", code)
	}
	if code := redeem("wst_nope", "hermes-session"); code != 404 {
		t.Fatalf("malformed: %d", code)
	}
	var outcomes []string
	for _, l := range logLines(t, &logs, appapi.RedeemLog) {
		outcomes = append(outcomes, l["outcome"].(string)+"/"+l["why"].(string))
		if l["harness"] != "hermes" || l["connector"] != "wakeshift-hermes/2026.10.31" {
			t.Errorf("redeem line: %v", l)
		}
		if l["outcome"] != "ok" && l["level"] != "WARN" {
			t.Errorf("a refusal at %v", l["level"])
		}
	}
	if strings.Join(outcomes, ",") != "ok/,unknown_ticket/,unknown_ticket/malformed" {
		t.Fatalf("outcomes %v", outcomes)
	}
	all := logs.String()
	for _, secret := range []string{tok, cl.Ticket, "hermes-session", plain.Session, hermes.Session} {
		if secret != "" && strings.Contains(all, secret) {
			t.Fatalf("logged %q", secret)
		}
	}
}
