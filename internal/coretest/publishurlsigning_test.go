package coretest_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/apps"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/jws"
	"github.com/wakeshift/server/internal/mcpevents"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// appDo is an app API request with any method: the status and the body.
func appDo(t *testing.T, c *coretest.Core, app *wakeshift.Client, method, path string, body any) (int, map[string]any, string) {
	t.Helper()
	jti := make([]byte, 16)
	rand.Read(jti)
	now := time.Now()
	tok, err := jws.Sign(coretest.CredsOf(app).Key, "", "app+jwt", map[string]any{"iss": coretest.CredsOf(app).ID, "sub": coretest.CredsOf(app).ID, "aud": c.URL + "/api/apps",
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "jti": base64.RawURLEncoding.EncodeToString(jti)})
	if err != nil {
		t.Fatal(err)
	}
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.URL+path, r)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

func newWhsec(t *testing.T) (string, []byte) {
	t.Helper()
	key := make([]byte, 24)
	rand.Read(key)
	return "whsec_" + base64.StdEncoding.EncodeToString(key), key
}

// signed is Standard Webhooks headers for body, signed with keys at ts,
// under prefix ("webhook-" or Svix's "svix-").
func signed(prefix, id string, ts time.Time, body string, keys ...[]byte) map[string]string {
	return map[string]string{
		"Content-Type":       "application/json",
		prefix + "id":        id,
		prefix + "timestamp": strconv.FormatInt(ts.Unix(), 10),
		prefix + "signature": mcpevents.Sign(id, ts.Unix(), []byte(body), keys...),
	}
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

// A publish URL with a signing secret (a Svix endpoint's, say) takes only
// POSTs signed with it, Standard Webhooks' way or Svix's header names,
// within 5 minutes, and dedupes them by the signed webhook-id; rotating it
// keeps the previous secret for a day; removing it takes unsigned POSTs
// again; one without a secret is unchanged. The secret never reaches a log
// or a response: only its fingerprint does.
func TestSignedPublishURL(t *testing.T) {
	var logs lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	var sub eventSubResult
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"},
		"name": "chat.message", "arguments": json.RawMessage(`{"channel":"eng"}`)}, &sub); code != 200 {
		t.Fatalf("subscribe: %d", code)
	}
	tenant := userTenant(t, c, "chat", "u1")
	secret, key := newWhsec(t)
	secret2, key2 := newWhsec(t)
	var responses []string

	// Made with a secret: the response has its fingerprint, not the secret.
	code, made, raw := appDo(t, c, app, "POST", "/api/apps/v1/events/publish-urls", map[string]any{"name": "chat.message",
		"arguments": map[string]any{"channel": "eng"}, "signing_secret": secret})
	responses = append(responses, raw)
	signing, _ := made["signing"].(map[string]any)
	if code != 200 || signing == nil || len(signing["fingerprint"].(string)) != 12 {
		t.Fatalf("made: %d %s", code, raw)
	}
	url, id := made["url"].(string), made["id"].(string)
	body := `{"text": "deployed", "from": "ci"}`
	now := time.Now()

	// Signed: emitted once per webhook-id, whatever Idempotency-Key says.
	if code, out := post(t, url, body, signed("webhook-", "msg_1", now, body, key)); code != 202 || out["matched"] != 1.0 {
		t.Fatalf("signed: %d %v", code, out)
	}
	replay := signed("webhook-", "msg_1", now, body, key)
	replay["Idempotency-Key"] = "fresh"
	if code, out := post(t, url, body, replay); code != 202 || out["duplicate"] != true {
		t.Fatalf("the same webhook-id: %d %v", code, out)
	}
	// Svix's header names.
	if code, out := post(t, url, body, signed("svix-", "msg_2", now, body, key)); code != 202 || out["duplicate"] != false {
		t.Fatalf("svix headers: %d %v", code, out)
	}
	if code, out := post(t, url, body, signed("svix-", "msg_2", now, body, key)); code != 202 || out["duplicate"] != true {
		t.Fatalf("svix retry: %d %v", code, out)
	}
	// Several signatures, one of them ours.
	if code, out := post(t, url, body, signed("webhook-", "msg_3", now, body, key2, key)); code != 202 || out["duplicate"] != false {
		t.Fatalf("several signatures: %d %v", code, out)
	}
	waitFor(t, "the deliveries", func() bool { return deliveryCount(t, c, tenant) == 3 })

	// Refused, and nothing emitted: unsigned, the wrong secret, a tampered
	// body, an old or future timestamp, a missing header.
	missing := signed("webhook-", "msg_4", now, body, key)
	delete(missing, "webhook-timestamp")
	for name, tc := range map[string]struct {
		body string
		hdr  map[string]string
	}{
		"unsigned":         {body, map[string]string{"Content-Type": "application/json", "Idempotency-Key": "k"}},
		"wrong secret":     {body, signed("webhook-", "msg_4", now, body, key2)},
		"tampered body":    {`{"text": "deployed!", "from": "ci"}`, signed("webhook-", "msg_4", now, body, key)},
		"old timestamp":    {body, signed("webhook-", "msg_4", now.Add(-6*time.Minute), body, key)},
		"future timestamp": {body, signed("webhook-", "msg_4", now.Add(6*time.Minute), body, key)},
		"no timestamp":     {body, missing},
	} {
		if code, out := post(t, url, tc.body, tc.hdr); code != 401 || errCode(out) != "invalid_signature" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if n := deliveryCount(t, c, tenant); n != 3 {
		t.Fatalf("%d deliveries after refused posts, want 3", n)
	}

	// A bad secret is refused, and doesn't say what it was.
	code, out, raw := appDo(t, c, app, "PUT", "/api/apps/v1/events/publish-urls/"+id+"/signing-secret", map[string]any{"secret": "whsec_short"})
	if code != 400 || errCode(out) != "bad_signing_secret" || strings.Contains(raw, "short") {
		t.Fatalf("a bad secret: %d %s", code, raw)
	}

	// Rotated: both secrets work for a day, then only the new one.
	code, out, raw = appDo(t, c, app, "PUT", "/api/apps/v1/events/publish-urls/"+id+"/signing-secret", map[string]any{"secret": secret2})
	responses = append(responses, raw)
	signing, _ = out["signing"].(map[string]any)
	if code != 200 || signing == nil || signing["previous_valid_until"] == nil || signing["fingerprint"] == made["signing"].(map[string]any)["fingerprint"] {
		t.Fatalf("rotating: %d %s", code, raw)
	}
	for i, k := range [][]byte{key, key2} {
		mid := "msg_rot" + strconv.Itoa(i)
		if code, out := post(t, url, body, signed("webhook-", mid, now, body, k)); code != 202 {
			t.Fatalf("during the grace, key %d: %d %v", i, code, out)
		}
	}
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE topic_publish_urls SET previous_signing_until = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if code, _ := post(t, url, body, signed("webhook-", "msg_old", now, body, key)); code != 401 {
		t.Fatalf("the old secret after the grace: %d", code)
	}
	if code, _ := post(t, url, body, signed("webhook-", "msg_new", now, body, key2)); code != 202 {
		t.Fatalf("the new secret after the grace: %d", code)
	}

	// Setting the same secret again changes nothing.
	var rotations int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM app_log WHERE app_id = 'chat' AND action = $1`, apps.LogPublishURLSecretRotated).Scan(&rotations)
	if code, _, raw := appDo(t, c, app, "PUT", "/api/apps/v1/events/publish-urls/"+id+"/signing-secret", map[string]any{"secret": secret2}); code != 200 {
		t.Fatalf("the same secret again: %d %s", code, raw)
	}
	var again int
	c.DB.Pool.QueryRow(ctx, `SELECT count(*) FROM app_log WHERE app_id = 'chat' AND action = $1`, apps.LogPublishURLSecretRotated).Scan(&again)
	if rotations != 1 || again != 1 {
		t.Fatalf("rotations logged: %d then %d, want 1 and 1", rotations, again)
	}

	// Removed: unsigned posts work again, deduped as before.
	if code, out, raw := appDo(t, c, app, "DELETE", "/api/apps/v1/events/publish-urls/"+id+"/signing-secret", nil); code != 200 || out["removed"] != true {
		t.Fatalf("removing: %d %s", code, raw)
	}
	hdr := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "after"}
	if code, out := post(t, url, body, hdr); code != 202 || out["duplicate"] != false {
		t.Fatalf("unsigned after removing: %d %v", code, out)
	}

	// Another app can't touch it.
	other := eventsApp(t, c, "other")
	if code, out, _ := appDo(t, c, other, "PUT", "/api/apps/v1/events/publish-urls/"+id+"/signing-secret", map[string]any{"secret": secret}); code != 404 || errCode(out) != "unknown_publish_url" {
		t.Fatalf("another app: %d %v", code, out)
	}

	// The log records set, rotate and remove, with the fingerprint.
	rows, err := c.DB.Pool.Query(ctx, `SELECT action, detail::text FROM app_log WHERE app_id = 'chat' AND action LIKE 'app.publish_url_secret_%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for rows.Next() {
		var action, detail string
		rows.Scan(&action, &detail)
		actions = append(actions, action)
		if !strings.Contains(detail, id) || strings.Contains(detail, secret[6:]) || strings.Contains(detail, secret2[6:]) {
			t.Fatalf("log detail %s", detail)
		}
	}
	if got := strings.Join(actions, " "); got != "app.publish_url_secret_set app.publish_url_secret_rotated app.publish_url_secret_removed" {
		t.Fatalf("logged %q", got)
	}

	// Revoking a URL deletes its secret.
	code, made2, raw := appDo(t, c, app, "POST", "/api/apps/v1/events/publish-urls", map[string]any{"name": "chat.message",
		"arguments": map[string]any{"channel": "ops"}, "signing_secret": secret})
	responses = append(responses, raw)
	if code != 200 {
		t.Fatalf("another: %d %s", code, raw)
	}
	if err := app.RevokePublishURL(ctx, made2["id"].(string)); err != nil {
		t.Fatal(err)
	}
	var left bool
	if err := c.DB.Pool.QueryRow(ctx, `SELECT signing_secret IS NOT NULL OR previous_signing_secret IS NOT NULL FROM topic_publish_urls WHERE id = $1`, made2["id"]).Scan(&left); err != nil || left {
		t.Fatalf("a revoked URL's secret: kept %v %v", left, err)
	}
	if code, out, _ := appDo(t, c, app, "PUT", "/api/apps/v1/events/publish-urls/"+made2["id"].(string)+"/signing-secret", map[string]any{"secret": secret}); code != 410 || errCode(out) != "revoked" {
		t.Fatalf("setting a revoked URL's: %d %v", code, out)
	}

	// Sealed at rest: the stored bytes aren't the key.
	var stored []byte
	if err := c.DB.Pool.QueryRow(ctx, `SELECT signing_secret FROM topic_publish_urls WHERE signing_secret IS NOT NULL LIMIT 1`).Scan(&stored); err == nil && bytes.Contains(stored, key) {
		t.Fatal("a signing secret stored in the clear")
	}

	for _, s := range []string{secret, secret2} {
		raw := s[len("whsec_"):]
		if strings.Contains(logs.String(), raw) {
			t.Fatal("the log has a signing secret")
		}
		for _, r := range responses {
			if strings.Contains(r, raw) {
				t.Fatalf("a response has a signing secret: %s", r)
			}
		}
	}
}

