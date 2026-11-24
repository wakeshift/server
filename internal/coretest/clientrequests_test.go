package coretest_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/beacon"
	"github.com/wakeshift/server/internal/config"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// Client requests (appapi/clientrequests.go): a Codex conversation's first
// subscribing call gets the install offer as a form the SDK shows in
// Codex's own dialog, where the app is on WAKESHIFT_CODEX_ELICIT, its SDK
// offers the form, the turn is the user's, and the model isn't GPT-6.1
// Sol. The SDK reports the answer with the request's one-time token.

type requestOut struct {
	Token    string         `json:"token"`
	Kind     string         `json:"kind"`
	Message  string         `json:"message"`
	Schema   map[string]any `json:"schema"`
	Outcomes map[string]struct {
		Text      string `json:"text"`
		Placement string `json:"placement"`
		Beacon    string `json:"beacon"`
	} `json:"outcomes"`
}

type additionWithRequests struct {
	Text     string         `json:"text"`
	Trailer  string         `json:"trailer"`
	Meta     map[string]any `json:"meta"`
	Requests []requestOut   `json:"requests"`
}

// elicitCall is a Codex CLI thread's tool call: its turn trigger and model
// in Codex's turn metadata, and the kinds its SDK offers.
func elicitCall(user, thread, session, trigger, model string, offer bool) map[string]any {
	meta := map[string]any{"thread_id": thread, "turn_trigger": trigger, "model": model}
	call := map[string]any{"user": user, "client": "codex-mcp-client", "tool": "join", "meta": map[string]any{"x-codex-turn-metadata": meta}}
	if offer {
		call["client_requests"] = []string{"elicit.form"}
	}
	if session != "" {
		call["session"] = session
	}
	return call
}

func elicitCore(t *testing.T, apps ...string) *coretest.Core {
	t.Helper()
	return coretest.StartWith(t, func(o *core.Options) { o.CodexElicit = config.AllowlistOf(apps...) })
}

// subscribeWith subscribes the call (deferred) and returns its block and
// session handle.
func subscribeWith(t *testing.T, c *coretest.Core, app *wakeshift.Client, call map[string]any) (additionWithRequests, string) {
	t.Helper()
	var sub deferredOut
	if code, e := appAPI(t, c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "defer_addition": true}, &sub); code != 200 {
		t.Fatalf("subscribe: %d %s", code, e)
	}
	call["session"] = sub.Session
	var block additionWithRequests
	if code, e := appAPI(t, c, app, "/api/apps/v1/calls/addition", map[string]any{"call": call, "pending": sub.Pending}, &block); code != 200 {
		t.Fatalf("addition: %d %s", code, e)
	}
	return block, sub.Session
}

// subscribeRaw is subscribeWith for a goroutine: errors returned, not fatal.
func subscribeRaw(c *coretest.Core, app *wakeshift.Client, call map[string]any) (additionWithRequests, error) {
	var sub deferredOut
	var block additionWithRequests
	if code, e, err := appPost(c, app, "/api/apps/v1/event-subscriptions", map[string]any{"call": call, "name": "game.turn", "arguments": map[string]any{"table": "juniper"}, "defer_addition": true}, &sub); err != nil || code != 200 {
		return block, fmt.Errorf("subscribe: %d %s %v", code, e, err)
	}
	if code, e, err := appPost(c, app, "/api/apps/v1/calls/addition", map[string]any{"call": call, "pending": sub.Pending}, &block); err != nil || code != 200 {
		return block, fmt.Errorf("addition: %d %s %v", code, e, err)
	}
	return block, nil
}

func reportOutcome(t *testing.T, c *coretest.Core, app *wakeshift.Client, token, outcome string) (int, string) {
	t.Helper()
	return appAPI(t, c, app, "/api/apps/v1/requests/outcome", map[string]any{"token": token, "outcome": outcome}, nil)
}

