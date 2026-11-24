package coretest_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/botkeys"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/dblimit"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Stand-ins for Grok Bot and Cursor's Automations webhooks: a user's VM
// (one HOME shared by the user's bots, as Grok's are), the agent following
// Wakeshift's texts with the exact commands they give (the install command,
// then the real connector binary it installs from the core), and a webhook
// endpoint whose runs, like a routine's, run the routine's command.

// grokVM is one user's VM: every bot of theirs runs in it, as one OS user.
type grokVM struct{ home string }

func newGrokVM(t *testing.T) grokVM {
	t.Helper()
	for _, tool := range []string{"curl", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s here", tool)
		}
	}
	return grokVM{home: t.TempDir()}
}

// run runs a command in the VM as bot, and returns what it printed.
func (vm grokVM) run(t *testing.T, bot, cmd string) string {
	t.Helper()
	c := exec.Command("sh", "-c", cmd)
	c.Env = []string{"HOME=" + vm.home, "PATH=" + os.Getenv("PATH"), "CURSOR_CONVERSATION_ID=" + bot}
	out, _ := c.CombinedOutput() // the connector's exit codes say what happened; its output says it too
	return string(out)
}

func (vm grokVM) dir(bot string) string { return filepath.Join(vm.home, ".local/share/wakeshift", bot) }

// bin is the connector's path in the VM, once installed.
func (vm grokVM) bin() string { return filepath.Join(vm.home, ".wakeshift", "bin", "wakeshift") }

// installDirect puts the test's connector build in the VM, as the install
// command would (for a bot whose own text never gave it one).
func (vm grokVM) installDirect(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(coretest.ConnectorDir(t), "wakeshift-"+runtime.GOOS+"-"+runtime.GOARCH))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(vm.bin()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vm.bin(), b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// reset is a VM reset: the connector, its settings and every key go.
func (vm grokVM) reset(t *testing.T) {
	t.Helper()
	for _, p := range []string{filepath.Join(vm.home, ".wakeshift"), filepath.Join(vm.home, ".local")} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
}

// grokBot is a Grok Bot: its ID, its VM, and whether its memory has
// "Wakeshift key".
type grokBot struct {
	c      *coretest.Core
	vm     grokVM
	id     string
	memory bool
	// routine is the instruction this bot's routine was made with (the «»
	// text Wakeshift gave it), which its runs follow.
	routine string
	// shown is the line its last key step's prove printed before
	// Wakeshift's answer: which app, in which workspace, it proved to.
	shown string
}

// splitShown separates prove's first line saying where it proves
// (connector.grokShown) from Wakeshift's answer.
func splitShown(out string) (shown, answer string) {
	if first, rest, ok := strings.Cut(out, "\n"); ok && strings.HasPrefix(first, "Wakeshift: proving this bot's key") {
		return first, rest
	}
	return "", out
}

var routineTextRE = regexp.MustCompile(`«([^»]+)»`)

// learnRoutine keeps the routine's instruction from a text that gives one.
func (b *grokBot) learnRoutine(text string) {
	if m := routineTextRE.FindStringSubmatch(text); m != nil {
		b.routine = m[1]
	}
}

func (b *grokBot) call(user string) caller {
	return caller{User: user, Client: "Cursor", UserAgent: "Cursor/1.0.0", Harness: "grok-bot", Conversation: b.id}
}

var (
	setupLinkRE = regexp.MustCompile(`https?://\S+/webhook-setup#t=([A-Za-z0-9_.-]+)`)
	// The commands a text gives, as backquoted code.
	installCmdRE = regexp.MustCompile("`(os=\\$\\(uname -s [^`]+)`")
	initCmdRE    = regexp.MustCompile("`(\\$HOME/\\.wakeshift/bin/wakeshift grok init [^`]+)`")
	proveCmdRE   = regexp.MustCompile("`(\\$HOME/\\.wakeshift/bin/wakeshift grok prove [^`]+)`")
	proveArgsRE  = regexp.MustCompile(`grok prove --bot '([^']+)' --issuer '[^']+' --challenge '([0-9a-f-]{36})'`)
)

// registerCh is the register challenge a text's prove command names,
// fetched as the connector fetches it: for the tests that sign it
// themselves.
func registerCh(t *testing.T, c *coretest.Core, text string) string {
	t.Helper()
	m := proveArgsRE.FindStringSubmatch(text)
	if m == nil || m[2] == "" {
		t.Fatalf("no register challenge in: %s", text)
	}
	code, body := fetchChallenge(t, c, m[1], m[2])
	if code != 200 {
		t.Fatalf("fetching the register challenge: %d %s", code, body)
	}
	return strings.TrimSpace(body)
}

// challengeIn is the workspace's latest challenge of purpose, used or
// not, made again as the core makes it: for the tests that look for it in
// logs or send it where it doesn't belong.
func challengeIn(t *testing.T, c *coretest.Core, tenant, purpose string) string {
	t.Helper()
	var id string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id::text FROM bot_challenges WHERE purpose = $1 ORDER BY created_at DESC LIMIT 1`, purpose).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	return botkeys.NewCodec(coretest.Key("bot-proof")).Challenge(purpose, tenant, id)
}

// firstChallengeIn is the workspace's earliest challenge of purpose.
func firstChallengeIn(t *testing.T, c *coretest.Core, tenant, purpose string) string {
	t.Helper()
	var id string
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id::text FROM bot_challenges WHERE purpose = $1 ORDER BY created_at LIMIT 1`, purpose).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	return botkeys.NewCodec(coretest.Key("bot-proof")).Challenge(purpose, tenant, id)
}

