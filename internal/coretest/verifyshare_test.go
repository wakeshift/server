package coretest_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/wakeshift/server/internal/appapi"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/dblimit"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// setLimit sets one verification limit's Max for a test.
func setLimit(t *testing.T, l *dblimit.Limit, max int) {
	was := *l
	l.Max = max
	t.Cleanup(func() { *l = was })
}

// One app's made-up principals, with callbacks that never echo, can't use
// up a shared callback host's verification budget: its account's failed
// verifications there stop it first (VerifyFailuresPerOwnerHost), and
// what it sent is within its share (VerifyPerOwnerHost), so another app's
// real user at that host verifies. Past its failures, the app's
// subscribes there send nothing.
func TestVerifyFailingAppDoesntBlockAHostForOthers(t *testing.T) {
	eventsTestSetup(t)
	setVerifyLimits(t, 2, 6) // scaled down from 30 and 600
	setLimit(t, &appapi.VerifyPerOwnerHost, 3)
	setLimit(t, &appapi.VerifyFailuresPerOwnerHost, 2)
	c := coretest.Start(t)
	attacker := eventsApp(t, c, "evil")
	victim := eventsApp(t, c, "chat")
	hook := newFakeChatGPT(t) // stands in for ChatGPT's callback host
	secret := hook.secret()
	hook.setEcho(false)
	for i := range 3 { // 3 made-up principals x 2: the host's whole 6, before
		for _, ch := range []string{"a", "b"} {
			if code, _, _, out := webhookSub(t, c, attacker, fmt.Sprint("fake", i), ch, hook.url()+"/nope", secret); code == 200 {
				t.Fatalf("the attacker's subscribe verified: %v", out)
			}
		}
	}
	if n := hook.verifications(); n != 2 {
		t.Fatalf("the attacker's app sent the host %d verifications, want 2 (its failures there)", n)
	}
	// Refused without sending, as challenge_failed.
	code, e, reason, out := webhookSub(t, c, attacker, "fake9", "a", hook.url()+"/nope", secret)
	refusedAtLimit(t, "the attacker past its failures", code, e, reason, out)
	if hook.verifications() != 2 {
		t.Fatalf("a refusal sent a verification: %d", hook.verifications())
	}
	hook.setEcho(true)
	if code, e, reason, _ := webhookSub(t, c, victim, "realuser", "eng", hook.url(), secret); code != 200 {
		t.Fatalf("the victim app's real user at the same host: %d %s %s", code, e, reason)
	}
	// The attacker's app is still held back there, even with a callback
	// that echoes now: its failures last the window.
	code, e, reason, out = webhookSub(t, c, attacker, "fake10", "a", hook.url(), secret)
	refusedAtLimit(t, "the attacker within the window", code, e, reason, out)
}