func TestCodexElicitFormOnceThenTheAnswer(t *testing.T) {
	c := elicitCore(t, "poker")
	app := pokerApp(t, c, "poker")
	codex, _ := harnesses.Get("codex")
	first, session := subscribeWith(t, c, app, elicitCall("u1", "thread-1", "", "user", "gpt-6.2-sol", true))
	if len(first.Requests) != 1 {
		t.Fatalf("the first call's requests: %+v", first.Requests)
	}
	req := first.Requests[0]
	if req.Kind != "elicit.form" || !strings.HasPrefix(req.Token, "wse_") || !strings.Contains(req.Message, "In a terminal") || !strings.Contains(req.Message, "codex resume thread-1") {
		t.Fatalf("the request: %+v", req)
	}
	if len(req.Outcomes) != 6 || req.Outcomes["choice=setup"].Placement != "before" || req.Outcomes["timeout"].Text != first.Text || req.Outcomes["unsupported"].Text != first.Text {
		t.Fatalf("the outcomes: %+v (text %q)", req.Outcomes, first.Text)
	}
	// Every outcome's text within the bound every SDK enforces (the shared
	// fixture's): one past it and the SDK shows no form at all.
	var fixture struct {
		Bounds struct {
			OutcomeTextMax int `json:"outcome_text_max"`
		} `json:"bounds"`
	}
	if b, err := os.ReadFile("../../sdk/typescript/test/client-requests.json"); err != nil || json.Unmarshal(b, &fixture) != nil || fixture.Bounds.OutcomeTextMax == 0 {
		t.Fatalf("the shared fixture's bounds: %v", err)
	}
	for k, o := range req.Outcomes {
		if n := utf8.RuneCountInString(o.Text); n > fixture.Bounds.OutcomeTextMax {
			t.Errorf("outcome %s's text is %d characters, past the SDKs' %d", k, n, fixture.Bounds.OutcomeTextMax)
		}
	}
	if !strings.HasPrefix(first.Text, harnesses.SubscribeNote(codex, c.URL)) || first.Meta[beacon.MetaKey] == nil {
		t.Fatalf("the fallback text and beacon: %q %v", first.Text, first.Meta)
	}
	if !strings.HasPrefix(req.Outcomes["choice=setup"].Text, "To set up notifications here, run in a terminal:") || !strings.Contains(req.Outcomes["choice=later"].Text, "don't bring it up again") {
		t.Fatalf("the answers' texts: %+v", req.Outcomes)
	}
	tenant := userTenant(t, c, "poker", "u1")

	// While the dialog is open, another subscribing call in the conversation says so, and asks nothing.
	open, _ := subscribeWith(t, c, app, elicitCall("u1", "thread-1", session, "user", "gpt-6.2-sol", true))
	if len(open.Requests) != 0 || !strings.HasPrefix(open.Text, harnesses.CodexElicitPendingNote()) {
		t.Fatalf("while the dialog is open: %+v", open)
	}

	// The answer: recorded once; the same report again is fine, another is refused.
	if code, e := reportOutcome(t, c, app, req.Token, "choice=setup"); code != 200 {
		t.Fatalf("the outcome: %d %s", code, e)
	}
	if code, _ := reportOutcome(t, c, app, req.Token, "choice=setup"); code != 200 {
		t.Fatalf("the same outcome again: %d", code)
	}
	if code, e := reportOutcome(t, c, app, req.Token, "choice=later"); code != 409 || e != "outcome_taken" {
		t.Fatalf("another outcome: %d %s", code, e)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM sessions WHERE install_choice = 'setup'`); n != 1 {
		t.Fatalf("%d sessions with the answer", n)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'install.elicit' AND actor = 'app:poker' AND detail->>'outcome' = 'setup' AND detail->>'reported_by' = 'app'`); n != 1 {
		t.Fatalf("%d audit entries", n)
	}

	// A later subscribing call words the answer.
	later, _ := subscribeWith(t, c, app, elicitCall("u1", "thread-1", session, "user", "gpt-6.2-sol", true))
	want, _ := harnesses.CodexElicitAskedNote("setup", c.URL, "thread-1")
	if len(later.Requests) != 0 || !strings.HasPrefix(later.Text, want) {
		t.Fatalf("after the answer: %+v", later)
	}
}

