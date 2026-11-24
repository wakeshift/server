package coretest_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
)

// The setup and proof routes' limits, counted in Postgres: each at its
// boundary, across two cores on one database, and a refusal for too many
// failures answering exactly as an invalid token or challenge does.

// startLimited is startGrok with the default limits, behind a load
// balancer (two proxies), so a test chooses each request's address with
// X-Forwarded-For.
func startLimited(t *testing.T, set func(*core.Options)) *coretest.Core {
	t.Helper()
	return startGrok(t, func(o *core.Options) {
		o.GrokLimits = appapi.GrokLimits{}
		o.GrokRateLimits = true
		o.ProxyHops = 2
		if set != nil {
			set(o)
		}
	})
}

type limitedAnswer struct {
	code  int
	out   map[string]any
	raw   []byte
	retry int // Retry-After, in seconds; 0 if absent
}

// pageFrom calls a setup route from addr: the load balancer appends the
// client's address, then its own.
func pageFrom(t *testing.T, c *coretest.Core, addr, path string, body any) limitedAnswer {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", c.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "192.0.2.250, "+addr+", 10.0.0.1") // the first entry is the client's own claim
	return limitedDo(t, req)
}

// proveFrom posts a proof form from addr, as curl -F does.
func proveFrom(t *testing.T, c *coretest.Core, addr string, form map[string]string) limitedAnswer {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for k, v := range form {
		w.WriteField(k, v)
	}
	w.Close()
	req, _ := http.NewRequest("POST", c.URL+"/v1/stream/proof", &b)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Forwarded-For", addr+", 10.0.0.1")
	return limitedDo(t, req)
}

func limitedDo(t *testing.T, req *http.Request) limitedAnswer {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	a := limitedAnswer{code: resp.StatusCode}
	a.raw, _ = io.ReadAll(resp.Body)
	json.Unmarshal(a.raw, &a.out)
	a.retry, _ = strconv.Atoi(resp.Header.Get("Retry-After"))
	return a
}

// rateLimited: 429 with the code and a Retry-After within the window.
func rateLimited(t *testing.T, name string, a limitedAnswer, code string, window time.Duration) {
	t.Helper()
	if a.code != 429 || errCode(a.out) != code || a.retry < 1 || a.retry > int(window/time.Second) {
		t.Fatalf("%s: %d %s, Retry-After %d; want 429 %s", name, a.code, a.raw, a.retry, code)
	}
}

// limitedBotID is the i-th test bot's ID.
func limitedBotID(i int) string { return fmt.Sprintf("%08x-1111-4222-8333-%012x", i+1, i+1) }

// setupTokens subscribes n bots of user through app, each making its key,
// and returns their setup tokens.
func setupTokens(t *testing.T, c *coretest.Core, vm grokVM, user string, from, n int) ([]string, []*grokBot) {
	t.Helper()
	app := noteApp(t, c, "ci")
	var tokens []string
	var bots []*grokBot
	for i := from; i < from+n; i++ {
		b := &grokBot{c: c, vm: vm, id: limitedBotID(i)}
		sub, err := subscribe(app, b.call(user), fmt.Sprintf("ci/limits%d", i), subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, setupToken(t, b.keyStep(t, sub.Addition.Text)))
		bots = append(bots, b)
	}
	return tokens, bots
}

var wrongCursorKey = strings.Repeat("W", 40)

