package coretest_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/pkg/wakeshift"
)

// heldNotShown waits until a publish is a pending delivery for the topic
// subscription subID (what arrives waits for the session's listener), then
// checks that the conversation's next result doesn't hand it over or say
// to call again: no result delivers messages (the app's tool may have side
// effects, and its own tools are how the agent checks).
func heldNotShown(t *testing.T, c *coretest.Core, app *wakeshift.Client, call caller, subID, text string) {
	t.Helper()
	tenant, sub := topicSubRow(t, c, subID)
	waitFor(t, "the delivery of "+text, func() bool {
		return count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE subscription_id = $1 AND state = 'pending'`, sub) > 0
	})
	r, err := annotate(app, call)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Text, text) || strings.Contains(r.Text, "again") {
		t.Fatalf("a result handed over %q or asked for another call: %q", text, r.Text)
	}
	time.Sleep(50 * time.Millisecond)
	if n := count(t, c, tenant, `SELECT count(*) FROM deliveries WHERE subscription_id = $1 AND state = 'pending'`, sub); n == 0 {
		t.Fatalf("%q isn't held any more", text)
	}
}

// takeByWait takes what's waiting for a Claude Code conversation through
// the one-shot wait its Monitor command (in text) runs, and confirms it, as
// the Monitor's next round does: the listener held messages reach. It
// returns the wait's lines, as text.
func takeByWait(t *testing.T, text string) string {
	t.Helper()
	m := monitorRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no Monitor command: %s", text)
	}
	lines := waitLines(t, m[2])
	var b strings.Builder
	after := ""
	for _, l := range lines {
		if l["type"] == "cursor" {
			after, _ = l["after"].(string)
		}
		j, _ := json.Marshal(l)
		b.Write(j)
		b.WriteByte('\n')
	}
	if after != "" {
		waitLines(t, m[2]+"&after="+url.QueryEscape(after)+"&confirm=1")
	}
	return b.String()
}
