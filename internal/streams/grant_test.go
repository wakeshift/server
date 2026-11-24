package streams_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/internal/dbtest"
	"github.com/wakeshift/server/internal/streams"
)

// A link made under a grant stops working at the grant's end date, before
// the expiry pass revokes it: an open stream closes, and it can't connect.
func TestLinksEndWithTheirGrant(t *testing.T) {
	f := setup(t)
	f.s.Renew = 200 * time.Millisecond
	if _, err := f.d.Pool.Exec(ctx, `INSERT INTO apps (id, name) VALUES ('pacer', 'Pacer')`); err != nil {
		t.Fatal(err)
	}
	var grant, u string
	f.tx(func(tx pgx.Tx) (err error) {
		grant = dbtest.Grant(t, tx, "pacer", "codex", time.Hour)
		u, _, err = f.s.Mint(ctx, tx, f.tenant, streams.NewLink{Agent: "codex", CreatedBy: "app:pacer", GrantID: grant, TTL: time.Hour})
		return err
	})
	c, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	f.tx(func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE grants SET expires_at = now() - interval '1 second' WHERE id = $1`, grant)
		return err
	})
	if _, err := read(t, c); websocket.CloseStatus(err) != streams.CodeRevoked {
		t.Fatalf("open stream after the grant's end: %v", err)
	}
	if _, resp, err := websocket.Dial(ctx, u, nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("connecting after the grant's end: %v", err)
	}
}

// A hook's poll waiting beside a background command's wait is held within
// the instance's cap, as any poll is.
func TestBackgroundPollCountsTowardTheCap(t *testing.T) {
	f := setup(t)
	base := f.waitServer()
	link, _ := f.sessionLinkFor("agy", streams.SessionLinkTTL)
	waited, polled := make(chan waited), make(chan struct{})
	go func() { waited <- wait(t, waitURL(base, link, "timeout=3s")) }()
	time.Sleep(300 * time.Millisecond)
	before := f.s.Active()
	go func() { mustPoll(t, base, token(link), "wait=1s"); close(polled) }()
	time.Sleep(300 * time.Millisecond)
	if got := f.s.Active(); got != before+1 {
		t.Fatalf("held %d with the poll waiting, %d before", got, before)
	}
	<-polled
	<-waited
}

// A stream whose lease and link were deleted underneath it (its workspace
// erased) closes at its next renewal, rather than renewing nothing until
// its maximum age.
func TestStreamWhoseLeaseVanishedCloses(t *testing.T) {
	f := setup(t)
	f.s.Renew = 200 * time.Millisecond
	c := f.dial(false)
	time.Sleep(100 * time.Millisecond)
	f.tx(func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM stream_leases`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM stream_links`)
		return err
	})
	if _, err := read(t, c); websocket.CloseStatus(err) != streams.CodeRevoked {
		t.Fatalf("after its lease vanished: %v", err)
	}
}