// fetchChallenge calls POST /v1/stream/proof/challenge as the connector
// does.
func fetchChallenge(t *testing.T, c *coretest.Core, bot, id string) (int, string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	w.WriteField("bot", bot)
	if id != "" {
		w.WriteField("challenge", id)
	}
	w.Close()
	resp, err := http.Post(c.URL+"/v1/stream/proof/challenge", w.FormDataContentType(), &b)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// keyStep follows GrokKeyNote (or GrokKeyMissingNote): the install
// command, the connector's init, then its prove, each exactly as the text
// gives it. It returns what prove printed (Wakeshift's answer).
func (b *grokBot) keyStep(t *testing.T, text string) (answer string) {
	t.Helper()
	b.learnRoutine(text)
	install, init, prove := installCmdRE.FindStringSubmatch(text), initCmdRE.FindStringSubmatch(text), proveCmdRE.FindStringSubmatch(text)
	if install == nil || init == nil || prove == nil {
		t.Fatalf("the key step lacks a command: %s", text)
	}
	if out := b.vm.run(t, b.id, install[1]); !strings.Contains(out, "Wakeshift's connector is installed.") {
		t.Fatalf("install: %s", out)
	}
	if out := b.vm.run(t, b.id, init[1]); !strings.HasPrefix(out, "Wakeshift key ready for this bot") {
		t.Fatalf("init: %s", out)
	}
	b.memory = true
	b.shown, answer = splitShown(b.vm.run(t, b.id, prove[1]))
	b.learnRoutine(answer)
	return answer
}

// loseKey deletes the bot's key file.
func (b *grokBot) loseKey(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(b.vm.dir(b.id), "signing.pem")); err != nil {
		t.Fatal(err)
	}
}

// setupToken is the token in a text's setup link.
func setupToken(t *testing.T, text string) string {
	t.Helper()
	m := setupLinkRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no setup link: %s", text)
	}
	return m[1]
}

