package coretest_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/dblimit"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// verifications is how many verification POSTs f has answered.
func (f *fakeChatGPT) verifications() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.verified)
}

func (f *fakeChatGPT) setEcho(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.echo = on
}

// webhookSub subscribes user to chat.message in channel by webhook at url,
// through the app API as an app's SDK does, returning the status, the
// error's code and reason, and the result.
func webhookSub(t *testing.T, c *coretest.Core, app *wakeshift.Client, user, channel, url, secret string) (int, string, string, map[string]any) {
	t.Helper()
	code, raw, err := appPostRaw(c, app, "/api/apps/v1/events/subscribe", map[string]any{
		"call": map[string]any{"user": user, "client": "openai-mcp", "user_agent": "openai-mcp/1.0.0"},
		"name": "chat.message", "arguments": map[string]any{"channel": channel},
		"delivery": map[string]any{"mode": "webhook", "url": url, "secret": secret}, "ttl_ms": 3600000})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(raw, &out)
	e, _ := out["error"].(map[string]any)
	ec, _ := e["code"].(string)
	reason, _ := e["reason"].(string)
	return code, ec, reason, out
}

// SEP-3415 (Endpoint verification): verification is cached per (principal,
// url). A principal's new subscription to a callback it has a live,
// verified subscription at isn't challenged again, whatever its arguments;
// another principal's is, as is one in another app, one to another URL,
// and one after the principal's subscriptions there ended or lapsed.
func TestVerificationPerPrincipalAndURL(t *testing.T) {
	eventsTestSetup(t)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	other := eventsApp(t, c, "notes")
	hook := newFakeChatGPT(t)
	secret := hook.secret()
	step := func(what string, app *wakeshift.Client, user, channel, url string, want int) {
		t.Helper()
		if code, e, _, _ := webhookSub(t, c, app, user, channel, url, secret); code != 200 {
			t.Fatalf("%s: %d %s", what, code, e)
		}
		if got := hook.verifications(); got != want {
			t.Fatalf("%s: %d verification POSTs, want %d", what, got, want)
		}
	}
	unsub := func(user, channel string) {
		t.Helper()
		if code, e := appAPI(t, c, app, "/api/apps/v1/events/unsubscribe", map[string]any{
			"call": map[string]any{"user": user, "client": "openai-mcp", "user_agent": "openai-mcp/1.0.0"},
			"name": "chat.message", "arguments": map[string]any{"channel": channel},
			"delivery": map[string]any{"mode": "webhook", "url": hook.url()}}, nil); code != 200 {
			t.Fatalf("unsubscribe: %d %s", code, e)
		}
	}

	step("u1's first subscription", app, "u1", "eng", hook.url(), 1)
	step("u1, other arguments, the same URL", app, "u1", "ops", hook.url(), 1)
	step("u1, a third set of arguments", app, "u1", "dev", hook.url(), 1)
	step("u1 refreshing", app, "u1", "eng", hook.url(), 1)
	step("u2 at the same URL: another principal", app, "u2", "eng", hook.url(), 2)
	step("u1 in another app", other, "u1", "eng", hook.url(), 3)
	step("u1 at another URL on the same host", app, "u1", "eng", hook.url()+"/other", 4)

	// A callback that stops echoing: u1's cached verification still covers
	// its own URL; u3's first subscription there is challenged, and fails.
	hook.setEcho(false)
	step("u1, cached, the callback no longer echoing", app, "u1", "qa", hook.url(), 4)
	if code, e, reason, _ := webhookSub(t, c, app, "u3", "eng", hook.url(), secret); code != 400 || e != "callback_failed" ||
		reason != mcpevents.ReasonChallenge || hook.verifications() != 5 {
		t.Fatalf("u3, no echo: %d %s %s, %d POSTs", code, e, reason, hook.verifications())
	}
	hook.setEcho(true)

	// Every subscription of u1's at the URL ended: challenged again.
	for _, ch := range []string{"eng", "ops", "dev", "qa"} {
		unsub("u1", ch)
	}
	step("u1 after its subscriptions there ended", app, "u1", "eng", hook.url(), 6)

	// Lapsed (past refreshBefore, not swept yet): challenged again.
	if err := c.DB.InTenant(ctx, userTenant(t, c, "chat", "u1"), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE mcp_event_webhooks SET expires_at = now() - interval '1 second' WHERE url = $1`, hook.url())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	step("u1 after its subscription there lapsed", app, "u1", "ops", hook.url(), 7)
}

// setVerifyLimits lowers the verification limits for a test.
func setVerifyLimits(t *testing.T, perPrincipal, perHost int) {
	p, h := appapi.VerifyPerPrincipalHost, appapi.VerifyPerHost
	appapi.VerifyPerPrincipalHost = dblimit.Limit{Name: p.Name, Max: perPrincipal, Window: p.Window}
	appapi.VerifyPerHost = dblimit.Limit{Name: h.Name, Max: perHost, Window: h.Window}
	t.Cleanup(func() { appapi.VerifyPerPrincipalHost, appapi.VerifyPerHost = p, h })
}

// refusedAtLimit fails the test unless a subscribe was refused as
// challenge_failed, with a message that tells no one to retry.
func refusedAtLimit(t *testing.T, what string, code int, e, reason string, out map[string]any) {
	t.Helper()
	if code != 400 || e != "callback_failed" || reason != mcpevents.ReasonChallenge {
		t.Fatalf("%s: %d %s %s %v", what, code, e, reason, out)
	}
	msg, _ := out["error"].(map[string]any)["message"].(string)
	for _, w := range []string{"again", "retry"} {
		if strings.Contains(strings.ToLower(msg), w) {
			t.Errorf("%s: the refusal says %q: %s", what, w, msg)
		}
	}
}

// SEP-3415 (Endpoint verification): verification POSTs are rate-limited
// per destination host, counted per principal first. One principal sending
// failing subscribes at a shared callback host uses up only its own count:
// past it, its subscribes there are refused as challenge_failed and nothing
// is sent, while another principal's verification at the same host goes
// through, and the flooding principal can still verify at another host.
// The path, query and port don't make another host.
func TestVerificationPerPrincipalHostLimit(t *testing.T) {
	eventsTestSetup(t)
	setVerifyLimits(t, 2, 100)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	hook := newFakeChatGPT(t)
	hook2 := newFakeChatGPT(t) // on 127.0.0.1 too, another port
	secret := hook.secret()
	hook2.secrets = hook.secrets
	byName := strings.Replace(hook.url(), "127.0.0.1", "localhost", 1)

	// u1 floods the shared host with subscribes that fail verification.
	hook.setEcho(false)
	for i, ch := range []string{"a", "b"} {
		if code, e, reason, _ := webhookSub(t, c, app, "u1", ch, hook.url(), secret); code != 400 || reason != mcpevents.ReasonChallenge || hook.verifications() != i+1 {
			t.Fatalf("u1's failing subscribe %d: %d %s %s", i, code, e, reason)
		}
	}
	for _, url := range []string{hook.url(), hook.url() + "/elsewhere?x=1", hook2.url()} {
		code, e, reason, out := webhookSub(t, c, app, "u1", "c", url, secret)
		refusedAtLimit(t, "u1 past its limit at "+url, code, e, reason, out)
		if hook.verifications() != 2 || hook2.verifications() != 0 {
			t.Fatalf("u1 past its limit: %d+%d POSTs", hook.verifications(), hook2.verifications())
		}
	}
	// Another principal at the same host isn't held up.
	hook.setEcho(true)
	if code, e, _, _ := webhookSub(t, c, app, "u2", "eng", hook.url(), secret); code != 200 || hook.verifications() != 3 {
		t.Fatalf("u2 at the host u1 flooded: %d %s, %d POSTs", code, e, hook.verifications())
	}
	// u1 at another host: its own count there.
	if code, e, _, _ := webhookSub(t, c, app, "u1", "eng", byName, secret); code != 200 || hook.verifications() != 4 {
		t.Fatalf("u1 at another host: %d %s, %d POSTs", code, e, hook.verifications())
	}
	// u2's cached verification sends nothing, so it isn't counted.
	for _, ch := range []string{"ops", "dev", "qa"} {
		if code, e, _, _ := webhookSub(t, c, app, "u2", ch, hook.url(), secret); code != 200 || hook.verifications() != 4 {
			t.Fatalf("u2, cached: %d %s, %d POSTs", code, e, hook.verifications())
		}
	}
}

// The per-host backstop: past VerifyPerHost at a host, summed over
// principals, a new principal's verification there is refused too, and
// nothing is sent; another host isn't held up, and a cached verification
// sends nothing. A principal's refusals at its own limit don't count
// against the host.
func TestVerificationPerHostBackstop(t *testing.T) {
	eventsTestSetup(t)
	setVerifyLimits(t, 2, 3)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	hook := newFakeChatGPT(t)
	secret := hook.secret()
	byName := strings.Replace(hook.url(), "127.0.0.1", "localhost", 1)

	// u1 takes 2 POSTs (failing), then is refused at its own limit, many
	// times: those refusals send nothing and leave the host one POST.
	hook.setEcho(false)
	for _, ch := range []string{"a", "b"} {
		webhookSub(t, c, app, "u1", ch, hook.url(), secret)
	}
	for i := range 5 {
		code, e, reason, out := webhookSub(t, c, app, "u1", fmt.Sprint("x", i), hook.url(), secret)
		refusedAtLimit(t, "u1 past its limit", code, e, reason, out)
	}
	hook.setEcho(true)
	if code, e, _, _ := webhookSub(t, c, app, "u2", "eng", hook.url(), secret); code != 200 || hook.verifications() != 3 {
		t.Fatalf("u2, the host's last POST: %d %s, %d POSTs", code, e, hook.verifications())
	}
	// The host's backstop is used up: new principals are refused, each
	// under its own limit, and nothing is sent.
	for _, u := range []string{"u3", "u4"} {
		code, e, reason, out := webhookSub(t, c, app, u, "eng", hook.url(), secret)
		refusedAtLimit(t, u+" past the host's backstop", code, e, reason, out)
		if hook.verifications() != 3 {
			t.Fatalf("%s past the host's backstop: %d POSTs", u, hook.verifications())
		}
	}
	// A cached verification sends nothing, so it isn't held up.
	if code, e, _, _ := webhookSub(t, c, app, "u2", "ops", hook.url(), secret); code != 200 || hook.verifications() != 3 {
		t.Fatalf("u2, cached, at a host past its backstop: %d %s", code, e)
	}
	// Another host name: its own counts.
	if code, e, _, _ := webhookSub(t, c, app, "u3", "eng", byName, secret); code != 200 || hook.verifications() != 4 {
		t.Fatalf("another host: %d %s, %d POSTs", code, e, hook.verifications())
	}
	// Refusals at the backstop didn't use up u4's own count: with room at
	// the host again, u4 verifies its full two.
	for range 3 {
		code, e, reason, out := webhookSub(t, c, app, "u4", "eng", hook.url(), secret)
		refusedAtLimit(t, "u4 past the host's backstop, again", code, e, reason, out)
	}
	appapi.VerifyPerHost.Max = 100
	for i, ch := range []string{"eng", "ops"} {
		if code, e, _, _ := webhookSub(t, c, app, "u4", ch, hook.url()+"/"+ch, secret); code != 200 || hook.verifications() != 5+i {
			t.Fatalf("u4's verification %d after the backstop: %d %s, %d POSTs", i, code, e, hook.verifications())
		}
	}
}
