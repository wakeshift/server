package appapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/wakeshift/server/internal/db"
)

// lockWaiters returns once n transactions in d's database wait on a lock,
// so a deadlock test knows each of its transactions got where it waits
// before the one holding them back lets go.
func lockWaiters(t *testing.T, d *db.DB, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var got int
		if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d transactions wait on a lock, not %d", got, n)
		}
	}
}

// copiesMade fails t if a publish's copy is still to be made.
func copiesMade(t *testing.T, d *db.DB) {
	t.Helper()
	var left int
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM topic_fanout`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d copies not made", left)
	}
}