// An app's verifications that succeed are within its account's share of
// a host (VerifyPerOwnerHost): past it, its subscribes there are refused
// and send nothing; another app's go through. Two apps one account made
// share one count.
func TestVerifyPerOwnerHostShare(t *testing.T) {
	eventsTestSetup(t)
	setVerifyLimits(t, 30, 100)
	setLimit(t, &appapi.VerifyPerOwnerHost, 3)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	sibling := eventsApp(t, c, "chat2")
	other := eventsApp(t, c, "notes")
	var user string
	if err := c.DB.Pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ('dev@example.com') RETURNING id::text`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DB.Pool.Exec(ctx, `UPDATE apps SET created_by = $1 WHERE id IN ('chat', 'chat2')`, user); err != nil {
		t.Fatal(err)
	}
	hook := newFakeChatGPT(t)
	secret := hook.secret()
	for i, a := range []*wakeshift.Client{app, app, sibling} {
		if code, e, _, _ := webhookSub(t, c, a, fmt.Sprint("u", i), "eng", hook.url(), secret); code != 200 {
			t.Fatalf("verification %d within the account's share: %d %s", i, code, e)
		}
	}
	for name, a := range map[string]*wakeshift.Client{"chat": app, "chat2": sibling} {
		code, e, reason, out := webhookSub(t, c, a, "u9", "eng", hook.url(), secret)
		refusedAtLimit(t, name+" past its account's share", code, e, reason, out)
	}
	if hook.verifications() != 3 {
		t.Fatalf("%d verifications, want 3", hook.verifications())
	}
	if code, e, _, _ := webhookSub(t, c, other, "u1", "eng", hook.url(), secret); code != 200 || hook.verifications() != 4 {
		t.Fatalf("another account's app at the host: %d %s, %d POSTs", code, e, hook.verifications())
	}
}

// Verifications are counted per address too (VerifyPerAddr): names that
// resolve to one server (a wildcard DNS record's) share its count, though
// each name has a count of its own. Past it, nothing is sent there.
func TestVerifyPerAddr(t *testing.T) {
	eventsTestSetup(t)
	setVerifyLimits(t, 30, 100)
	setLimit(t, &appapi.VerifyPerAddr, 1)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	hook := newFakeChatGPT(t)
	secret := hook.secret()
	if code, e, _, _ := webhookSub(t, c, app, "u1", "eng", hook.url(), secret); code != 200 || hook.verifications() != 1 {
		t.Fatalf("the address's first: %d %s", code, e)
	}
	byName := strings.Replace(hook.url(), "127.0.0.1", "localhost", 1)
	code, e, reason, out := webhookSub(t, c, app, "u2", "eng", byName, secret)
	refusedAtLimit(t, "another name for the same address", code, e, reason, out)
	if hook.verifications() != 1 {
		t.Fatalf("a refusal sent a verification: %d", hook.verifications())
	}
}

// A verification refused at any limit counts under none: subscribes racing
// for a host's last places use up nothing of their account's share, or
// their principals' own counts, when the host refuses them.
func TestVerifyRefusalCountsNothing(t *testing.T) {
	eventsTestSetup(t)
	setVerifyLimits(t, 1, 3)
	setLimit(t, &appapi.VerifyPerOwnerHost, 5)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	hook := newFakeChatGPT(t)
	secret := hook.secret()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var verified, refused []string
	for i := range 10 {
		wg.Go(func() {
			u := fmt.Sprint("r", i)
			code, _, _, _ := webhookSub(t, c, app, u, "eng", hook.url(), secret)
			mu.Lock()
			defer mu.Unlock()
			if code == 200 {
				verified = append(verified, u)
			} else {
				refused = append(refused, u)
			}
		})
	}
	wg.Wait()
	if len(verified) != 3 || hook.verifications() != 3 {
		t.Fatalf("%v verified, %d POSTs, want the host's 3", verified, hook.verifications())
	}
	// With room at the host, the account has exactly its 2 left, and a
	// principal the host refused still has its own 1.
	appapi.VerifyPerHost.Max = 100
	for _, u := range []string{"r-new", refused[0]} {
		if code, e, _, _ := webhookSub(t, c, app, u, "ops", hook.url()+"/"+u, secret); code != 200 {
			t.Fatalf("%s after the race: %d %s", u, code, e)
		}
	}
	code, e, reason, out := webhookSub(t, c, app, "r-last", "eng", hook.url(), secret)
	refusedAtLimit(t, "past the account's share", code, e, reason, out)
}

// An account's failure limits hold under a burst: each verification counts
// as a failure as it's sent, so of many failing ones at once only the
// limit's worth go out; one that succeeds gives its count back.
func TestVerifyFailureLimitHoldsUnderABurst(t *testing.T) {
	eventsTestSetup(t)
	setVerifyLimits(t, 30, 100)
	setLimit(t, &appapi.VerifyFailuresPerOwnerHost, 2)
	c := coretest.Start(t)
	app := eventsApp(t, c, "chat")
	hook := newFakeChatGPT(t)
	secret := hook.secret()
	// Successes: none of them counts as a failure.
	for i := range 4 {
		if code, e, _, _ := webhookSub(t, c, app, fmt.Sprint("ok", i), "eng", hook.url(), secret); code != 200 {
			t.Fatalf("success %d: %d %s", i, code, e)
		}
	}
	hook.setEcho(false)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() { webhookSub(t, c, app, fmt.Sprint("bad", i), "eng", hook.url()+"/nope", secret) })
	}
	wg.Wait()
	if n := hook.verifications() - 4; n != 2 {
		t.Fatalf("%d failing verifications went out at once, want the limit's 2", n)
	}
}