// A publish URL without a secret takes unsigned POSTs, as before, and
// dedupes by webhook-id or svix-id too, when there's no Idempotency-Key.
func TestUnsignedPublishURLWebhookID(t *testing.T) {
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	var sub eventSubResult
	if code, _ := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": map[string]any{"user": "u1", "client": "claude-code"},
		"name": "chat.message", "arguments": json.RawMessage(`{"channel":"eng"}`)}, &sub); code != 200 {
		t.Fatalf("subscribe: %d", code)
	}
	code, made, raw := appDo(t, c, app, "POST", "/api/apps/v1/events/publish-urls", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng"}})
	if code != 200 || made["signing"] != nil {
		t.Fatalf("made: %d %s", code, raw)
	}
	url := made["url"].(string)
	body := `{"text": "x", "from": "ci"}`
	for i, tc := range []struct {
		hdr map[string]string
		dup bool
	}{
		{map[string]string{"webhook-id": "msg_a", "webhook-signature": "v1,anything"}, false},
		{map[string]string{"webhook-id": "msg_a"}, true},
		{map[string]string{"svix-id": "msg_b"}, false},
		{map[string]string{"svix-id": "msg_b"}, true},
		{nil, false},
		{nil, false},
	} {
		if code, out := post(t, url, body, tc.hdr); code != 202 || out["duplicate"] != tc.dup {
			t.Fatalf("post %d: %d %v", i, code, out)
		}
	}
}