// Saves: 3 per link and 5 per address an hour, both answered 429 with
// Retry-After; another address isn't affected.
func TestGrokSaveLimits(t *testing.T) {
	c := startLimited(t, nil)
	f := newFakeCursor(t, c)
	tokens, bots := setupTokens(t, c, newGrokVM(t), "gina", 0, 3)
	var urls []string
	for i, b := range bots {
		u, _ := f.webhook(fmt.Sprintf("limitHook%d", i), b)
		urls = append(urls, u)
	}
	const a, other = "203.0.113.10", "203.0.113.11"
	save := func(addr string, i int) limitedAnswer {
		return pageFrom(t, c, addr, "/v1/stream/webhook-setup", map[string]string{"token": tokens[i], "url": urls[i], "key": wrongCursorKey})
	}
	// A wrong key: each save POSTs, fails and frees the link again.
	for range 3 {
		if r := save(a, 0); r.code != 400 || errCode(r.out) != "webhook_unauthorized" {
			t.Fatalf("save: %d %s", r.code, r.raw)
		}
	}
	posts := len(f.got())
	rateLimited(t, "a fourth save on one link", save(a, 0), "too_many_tries", time.Hour)
	// Two more from the address, on another link: five in all.
	for range 2 {
		if r := save(a, 1); r.code != 400 {
			t.Fatalf("save: %d %s", r.code, r.raw)
		}
	}
	rateLimited(t, "a sixth save from one address", save(a, 2), "rate_limited", time.Hour)
	if got := len(f.got()); got != posts+2 {
		t.Fatalf("%d test POSTs after the refusals, want %d", got, posts+2)
	}
	// Another address, on a fresh link, with the right key: sent.
	_, key := f.webhook("limitHook2", bots[2])
	if r := pageFrom(t, c, other, "/v1/stream/webhook-setup", map[string]string{"token": tokens[2], "url": urls[2], "key": key}); r.code != 200 || r.out["awaiting_proof"] != true {
		t.Fatalf("another address: %d %s", r.code, r.raw)
	}
}

// Describe: 120 per address per 10 minutes, the page's polling for a whole
// setup challenge's 10 minutes (every 6 seconds: 100) within it; polling
// up to the limit leaves the address's saves and failures untouched.
func TestGrokDescribeLimit(t *testing.T) {
	c := startLimited(t, nil)
	f := newFakeCursor(t, c)
	tokens, bots := setupTokens(t, c, newGrokVM(t), "gina", 0, 1)
	const a = "203.0.113.20"
	for i := range 120 {
		if r := pageFrom(t, c, a, "/v1/stream/webhook-setup/describe", map[string]string{"token": tokens[0]}); r.code != 200 {
			t.Fatalf("describe %d: %d %s", i+1, r.code, r.raw)
		}
	}
	url, key := f.webhook("describeHook", bots[0])
	if r := pageFrom(t, c, a, "/v1/stream/webhook-setup", map[string]string{"token": tokens[0], "url": url, "key": key}); r.code != 200 {
		t.Fatalf("a save after polling: %d %s", r.code, r.raw)
	}
	rateLimited(t, "the 121st describe", pageFrom(t, c, a, "/v1/stream/webhook-setup/describe", map[string]string{"token": tokens[0]}), "rate_limited", 10*time.Minute)
	// The setup still completes: the bot's proof isn't limited.
	waitFor(t, "the bot to confirm", func() bool {
		r := pageFrom(t, c, "203.0.113.21", "/v1/stream/webhook-setup/describe", map[string]string{"token": tokens[0]})
		return r.out["state"] == "on"
	})
}

