package coretest

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/harnesses"
)

var (
	// The key step's three commands, in order (install, init, prove), and
	// the register challenge's ID the last names.
	grokCmdRE         = regexp.MustCompile("[0-9]\\. `([^`]+)`")
	grokChallengeIDRE = regexp.MustCompile(`--challenge '([0-9a-f-]{36})'`)
	grokSetupRE       = regexp.MustCompile(`/webhook-setup#t=([A-Za-z0-9_.-]+)`)
)

// grokWebhookCanaries runs Grok Bot's webhook setup end to end for the
// leak checks: a Grok Bot subscribes, makes and proves its key with the
// exact commands (openssl and curl), the user pastes a webhook (on a
// stand-in for Cursor, agenthooks.AllowTestOrigin) on the setup page, the
// run it starts proves the setup challenge, and a delivery is POSTed
// there. It adds to canaries the webhook's key and ID, the setup and
// removal tokens, both challenges and the bot's ID, none of which an
// exported log record or span may carry, and a forged setup token and
// challenge it sends.
func grokWebhookCanaries(t *testing.T, c *Core, canaries map[string]string) {
	t.Helper()
	for _, tool := range []string{"curl", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Logf("no %s here: Grok Bot's webhook canaries skipped", tool)
			return
		}
	}
	const (
		bot     = "botcnry7-1c2b-4a3d-9e8f-0a1b2c3d4e5f"
		hookID  = "ghkCnry5Lq8Rt2V"
		hookKey = "crsrKeyCnry" + "0123456789abcdefghijklmnopqrstuvwxyzABCD"
	)
	home := t.TempDir()
	run := func(cmd string) string {
		x := exec.Command("sh", "-c", cmd)
		x.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "CURSOR_CONVERSATION_ID=" + bot}
		out, _ := x.CombinedOutput()
		return string(out)
	}
	// fetch is what the connector's prove fetches, for the canaries.
	fetch := func(id string) string {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		w.WriteField("bot", bot)
		if id != "" {
			w.WriteField("challenge", id)
		}
		w.Close()
		resp, err := http.Post(c.URL+"/v1/stream/proof/challenge", w.FormDataContentType(), &b)
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return strings.TrimSpace(string(raw))
	}
	proveCmd := harnesses.GrokProveCommand(c.URL, bot, "")
	var mu sync.Mutex
	var events, wg = 0, sync.WaitGroup{}
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body agenthooks.CursorBody
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		if r.URL.Path != agenthooks.CursorPath+hookID || r.Header.Get("Authorization") != "Bearer "+hookKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"success":true,"runUuid":"r"}`)
		switch body.Type {
		case agenthooks.TypeChallenge:
			ch := fetch("")
			mu.Lock()
			canaries[ch] = "a setup challenge"
			mu.Unlock()
			wg.Go(func() { run(proveCmd) })
		case agenthooks.TypeEvents:
			mu.Lock()
			events++
			mu.Unlock()
		}
	}))
	t.Cleanup(func() { wg.Wait(); fake.Close() })
	t.Cleanup(agenthooks.AllowTestOrigin(fake.URL))

	post := func(path string, body any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		resp, err := http.Post(c.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	// Forged, refused.
	const forgedToken, forgedCh = "grksetupfrg7c3", "wakeshift:v1:register:frgCnry" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + ":frgNonceCnryBBBBBBBBBB"
	if code, _ := post("/v1/stream/webhook-setup/describe", map[string]string{"token": forgedToken}); code != 401 {
		t.Fatalf("a forged setup token: %d", code)
	}
	canaries[forgedToken] = "a forged setup token"
	resp, err := http.Post(c.URL+"/v1/stream/proof", "application/x-www-form-urlencoded", strings.NewReader("challenge="+forgedCh))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	canaries["frgNonceCnry"] = "a forged challenge"

	app := c.NoteApp("grokci")
	sub, err := Subscribe(app, Caller{User: "grok-canary", Client: "Cursor", Harness: "grok-bot", Conversation: bot}, "grokci/acme/web", SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	cmds := grokCmdRE.FindAllStringSubmatch(sub.Addition.Text, -1)
	id := grokChallengeIDRE.FindStringSubmatch(sub.Addition.Text)
	if len(cmds) < 3 || id == nil {
		t.Fatalf("no key step: %s", sub.Addition.Text)
	}
	canaries[fetch(id[1])] = "a register challenge"
	canaries[bot] = "a Grok Bot's ID"
	run(cmds[0][1]) // install
	run(cmds[1][1]) // init
	m := grokSetupRE.FindStringSubmatch(run(cmds[2][1]))
	if m == nil {
		t.Fatal("no setup link")
	}
	token := m[1]
	canaries[token] = "a setup token"
	canaries[hookID] = "a Cursor webhook's ID"
	canaries[hookKey] = "a Cursor webhook's key"
	if code, out := post("/v1/stream/webhook-setup", map[string]string{"token": token, "url": fake.URL + agenthooks.CursorPath + hookID, "key": hookKey}); code != 200 {
		t.Fatalf("save: %d %v", code, out)
	}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the webhook wasn't bound")
		}
		if _, out := post("/v1/stream/webhook-setup/describe", map[string]string{"token": token}); out["state"] == "on" {
			break
		}
	}
	if _, err := EmitNote(app, []string{"grokci/acme/web"}, Note{Text: "grok deploy finished"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		mu.Lock()
		n := events
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing was delivered to the webhook")
		}
	}
}