// page calls one of the setup page's routes, as the page does.
func page(t *testing.T, c *coretest.Core, path string, body any) (int, map[string]any, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(c.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out, raw
}

func describe(t *testing.T, c *coretest.Core, token string) (int, map[string]any) {
	t.Helper()
	code, out, _ := page(t, c, "/v1/stream/webhook-setup/describe", map[string]string{"token": token})
	return code, out
}

func save(t *testing.T, c *coretest.Core, token, url, key string) (int, map[string]any) {
	t.Helper()
	code, out, _ := page(t, c, "/v1/stream/webhook-setup", map[string]string{"token": token, "url": url, "key": key})
	return code, out
}

// waitOn waits for the page's describe to say wake-ups are on.
func waitOn(t *testing.T, c *coretest.Core, token string) {
	t.Helper()
	waitFor(t, "the bot to confirm the webhook", func() bool {
		_, out := describe(t, c, token)
		return out["state"] == "on"
	})
}

// fakeCursor stands in for Cursor's Automations webhooks on this machine
// (agenthooks.AllowTestOrigin). Each webhook has a key and lands its runs
// in one bot; a run started by a challenge does what the routine's
// instruction says: checks the bot and the challenge, writes it to the
// bot's folder with the file-writing tool, and runs the proof command.
type fakeCursor struct {
	t    *testing.T
	srv  *httptest.Server
	core *coretest.Core
	mu   sync.Mutex
	// stopped: the test is over; challenges start no more runs.
	stopped bool
	// hooks: webhook ID → its key and the bot its runs land in.
	hooks map[string]fakeHook
	reqs  []cursorReq
	plan  []int // statuses to answer in turn (302: a redirect); then 200 success
	runs  []string
	// wakes: what each events POST's run did ("woken: n events", or
	// "another bot" when the routine's check refused it).
	wakes []string
	// runCount picks each run's shell.
	runCount int
	// wakeConv: the conversation ID each acting wake's run passes to the
	// app's tools.
	wakeConv []string
	wg       sync.WaitGroup
	// runMu: one run at a time writes the bot's challenge file and proves
	// it, as a bot handles one run at a time (two at once would race on
	// the one file).
	runMu sync.Mutex
}

type fakeHook struct {
	key string
	bot *grokBot
	// silent: its runs never answer; reckless: its runs skip the routine's
	// bot check (someone else's routine, answering whatever comes).
	silent, reckless bool
}

// grokNoSuccess is a plan entry: 200 without "success": true.
const grokNoSuccess = 2000

type cursorReq struct {
	ID, Auth, Idempotency string
	Body                  agenthooks.CursorBody
}

func newFakeCursor(t *testing.T, c *coretest.Core) *fakeCursor {
	f := &fakeCursor{t: t, core: c, hooks: map[string]fakeHook{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutPrefix(r.URL.Path, agenthooks.CursorPath)
		raw, _ := io.ReadAll(r.Body)
		var body agenthooks.CursorBody
		json.Unmarshal(raw, &body)
		f.mu.Lock()
		h, known := f.hooks[id]
		f.reqs = append(f.reqs, cursorReq{ID: id, Auth: r.Header.Get("Authorization"), Idempotency: r.Header.Get("Idempotency-Key"), Body: body})
		status := http.StatusOK
		if len(f.plan) > 0 {
			status, f.plan = f.plan[0], f.plan[1:]
		}
		f.mu.Unlock()
		switch {
		case !ok || !known:
			w.WriteHeader(http.StatusNotFound)
			return
		case r.Header.Get("Authorization") != "Bearer "+h.key:
			w.WriteHeader(http.StatusUnauthorized)
			return
		case status == http.StatusFound:
			http.Redirect(w, r, "https://example.com/", http.StatusFound)
			return
		case status == grokNoSuccess:
			io.WriteString(w, `{"success":false}`)
			return
		case status != http.StatusOK:
			w.WriteHeader(status)
			return
		}
		io.WriteString(w, `{"success":true,"runUuid":"run-1"}`)
		if body.Type == agenthooks.TypeEvents && !h.silent {
			f.wake(h, body)
		}
		if body.Type == agenthooks.TypeChallenge && !h.silent {
			f.mu.Lock()
			if !f.stopped {
				f.wg.Add(1)
				go func() { defer f.wg.Done(); f.routine(t, h, body) }()
			}
			f.mu.Unlock()
		}
	}))
	t.Cleanup(func() { f.stop(); f.srv.Close() })
	t.Cleanup(agenthooks.AllowTestOrigin(f.srv.URL))
	return f
}

