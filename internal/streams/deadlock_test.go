package streams_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/connectorversion"
	"github.com/wakeshift/server/internal/deliveries"
	"github.com/wakeshift/server/internal/sessions"
	"github.com/wakeshift/server/internal/streams"
)

func validAlways(context.Context, pgx.Tx) error { return nil }

// A poll that acks what the last holder had, just as the hooks take the
// session's stream over (the redeem: the session's row, then TakeOver: the
// lease, then the last holder's deliveries), waits for the takeover rather
// than deadlocking with it: the poll locks the session's row and the
// lease before its acks lock deliveries.
func TestPollAckingDuringATakeover(t *testing.T) {
	f := setup(t)
	_, id := f.sessionLink("x")
	f.toSession(id, "one")
	b, err := f.s.LongPoll(ctx, streams.PollRequest{Tenant: f.tenant, Agent: "codex", Target: id, Valid: validAlways, Wait: time.Second})
	if err != nil || len(b.Deliveries) != 1 {
		t.Fatalf("first poll: %+v %v", b, err)
	}
	d := b.Deliveries[0].DeliveryID

	polled := make(chan error, 1)
	var acked []string
	err = f.d.InTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		// The redeem's update of the session, held open while the poll
		// starts.
		if _, err := tx.Exec(ctx, `UPDATE sessions SET connector_seen_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		go func() {
			b, err := f.s.LongPoll(ctx, streams.PollRequest{Tenant: f.tenant, Agent: "codex", Target: id, Valid: validAlways,
				Acks: []string{d}, Wait: 100 * time.Millisecond})
			acked = b.Acked
			polled <- err
		}()
		time.Sleep(300 * time.Millisecond) // the poll is waiting now
		return streams.TakeOver(ctx, tx, "codex", id)
	})
	perr := <-polled
	if err != nil {
		t.Fatalf("the takeover: %v", err)
	}
	if perr != nil {
		t.Fatalf("the poll: %v", perr)
	}
	if len(acked) != 1 || acked[0] != d || f.state(d) != "acked" {
		t.Fatalf("acked %v, state %s", acked, f.state(d))
	}
}

// Two requests acking the same deliveries, listed in different orders,
// don't deadlock: each acks them in one order. What's recorded is still
// reported as it was listed, repeats included.
func TestAcksInAnyOrderDontDeadlock(t *testing.T) {
	f := setup(t)
	_, id := f.sessionLink("x")
	f.toSession(id, "one")
	f.toSession(id, "two")
	b, err := f.s.LongPoll(ctx, streams.PollRequest{Tenant: f.tenant, Agent: "codex", Target: id, Valid: validAlways, Wait: time.Second})
	if err != nil || len(b.Deliveries) != 2 {
		t.Fatalf("poll: %+v %v", b, err)
	}
	ids := []string{b.Deliveries[0].DeliveryID, b.Deliveries[1].DeliveryID}
	slices.Sort(ids)
	reversed := []string{ids[1], ids[0], ids[1]}

	batch := make(chan error, 1)
	var acked []string
	err = f.d.InTenant(ctx, f.tenant, func(tx pgx.Tx) error {
		// Another request acking both, the first of them now.
		if _, err := deliveries.AckFor(ctx, tx, "codex", id, "", ids[0]); err != nil {
			return err
		}
		go func() {
			var err error
			acked, err = f.s.AckBatch(ctx, f.tenant, "codex", id, "", validAlways, reversed)
			batch <- err
		}()
		time.Sleep(300 * time.Millisecond) // the batch is waiting now
		_, err := deliveries.AckFor(ctx, tx, "codex", id, "", ids[1])
		return err
	})
	berr := <-batch
	if err != nil {
		t.Fatalf("the first request: %v", err)
	}
	if berr != nil {
		t.Fatalf("the batch: %v", berr)
	}
	// Acked again (a retry): reported as listed.
	if acked, err = f.s.AckBatch(ctx, f.tenant, "codex", id, "", validAlways, reversed); err != nil || !slices.Equal(acked, reversed) {
		t.Fatalf("acked %v (%v), want %v", acked, err, reversed)
	}
}

// A request on a connector's link that records a new connector version
// (seen: the session's row and the link's), a grace renewal of another
// link of the same session, and an unsubscribe stopping the session, all
// at once. The renewal and the unsubscribe lock the session's row before
// the link's; seen did it the other way round (the link marked used, then
// the session), so the request held the link the unsubscribe was about
// to revoke while it waited for the session the unsubscribe held, and
// Postgres broke the deadlock by failing one of them (a 503, or the
// unsubscribe's error). Now each takes the session first: the unsubscribe
// goes through, and both requests are refused as on a stopped session.
func TestSeenRenewalAndUnsubscribeDontDeadlock(t *testing.T) {
	f := setup(t)
	base := f.pollServer()
	link, id := f.graceSession("racing", false, time.Hour)
	var monitor string
	f.tx(func(tx pgx.Tx) (err error) {
		monitor, err = f.mintFor(tx, id, true, time.Hour) // a Monitor's link: its own chain
		return err
	})
	f.expire(monitor, time.Minute)

	locked, release, stopped := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		stopped <- f.d.InTenant(ctx, f.tenant, func(tx pgx.Tx) error {
			// As appapi's endListening: lockWatching, then stopListening.
			if _, err := tx.Exec(ctx, `SELECT 1 FROM sessions WHERE id = $1::uuid FOR NO KEY UPDATE`, id); err != nil {
				return err
			}
			if err := sessions.SetWatching(ctx, tx, id, false); err != nil {
				return err
			}
			close(locked)
			<-release
			_, err := streams.RevokeSessionLinks(ctx, tx, "codex", id)
			return err
		})
	}()
	<-locked

	type answer struct {
		what    string
		code    int
		rotated bool
		err     error
	}
	answered := make(chan answer, 2)
	ask := func(what, tok, connector string) {
		req, _ := http.NewRequest("GET", base+"/v1/stream/poll?wait=0", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if connector != "" {
			req.Header.Set(connectorversion.Header, connector)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answered <- answer{what: what, err: err}
			return
		}
		defer resp.Body.Close()
		var b pollBody
		json.NewDecoder(resp.Body).Decode(&b)
		answered <- answer{what, resp.StatusCode, b.Rotate != nil, nil}
	}
	// The connector, updated: its link and its session record the new version.
	go ask("the connector's poll", token(link), "wakeshift/9.9.10 claude-code")
	go ask("the renewal", token(monitor), "")
	select {
	case a := <-answered:
		close(release)
		t.Fatalf("%s answered (%d, rotated %v, %v) while the session was stopping", a.what, a.code, a.rotated, a.err)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("the unsubscribe: %v", err)
	}
	for range 2 {
		if a := <-answered; a.err != nil || a.code != http.StatusUnauthorized || a.rotated {
			t.Errorf("%s after the stop: %d, rotated %v, %v", a.what, a.code, a.rotated, a.err)
		}
	}
	var live int
	f.tx(func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM stream_links WHERE target = $1 AND revoked_at IS NULL`, id).Scan(&live)
	})
	if live != 0 {
		t.Fatalf("%d live links for the stopped session", live)
	}
}