// Failed token and challenge checks: 20 per address per 10 minutes, on
// describe, save and proof together. Past them the address's valid tokens
// and challenges get, byte for byte, what an invalid one gets; never a 429.
func TestGrokFailureLimit(t *testing.T) {
	c := startLimited(t, nil)
	f := newFakeCursor(t, c)
	tokens, bots := setupTokens(t, c, newGrokVM(t), "gina", 0, 1)
	url, key := f.webhook("failHook", bots[0])
	app := noteApp(t, c, "ci")
	sub, err := subscribe(app, bots[0].call("hank"), "ci/failures", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	validCh := registerCh(t, c, sub.Addition.Text)
	const a = "203.0.113.30"

	badDescribe := pageFrom(t, c, a, "/v1/stream/webhook-setup/describe", map[string]string{"token": "bad"})
	badSave := pageFrom(t, c, a, "/v1/stream/webhook-setup", map[string]string{"token": "bad", "url": url, "key": key})
	badProof := proveFrom(t, c, a, map[string]string{"challenge": "$(id)"})
	if badDescribe.code != 401 || errCode(badDescribe.out) != "invalid_link" || !bytes.Equal(badDescribe.raw, badSave.raw) {
		t.Fatalf("bad tokens: %d %s / %d %s", badDescribe.code, badDescribe.raw, badSave.code, badSave.raw)
	}
	if badProof.code != 401 || strings.TrimSpace(string(badProof.raw)) != harnesses.GrokChallengeGone {
		t.Fatalf("bad challenge: %d %s", badProof.code, badProof.raw)
	}
	// Bad proofs from the address don't count: bots' VMs share addresses.
	for i := range 25 {
		if r := proveFrom(t, c, a, map[string]string{"challenge": "wakeshift:v1:register:" + strings.Repeat("A", 43) + ":" + strings.Repeat("B", 22)}); r.code != 401 {
			t.Fatalf("bad proof %d: %d %s", i+1, r.code, r.raw)
		}
	}
	if r := pageFrom(t, c, a, "/v1/stream/webhook-setup/describe", map[string]string{"token": tokens[0]}); r.code != 200 {
		t.Fatalf("a valid describe after bad proofs only: %d %s", r.code, r.raw)
	}
	for i := 2; i < 20; i++ { // 20 in all, across the page's two routes
		var r limitedAnswer
		switch i % 2 {
		case 0:
			r = pageFrom(t, c, a, "/v1/stream/webhook-setup/describe", map[string]string{"token": "bad" + strconv.Itoa(i)})
		default:
			r = pageFrom(t, c, a, "/v1/stream/webhook-setup", map[string]string{"token": "bad" + strconv.Itoa(i), "url": url, "key": key})
		}
		if r.code != 401 {
			t.Fatalf("failure %d: %d %s", i+1, r.code, r.raw)
		}
	}
	// The 21st, with a valid token or challenge: refused as invalid.
	if r := pageFrom(t, c, a, "/v1/stream/webhook-setup/describe", map[string]string{"token": tokens[0]}); r.code != 401 || !bytes.Equal(r.raw, badDescribe.raw) || r.retry != 0 {
		t.Fatalf("valid describe past the failures: %d %s", r.code, r.raw)
	}
	if r := pageFrom(t, c, a, "/v1/stream/webhook-setup", map[string]string{"token": tokens[0], "url": url, "key": key}); r.code != 401 || !bytes.Equal(r.raw, badSave.raw) || r.retry != 0 {
		t.Fatalf("valid save past the failures: %d %s", r.code, r.raw)
	}
	if len(f.got()) != 0 {
		t.Fatal("a refused save reached the webhook")
	}
	// Another address: the same token works.
	if r := pageFrom(t, c, "203.0.113.31", "/v1/stream/webhook-setup/describe", map[string]string{"token": tokens[0]}); r.code != 200 {
		t.Fatalf("another address: %d %s", r.code, r.raw)
	}
	// Proofs have no limit per address: a valid one from the blocked
	// address still works (a bot behind the same outbound address isn't
	// locked out by someone else's failures).
	if r := proveFrom(t, c, a, newTestKey().form(bots[0].id, validCh)); r.code != 200 {
		t.Fatalf("a valid proof from the blocked address: %d %s", r.code, r.raw)
	}
}

// Proofs from one address for many challenges (bots behind one outbound
// address) aren't limited: more than every per-address limit, all
// answered.
func TestGrokProofsFromOneAddress(t *testing.T) {
	c := startLimited(t, nil)
	app := noteApp(t, c, "ci")
	const a = "203.0.113.40"
	for i := range 30 {
		bot := limitedBotID(100 + i)
		call := caller{User: fmt.Sprintf("p%d", i), Client: "Cursor", Harness: "grok-bot", Conversation: bot}
		sub, err := subscribe(app, call, fmt.Sprintf("ci/p%d", i), subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		ch := registerCh(t, c, sub.Addition.Text)
		if r := proveFrom(t, c, a, newTestKey().form(bot, ch)); r.code != 200 {
			t.Fatalf("proof %d: %d %s", i+1, r.code, r.raw)
		}
	}
}

// Webhooks bound per user: 10 a day. Two saves that both pass the save's
// check bind only one between them; the next save is refused with 429
// before anything is sent.
func TestGrokBindsPerUser(t *testing.T) {
	c := startLimited(t, nil)
	f := newFakeCursor(t, c)
	vm := newGrokVM(t)
	tokens, bots := setupTokens(t, c, vm, "gina", 0, 12)
	saveFrom := func(i int) limitedAnswer {
		t.Helper()
		url, key := f.webhook(fmt.Sprintf("bindHook%d", i), bots[i])
		addr := fmt.Sprintf("203.0.113.%d", 50+i) // apart from the saves' own limit
		return pageFrom(t, c, addr, "/v1/stream/webhook-setup", map[string]string{"token": tokens[i], "url": url, "key": key})
	}
	for i := range 9 {
		if r := saveFrom(i); r.code != 200 {
			t.Fatalf("save %d: %d %s", i+1, r.code, r.raw)
		}
		waitOn(t, c, tokens[i])
	}
	// The 10th and 11th, saved before either binds.
	for _, i := range []int{9, 10} {
		if r := saveFrom(i); r.code != 200 {
			t.Fatalf("save %d: %d %s", i+1, r.code, r.raw)
		}
	}
	states := map[string]int{}
	waitFor(t, "both proofs", func() bool {
		states = map[string]int{}
		for _, i := range []int{9, 10} {
			_, out := describe(t, c, tokens[i])
			switch {
			case out["state"] == "on":
				states["on"]++
			case out["state"] == "open" && out["error"] == "too_many_webhooks":
				states["refused"]++
			}
		}
		return states["on"]+states["refused"] == 2
	})
	if states["on"] != 1 {
		t.Fatalf("%d of the two racing setups bound, want 1", states["on"])
	}
	posts := len(f.got())
	rateLimited(t, "a save past 10 webhooks", saveFrom(11), "too_many_webhooks", 24*time.Hour)
	if len(f.got()) != posts {
		t.Fatal("the refused save reached the webhook")
	}
	// Another user isn't affected.
	other, otherBots := setupTokens(t, c, vm, "hank", 20, 1)
	url, key := f.webhook("bindHookHank", otherBots[0])
	if r := pageFrom(t, c, "203.0.113.71", "/v1/stream/webhook-setup", map[string]string{"token": other[0], "url": url, "key": key}); r.code != 200 {
		t.Fatalf("another user: %d %s", r.code, r.raw)
	}
}

// Failed proofs across Wakeshift: the 30th in an hour logs one alert,
// naming at most the proof's app; nothing is refused for it.
func TestGrokFailedProofsAlert(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	c := startLimited(t, nil)
	app := noteApp(t, c, "ci")
	bot := limitedBotID(0)
	sub, err := subscribe(app, caller{User: "gina", Client: "Cursor", Harness: "grok-bot", Conversation: bot}, "ci/alert", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	ch := registerCh(t, c, sub.Addition.Text)
	alerts := func() int { return strings.Count(logs.String(), "failed proofs over the alert threshold") }
	// Malformed and unknown challenges, which anyone can send, don't count.
	for i := range 40 {
		form := map[string]string{"challenge": "$(id)"}
		if i%2 == 1 {
			form["challenge"] = "wakeshift:v1:register:" + strings.Repeat("A", 43) + ":" + strings.Repeat("B", 22)
		}
		if r := proveFrom(t, c, fmt.Sprintf("198.51.100.%d", i+1), form); r.code != 401 {
			t.Fatalf("proof %d: %d %s", i+1, r.code, r.raw)
		}
	}
	if alerts() != 0 {
		t.Fatal("an alert from proofs anyone can send")
	}
	// Bad signatures (over another bot's ID) on real challenges count: 29
	// (each challenge ends after 3), then the 30th alerts, naming its app.
	k := newTestKey()
	badOn := func(ch string) map[string]string {
		form := k.form(bot, ch)
		form["signature"] = k.sign(limitedBotID(9), ch)
		return form
	}
	chs := []string{ch}
	for i := range 9 {
		b := limitedBotID(20 + i)
		s, err := subscribe(app, caller{User: "gina", Client: "Cursor", Harness: "grok-bot", Conversation: b}, fmt.Sprintf("ci/alert%d", i), subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		chs = append(chs, registerCh(t, c, s.Addition.Text))
	}
	n := 0
	for _, ch := range chs {
		for range 3 {
			if n++; n == 30 {
				break
			}
			if r := proveFrom(t, c, "198.51.100.200", badOn(ch)); r.code != 401 {
				t.Fatalf("bad signature %d: %d %s", n, r.code, r.raw)
			}
		}
	}
	if alerts() != 0 {
		t.Fatal("an alert before the 30th")
	}
	form := badOn(chs[9])
	if r := proveFrom(t, c, "198.51.100.100", form); r.code != 401 {
		t.Fatalf("bad signature: %d %s", r.code, r.raw)
	}
	if alerts() != 1 {
		t.Fatalf("%d alerts at 30 failed proofs, want 1:\n%s", alerts(), logs.String())
	}
	line := ""
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "failed proofs over the alert threshold") {
			line = l
		}
	}
	var entry map[string]any
	json.Unmarshal([]byte(line), &entry)
	if entry["level"] != "WARN" || entry["app"] == "" || entry["app"] == nil {
		t.Fatalf("alert line: %s", line)
	}
	for k := range entry {
		switch k {
		case "time", "level", "msg", "count", "window", "app":
		default:
			t.Fatalf("the alert carries %q: %s", k, line)
		}
	}
	if strings.Contains(line, "wakeshift:v1:") || strings.Contains(line, bot) || strings.Contains(line, "gina") || strings.Contains(line, "hank") {
		t.Fatalf("the alert carries a challenge, a bot or a person: %s", line)
	}
	// More failures: no second alert, and still no refusal for them.
	for i := range 5 {
		b := limitedBotID(40 + i)
		s, err := subscribe(app, caller{User: "gina", Client: "Cursor", Harness: "grok-bot", Conversation: b}, fmt.Sprintf("ci/more%d", i), subOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if r := proveFrom(t, c, fmt.Sprintf("198.51.100.%d", 150+i), badOn(registerCh(t, c, s.Addition.Text))); r.code != 401 || r.retry != 0 {
			t.Fatalf("proof past the threshold: %d %s", r.code, r.raw)
		}
	}
	if alerts() != 1 {
		t.Fatalf("%d alerts, want 1", alerts())
	}
}

// Two cores on one database (two Cloud Run instances) share the counts.
func TestGrokLimitsAcrossInstances(t *testing.T) {
	c1 := startLimited(t, nil)
	c2 := startLimited(t, func(o *core.Options) { o.DB = c1.DB })
	f := newFakeCursor(t, c1)
	tokens, bots := setupTokens(t, c1, newGrokVM(t), "gina", 0, 1)
	url, _ := f.webhook("crossHook", bots[0])
	cores := []*coretest.Core{c1, c2}
	const a = "203.0.113.80"

	// Saves on one link: two through one core, one through the other, and
	// the fourth is refused by either.
	for i := range 3 {
		if r := pageFrom(t, cores[i%2], a, "/v1/stream/webhook-setup", map[string]string{"token": tokens[0], "url": url, "key": wrongCursorKey}); r.code != 400 {
			t.Fatalf("save %d: %d %s", i+1, r.code, r.raw)
		}
	}
	rateLimited(t, "a fourth save, through the other core", pageFrom(t, c2, a, "/v1/stream/webhook-setup", map[string]string{"token": tokens[0], "url": url, "key": wrongCursorKey}), "too_many_tries", time.Hour)

	// Failures: ten through each core, then a valid token is refused by
	// both.
	const b = "203.0.113.81"
	for i := range 20 {
		if r := pageFrom(t, cores[i%2], b, "/v1/stream/webhook-setup/describe", map[string]string{"token": "bad"}); r.code != 401 {
			t.Fatalf("failure %d: %d", i+1, r.code)
		}
	}
	for _, c := range cores {
		if r := pageFrom(t, c, b, "/v1/stream/webhook-setup/describe", map[string]string{"token": tokens[0]}); r.code != 401 || errCode(r.out) != "invalid_link" {
			t.Fatalf("a valid token past the failures: %d %s", r.code, r.raw)
		}
	}
}

// A proof body that isn't the form the command sends, like a bad
// challenge, doesn't count toward the address's failed checks: proofs have
// no limit per address, so someone sharing a bot's outbound address can't
// lock its proofs out.
func TestGrokMalformedProofsDontCount(t *testing.T) {
	c := startLimited(t, nil)
	const a = "203.0.113.95"
	malformed := func() limitedAnswer {
		req, _ := http.NewRequest("POST", c.URL+"/v1/stream/proof", strings.NewReader("--x\r\nnot a part"))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
		req.Header.Set("X-Forwarded-For", a+", 10.0.0.1")
		return limitedDo(t, req)
	}
	for i := range 25 {
		if r := malformed(); r.code != 400 {
			t.Fatalf("malformed %d: %d %s", i+1, r.code, r.raw)
		}
		if r := proveFrom(t, c, a, map[string]string{"challenge": "wakeshift:v1:register:" + strings.Repeat("A", 43) + ":" + strings.Repeat("B", 22)}); r.code != 401 ||
			strings.TrimSpace(string(r.raw)) != harnesses.GrokChallengeGone {
			t.Fatalf("unknown challenge %d: %d %s", i+1, r.code, r.raw)
		}
	}
	if n := countPlatform(t, c, `SELECT count(*) FROM rate_counts`); n != 0 {
		t.Fatalf("%d counts stored for proofs", n)
	}
	app := noteApp(t, c, "ci")
	bot := limitedBotID(70)
	sub, err := subscribe(app, caller{User: "gina", Client: "Cursor", Harness: "grok-bot", Conversation: bot}, "ci/malformed", subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if r := proveFrom(t, c, a, newTestKey().form(bot, registerCh(t, c, sub.Addition.Text))); r.code != 200 {
		t.Fatalf("a valid proof from the address: %d %s", r.code, r.raw)
	}
}

// With WAKESHIFT_GROK_RATE_LIMITS off (the default), none of the request
// limits refuses anything and no client address is read or stored, while
// the tokens' own limits still hold: 5 failed attempts end a link, with
// the same 401 as any bad token.
func TestGrokRateLimitsOff(t *testing.T) {
	c := startGrok(t, func(o *core.Options) { o.GrokLimits = appapi.GrokLimits{}; o.ProxyHops = 2 })
	f := newFakeCursor(t, c)
	tokens, bots := setupTokens(t, c, newGrokVM(t), "gina", 0, 3)
	const a = "203.0.113.120"
	// Past every per-address and per-link number: 8 saves from one
	// address, 4 on each of two links (each a failed attempt, under 5).
	for i := range 2 {
		url, _ := f.webhook(fmt.Sprintf("offHook%d", i), bots[i])
		for j := range 4 {
			if r := pageFrom(t, c, a, "/v1/stream/webhook-setup", map[string]string{"token": tokens[i], "url": url, "key": wrongCursorKey}); r.code != 400 {
				t.Fatalf("save %d on link %d: %d %s", j+1, i, r.code, r.raw)
			}
		}
	}
	// The fifth failure ends the link: the token's own limit.
	url0, _ := f.webhook("offHook0", bots[0])
	bad := pageFrom(t, c, a, "/v1/stream/webhook-setup/describe", map[string]string{"token": "bad"})
	if r := pageFrom(t, c, a, "/v1/stream/webhook-setup", map[string]string{"token": tokens[0], "url": url0, "key": wrongCursorKey}); r.code != 401 || !bytes.Equal(r.raw, bad.raw) {
		t.Fatalf("the fifth failed attempt: %d %s", r.code, r.raw)
	}
	// 130 describes and 25 bad tokens from the address, then a valid
	// token works.
	for i := range 130 {
		if r := pageFrom(t, c, a, "/v1/stream/webhook-setup/describe", map[string]string{"token": tokens[2]}); r.code != 200 {
			t.Fatalf("describe %d: %d", i+1, r.code)
		}
	}
	for i := range 25 {
		pageFrom(t, c, a, "/v1/stream/webhook-setup/describe", map[string]string{"token": "bad" + strconv.Itoa(i)})
	}
	url2, key2 := f.webhook("offHook2", bots[2])
	if r := pageFrom(t, c, a, "/v1/stream/webhook-setup", map[string]string{"token": tokens[2], "url": url2, "key": key2}); r.code != 200 {
		t.Fatalf("a valid save after many failures: %d %s", r.code, r.raw)
	}
	waitOn(t, c, tokens[2])
	if n := countPlatform(t, c, `SELECT count(*) FROM rate_counts`); n != 0 {
		t.Fatalf("%d request counts stored with the limits off", n)
	}
}