// "Not now" is remembered for the conversation; an automatic decline or a
// cancel is not "Not now", and leaves today's note.
func TestCodexElicitLaterAndDecline(t *testing.T) {
	c := elicitCore(t, "poker")
	app := pokerApp(t, c, "poker")
	codex, _ := harnesses.Get("codex")
	for _, tc := range []struct{ outcome, want string }{
		{"choice=later", func() string { n, _ := harnesses.CodexElicitAskedNote("later", "", ""); return n }()},
		{"decline", harnesses.AskedNote(codex, c.URL)},
		{"cancel", harnesses.AskedNote(codex, c.URL)},
		{"timeout", harnesses.AskedNote(codex, c.URL)},
	} {
		thread := "thread-" + tc.outcome
		first, session := subscribeWith(t, c, app, elicitCall("u2", thread, "", "user", "gpt-6.2-sol", true))
		if len(first.Requests) != 1 {
			t.Fatalf("%s: no request", tc.outcome)
		}
		if code, e := reportOutcome(t, c, app, first.Requests[0].Token, tc.outcome); code != 200 {
			t.Fatalf("%s: %d %s", tc.outcome, code, e)
		}
		again, _ := subscribeWith(t, c, app, elicitCall("u2", thread, session, "user", "gpt-6.2-sol", true))
		if len(again.Requests) != 0 || !strings.HasPrefix(again.Text, tc.want) {
			t.Fatalf("%s: then %q", tc.outcome, again.Text)
		}
	}
	tenant := userTenant(t, c, "poker", "u2")
	if n := count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'install.elicit'`); n != 1 {
		t.Fatalf("%d audit entries: only the user's answers (later) are audited", n)
	}
}

// No form, and today's text, where any condition fails.
func TestCodexElicitOnlyWhereAllowed(t *testing.T) {
	c := elicitCore(t, "poker")
	app := pokerApp(t, c, "poker")
	other := pokerApp(t, c, "cards")
	codex, _ := harnesses.Get("codex")
	for name, tc := range map[string]struct {
		app  *wakeshift.Client
		call map[string]any
	}{
		"app not on the setting": {other, elicitCall("u3", "t-a", "", "user", "gpt-6.2-sol", true)},
		"SDK doesn't offer it":   {app, elicitCall("u3", "t-b", "", "user", "gpt-6.2-sol", false)},
		"codex exec":             {app, elicitCall("u3", "t-c", "", "exec", "gpt-6.2-sol", true)},
		"no turn trigger":        {app, elicitCall("u3", "t-d", "", "", "gpt-6.2-sol", true)},
		"GPT-6.1 Sol":            {app, elicitCall("u3", "t-e", "", "user", "gpt-6.1-sol", true)},
	} {
		block, _ := subscribeWith(t, c, tc.app, tc.call)
		if len(block.Requests) != 0 || !strings.HasPrefix(block.Text, harnesses.SubscribeNote(codex, c.URL)) {
			t.Errorf("%s: %+v", name, block)
		}
	}
	// The setting off: nothing.
	off := coretest.Start(t)
	offApp := pokerApp(t, off, "poker")
	if block, _ := subscribeWith(t, off, offApp, elicitCall("u3", "t-f", "", "user", "gpt-6.2-sol", true)); len(block.Requests) != 0 {
		t.Fatalf("the setting off: %+v", block.Requests)
	}
}

// Parallel subscribing calls in one conversation: one form, one full ask.
func TestCodexElicitParallelCallsAskOnce(t *testing.T) {
	c := elicitCore(t, "poker")
	app := pokerApp(t, c, "poker")
	_, session := subscribeWith(t, c, app, elicitCall("u4", "thread-p", "", "exec", "gpt-6.2-sol", true))
	// That call (exec) asked with text; reset the ask so the next two race for it.
	tenant := userTenant(t, c, "poker", "u4")
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET asked_harness = NULL`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	forms := make([]int, 4)
	errs := make([]error, 4)
	for i := range forms {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			block, err := subscribeRaw(c, app, elicitCall("u4", "thread-p", session, "user", "gpt-6.2-sol", true))
			forms[i], errs[i] = len(block.Requests), err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := forms[0] + forms[1] + forms[2] + forms[3]; n != 1 {
		t.Fatalf("%d forms from 4 parallel calls", n)
	}
}

// The token: one outcome, for the app that got it, within its expiry; every
// refusal is the same 404.
func TestRequestTokenRefusals(t *testing.T) {
	c := elicitCore(t, "poker", "cards")
	app := pokerApp(t, c, "poker")
	cards := pokerApp(t, c, "cards")
	first, _ := subscribeWith(t, c, app, elicitCall("u5", "thread-t", "", "user", "gpt-6.2-sol", true))
	tok := first.Requests[0].Token
	theirs, _ := subscribeWith(t, c, cards, elicitCall("u5", "thread-u", "", "user", "gpt-6.2-sol", true))
	other := theirs.Requests[0].Token

	for name, tc := range map[string]struct {
		app   *wakeshift.Client
		token string
	}{
		"malformed":                     {app, "wse_nope"},
		"unknown":                       {app, tok[:len(tok)-4] + "AAAA"},
		"another app's":                 {app, other},
		"for another app":               {cards, tok},
		"another workspace in its name": {app, "wse_" + userTenant(t, c, "cards", "u5") + tok[len("wse_")+36:]},
	} {
		if code, e := reportOutcome(t, c, tc.app, tc.token, "choice=setup"); code != 404 || e != "unknown_request" {
			t.Errorf("%s: %d %s", name, code, e)
		}
	}
	if code, _ := reportOutcome(t, c, app, tok, "choice=maybe"); code != 400 {
		t.Fatalf("an outcome that isn't one: %d", code)
	}
	// Expired, unanswered: 404.
	tenant := userTenant(t, c, "poker", "u5")
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE client_requests SET expires_at = now() - interval '1 second'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code, e := reportOutcome(t, c, app, tok, "choice=setup"); code != 404 || e != "unknown_request" {
		t.Fatalf("expired: %d %s", code, e)
	}
	// Nothing was stored for any refusal.
	if n := count(t, c, tenant, `SELECT count(*) FROM client_requests WHERE outcome IS NOT NULL`); n != 0 {
		t.Fatalf("%d outcomes stored", n)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM audit_log WHERE action = 'install.elicit'`); n != 0 {
		t.Fatalf("%d audit entries", n)
	}
}

// A late outcome from an older request never overwrites the pending choice
// a newer request set.
func TestRequestOutcomeOnlyAnswersItsOwnAsk(t *testing.T) {
	c := elicitCore(t, "poker")
	app := pokerApp(t, c, "poker")
	first, session := subscribeWith(t, c, app, elicitCall("u6", "thread-o", "", "user", "gpt-6.2-sol", true))
	old := first.Requests[0].Token
	// A newer ask in the same conversation (as after the old dialog's ask was reset).
	tenant := userTenant(t, c, "poker", "u6")
	if err := c.DB.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET asked_harness = NULL, install_choice = NULL`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	again, _ := subscribeWith(t, c, app, elicitCall("u6", "thread-o", session, "user", "gpt-6.2-sol", true))
	if len(again.Requests) != 1 {
		t.Fatalf("the newer ask: %+v", again.Requests)
	}
	if code, e := reportOutcome(t, c, app, old, "choice=later"); code != 200 {
		t.Fatalf("the old request's outcome: %d %s", code, e)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM sessions WHERE install_choice = 'pending'`); n != 1 {
		t.Fatalf("the newer request's pending choice was overwritten")
	}
	if code, e := reportOutcome(t, c, app, again.Requests[0].Token, "choice=setup"); code != 200 {
		t.Fatalf("the newer outcome: %d %s", code, e)
	}
	if n := count(t, c, tenant, `SELECT count(*) FROM sessions WHERE install_choice = 'setup'`); n != 1 {
		t.Fatalf("the newer request's answer wasn't stored")
	}
}