// stop starts no more runs and waits for the running ones. Runs write in
// their bot's VM, so webhook has it run before the VM's temp dir is
// removed: that dir is made after the fake, and cleanups run last first.
func (f *fakeCursor) stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	f.wg.Wait()
}

// A routine's run follows its instruction as a strict model does, in a
// run whose CURSOR_CONVERSATION_ID is the run's own ID, not the bot's (as
// Grok Bot's are, as observed).
var (
	routineBotRE  = regexp.MustCompile("if \"bot\" isn't exactly `([^`]+)`")
	routineRunRE  = regexp.MustCompile("\"wakeshift.challenge\": run exactly `([^`]+)`")
	routineConvRE = regexp.MustCompile("pass `([^`]+)` \\(this bot's ID\\)")
)

// runEnv is a routine run's environment in bot's VM.
func runEnv(b *grokBot) []string {
	return []string{"HOME=" + b.vm.home, "PATH=" + os.Getenv("PATH"), "CURSOR_CONVERSATION_ID=run-" + randomHex(8)}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// routineFor is the instruction b's routine runs: the one it was made with,
// else what Wakeshift gives today.
func (f *fakeCursor) routineFor(b *grokBot) string {
	if b.routine != "" {
		return b.routine
	}
	return harnesses.GrokRoutineInstruction(f.core.URL, b.id)
}

// forThisBot is the routine's first check, as its instruction words it,
// in a run whose CURSOR_CONVERSATION_ID is env's.
func forThisBot(instr, bot string, env []string) bool {
	if m := routineBotRE.FindStringSubmatch(instr); m != nil {
		return bot == m[1]
	}
	if strings.Contains(instr, "printenv CURSOR_CONVERSATION_ID") {
		for _, e := range env {
			if v, ok := strings.CutPrefix(e, "CURSOR_CONVERSATION_ID="); ok {
				return bot == v
			}
		}
	}
	return false
}

// wake is a run an events POST started: it acts only if the routine's
// check passes. When it goes on to use the app's tools, it passes as its
// conversation ID what the routine says to, else (as the tools' own
// description has it) its CURSOR_CONVERSATION_ID: wakeConv.
func (f *fakeCursor) wake(h fakeHook, body agenthooks.CursorBody) {
	out := "another bot"
	env := runEnv(h.bot)
	instr := f.routineFor(h.bot)
	if forThisBot(instr, body.Bot, env) {
		out = fmt.Sprintf("woken: %d events", len(body.Events))
		conv := ""
		if m := routineConvRE.FindStringSubmatch(instr); m != nil {
			conv = m[1]
		} else {
			for _, e := range env {
				if v, ok := strings.CutPrefix(e, "CURSOR_CONVERSATION_ID="); ok {
					conv = v
				}
			}
		}
		f.mu.Lock()
		f.wakeConv = append(f.wakeConv, conv)
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.wakes = append(f.wakes, out)
	f.mu.Unlock()
}

// routine is a run the challenge started, in the webhook's bot: the
// routine's check, the challenge written where its instruction says, and
// its proof command run exactly. A reckless routine (someone else's)
// answers whatever comes, as the bot the challenge names.
func (f *fakeCursor) routine(t *testing.T, h fakeHook, body agenthooks.CursorBody) {
	f.runMu.Lock()
	defer f.runMu.Unlock()
	b := h.bot
	env := runEnv(b)
	f.mu.Lock()
	f.runCount++
	shell := []string{"sh", "bash"}[f.runCount%2] // the routine's command under both shells
	f.mu.Unlock()
	if _, err := exec.LookPath(shell); err != nil {
		shell = "sh"
	}
	out := "another bot"
	switch {
	case h.reckless:
		// Someone else's routine, answering whatever comes, as the bot the
		// body names: its own key for that ID, which isn't the bot's.
		run := func(cmd string) string {
			c := exec.Command(shell, "-c", cmd)
			c.Env = env
			o, _ := c.CombinedOutput()
			return string(o)
		}
		if _, err := os.Stat(b.vm.bin()); err != nil {
			b.vm.installDirect(t)
		}
		run(harnesses.GrokInitCommand(f.core.URL, body.Bot))
		out = run(harnesses.GrokProveCommand(f.core.URL, body.Bot, ""))
	case forThisBot(f.routineFor(b), body.Bot, env):
		// A routine's run has no file-writing tool, and nothing from the
		// body goes into the command: it runs the routine's command as
		// written.
		m := routineRunRE.FindStringSubmatch(f.routineFor(b))
		if m == nil {
			out = "no command to run in the routine"
			break
		}
		c := exec.Command(shell, "-c", m[1])
		c.Env = env
		o, _ := c.CombinedOutput()
		out = string(o)
	}
	f.mu.Lock()
	f.runs = append(f.runs, out)
	f.mu.Unlock()
}

// webhook adds a webhook whose runs land in bot, and returns its URL and
// key.
func (f *fakeCursor) webhook(id string, bot *grokBot) (url, key string) {
	key = "crsrKey" + id + strings.Repeat("Q", 40)
	// bot's VM exists by now, so this runs before its temp dir goes.
	f.t.Cleanup(f.stop)
	f.mu.Lock()
	f.hooks[id] = fakeHook{key: key, bot: bot}
	f.mu.Unlock()
	return f.srv.URL + agenthooks.CursorPath + id, key
}

// set changes a webhook's behavior.
func (f *fakeCursor) set(id string, change func(*fakeHook)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.hooks[id]
	change(&h)
	f.hooks[id] = h
}

// rotate gives a webhook a new key (the old one gets 401).
func (f *fakeCursor) rotate(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.hooks[id]
	h.key = "crsrRotated" + id + strings.Repeat("R", 40)
	f.hooks[id] = h
	return h.key
}

func (f *fakeCursor) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.hooks, id)
}

