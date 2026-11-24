package coretest_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/streams"
)

// A once subscription's delivery is the last thing its conversation will
// get, so its stream then has nothing left. It still stays open until the
// listener acknowledges that delivery: a renewal between the write and the
// ack mustn't close it (4001, "nothing more to do"), which would hand the
// delivery back to a stream that's over, so it waited for the agent's next
// tool call (in testing, Codex's stream closed right after the write). Once it's acked, the stream ends promptly, saying why.
func TestOnceFinalDeliveryIsAckedBeforeTheStreamCloses(t *testing.T) {
	const renew = time.Second
	c := coretest.StartWith(t, func(o *core.Options) { o.StreamRenew = renew })
	app := noteApp(t, c, "ci")
	const thread = "019a14bd-bbbc-7b70-a723-0abb9f38f1a1"
	call := caller{User: "ada", Client: "codex-mcp-client", Meta: map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": thread}}}
	sub, err := subscribe(app, call, "crawls/acme", subOpts{Once: true})
	if err != nil {
		t.Fatal(err)
	}
	claims := codexBeacon(t, c, sub.Addition, thread)
	code, redeemed := redeemTicket(t, claims.Redeem, claims.Ticket, thread)
	if code != 200 {
		t.Fatalf("redeem: %d %v", code, redeemed)
	}
	tenant := strings.SplitN(claims.Ticket, "_", 3)[1]
	ws, _, err := websocket.Dial(ctx, redeemed["url"].(string), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	type read struct {
		b   []byte
		err error
	}
	reads := make(chan read, 16)
	go func() {
		for {
			_, b, err := ws.Read(ctx)
			reads <- read{b, err}
			if err != nil {
				return
			}
		}
	}()
	waitFor(t, "connected", func() bool {
		r, _ := annotate(app, call)
		return len(r.Meta) == 0
	})

	if p, err := emitNote(app, []string{"crawls/acme"}, note{Text: "crawl done"}); err != nil || p.Matched != 1 {
		t.Fatalf("emit: %+v %v", p, err)
	}
	var f frame
	select {
	case r := <-reads:
		if r.err != nil {
			t.Fatalf("closed before the delivery: %v", r.err)
		}
		if json.Unmarshal(r.b, &f); f.Type != "event" || f.Event.Subject != "crawl done" {
			t.Fatalf("first frame: %s", r.b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}

	// The listener is slow to acknowledge (a connector starting Codex's
	// turn): renewals pass, and the stream must stay.
	select {
	case r := <-reads:
		t.Fatalf("the stream wrote or closed before the ack: %s %v", r.b, r.err)
	case <-time.After(5 * renew / 2):
	}
	if s := deliveryState(t, c, tenant, f.DeliveryID); s != "in_flight" {
		t.Fatalf("before the ack the delivery is %s", s)
	}

	acked := time.Now()
	a, _ := json.Marshal(map[string]string{"type": "ack", "delivery_id": f.DeliveryID})
	if err := ws.Write(ctx, websocket.MessageText, a); err != nil {
		t.Fatal(err)
	}
	var closed error
	for closed == nil {
		select {
		case r := <-reads:
			closed = r.err
		case <-time.After(5 * time.Second):
			t.Fatal("the stream didn't end after the ack")
		}
	}
	if websocket.CloseStatus(closed) != streams.CodeRevoked || !strings.Contains(closed.Error(), `reason = "once"`) {
		t.Fatalf("the stream ended with %v", closed)
	}
	// At once, not at a later renewal.
	if took := time.Since(acked); took > renew/2 {
		t.Fatalf("ended %v after the ack (renewal %v)", took, renew)
	}
	// Acknowledged, not handed back for the next tool call.
	if s := deliveryState(t, c, tenant, f.DeliveryID); s != "acked" {
		t.Fatalf("after the close the delivery is %s", s)
	}
}

func deliveryState(t *testing.T, c *coretest.Core, tenant, id string) string {
	t.Helper()
	var state string
	if err := c.DB.InTenant(context.Background(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM deliveries WHERE id = $1::uuid`, id).Scan(&state)
	}); err != nil {
		t.Fatal(err)
	}
	return state
}
