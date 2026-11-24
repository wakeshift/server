package streams_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/wakeshift/server/internal/streams"
)

// A client that reconnects sends again the acks it had no receipt for. If
// the new connection has already claimed the delivery again (the old one
// handed it back), the ack lands between that claim and the write: the
// delivery is acknowledged and its receipt sent, so the frame claimed
// before mustn't follow it. A delivery never comes back after its receipt.
func TestNoRedeliveryAfterReceipt(t *testing.T) {
	f := setup(t)
	f.slack("acked, receipt lost")
	c := f.dial(true)
	first := mustRead(t, c)
	if first.Type != "event" {
		t.Fatalf("got %+v", first)
	}
	// Acked, but the connection drops before the server reads it.
	c.CloseNow()

	claimed, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	streams.SetAfterClaim(func() {
		once.Do(func() {
			close(claimed)
			<-proceed
		})
	})
	t.Cleanup(func() { streams.SetAfterClaim(nil) })

	c = f.dial(true)
	select {
	case <-claimed: // the new connection claimed it again, not yet written
	case <-time.After(10 * time.Second):
		t.Fatal("the delivery wasn't claimed again")
	}
	ack, _ := json.Marshal(map[string]string{"type": "ack", "delivery_id": first.DeliveryID})
	c.Write(ctx, websocket.MessageText, ack)
	receipt := mustRead(t, c)
	if receipt.Type != "acked" || receipt.DeliveryID != first.DeliveryID {
		close(proceed)
		t.Fatalf("receipt %+v", receipt)
	}
	close(proceed)
	if m, err := readWithin(t, c, 2*time.Second); err == nil {
		t.Fatalf("after its receipt, got %+v", m)
	}
	if s := f.state(first.DeliveryID); s != "acked" {
		t.Fatalf("state %s", s)
	}
}