func (f *fakeCursor) answer(plan ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plan = plan
}

func (f *fakeCursor) got() []cursorReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cursorReq(nil), f.reqs...)
}

// of are the requests of one type.
func (f *fakeCursor) of(typ string) []cursorReq {
	var out []cursorReq
	for _, r := range f.got() {
		if r.Body.Type == typ {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeCursor) ran() []string {
	f.wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runs...)
}

// startGrok starts a core with Grok Bot's webhooks on, paced for a test.
func startGrok(t *testing.T, set func(*core.Options)) *coretest.Core {
	t.Helper()
	fastHooks(t)
	return coretest.StartWith(t, func(o *core.Options) {
		o.GrokWebhooks = true
		o.GrokPacing = agenthooks.CursorPacing{Spacing: 300 * time.Millisecond}
		// The limits out of the way (grok_limits_test.go tests them).
		high := dblimit.Limit{Max: 1_000_000}
		o.GrokLimits = appapi.GrokLimits{SavePerAddress: high, SavePerLink: high, DescribePerAddress: high,
			FailuresPerAddress: high, BindsPerUser: high, FailedProofs: high}
		if set != nil {
			set(o)
		}
	})
}

// grokSetUp runs the whole setup for app in bot: subscribe, the key step,
// the user's paste of the webhook (id) on the page, the run's proof. It
// returns the subscription, the call (with its session) and the setup
// token.
func grokSetUp(t *testing.T, c *coretest.Core, f *fakeCursor, app *wakeshift.Client, b *grokBot, user, topic, hookID string) (noteSub, caller, string) {
	t.Helper()
	call := b.call(user)
	sub, err := subscribe(app, call, topic, subOpts{})
	if err != nil {
		t.Fatal(err)
	}
	call.Session = sub.Session
	answer := b.keyStep(t, sub.Addition.Text)
	token := setupToken(t, answer)
	url, key := f.webhook(hookID, b)
	if code, out := save(t, c, token, url, key); code != 200 || out["awaiting_proof"] != true {
		t.Fatalf("save: %d %v", code, out)
	}
	waitOn(t, c, token)
	return sub, call, token
}
