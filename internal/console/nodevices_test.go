package console_test

import (
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/coretest"
)

// An earlier desktop app and its device tokens are gone. A device token, even
// one still live in the devices table (approved by the previous revision
// while it served), signs nobody in to the console API, and the device
// routes are gone.
func TestDeviceTokensAreRejected(t *testing.T) {
	c := coretest.Start(t)
	ada := c.SignIn("ada@example.com")
	if code := ada.Do("POST", "/api/v1/agents", map[string]string{"id": "mac-agent"}, nil); code != 200 {
		t.Fatalf("agent: %d", code)
	}
	tenant := ada.Workspace(t)
	token := "wsd_" + tenant + "_c2VjcmV0LXRva2VuLWZvci10aGUtdGVzdA"
	hash := sha256.Sum256([]byte(token))
	ctx := context.Background()
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO devices (tenant_id, user_id, name, token_hash, epoch)
			SELECT current_tenant(), id, 'Mac', $2, session_epoch FROM users WHERE id = $1`, userID(t, ada), hash[:])
		return err
	}); err != nil {
		t.Fatal(err)
	}

	asDevice := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, c.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// What a device could do before: read the workspace and mint its
	// agents' stream links.
	for _, r := range [][2]string{
		{"GET", "/api/v1/agents"},
		{"GET", "/api/v1/subscriptions"},
		{"GET", "/api/v1/audit"},
	} {
		if code, out := asDevice(r[0], r[1], ``); code != 401 {
			t.Errorf("a device token: %s %s: %d %s", r[0], r[1], code, out)
		}
	}
	if code, out := asDevice("POST", "/api/v1/agents/mac-agent/stream-link", `{"ack": true}`); code != 401 && code != 403 {
		t.Errorf("a device token minted a stream link: %d %s", code, out)
	}
	if code, out := asDevice("POST", "/api/v1/agents/mac-agent/stream-link", `{"ack": true}`); strings.Contains(out, "wss://") || strings.Contains(out, "ws://") {
		t.Errorf("a device token got a stream link: %d %s", code, out)
	}

	// The device routes are gone, for the person too.
	for _, r := range [][2]string{
		{"GET", "/api/v1/devices"},
		{"POST", "/api/v1/devices"},
		{"POST", "/api/v1/devices/00000000-0000-0000-0000-000000000000/revoke"},
	} {
		if code := ada.Do(r[0], r[1], map[string]string{"name": "Mac", "callback": "http://127.0.0.1:9/cb", "state": "s"}, nil); code != 404 {
			t.Errorf("%s %s: %d, want 404", r[0], r[1], code)
		}
	}
}
