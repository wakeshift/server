package db_test

import (
	"context"
	"sync"
	"testing"

	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/dbtest"
)

// Several instances starting together on a fresh database (production's
// first deploy) all migrate at once. One migrates; the others wait. A
// waiter must not hold anything the migration waits for: CREATE INDEX
// CONCURRENTLY (040, 041) waits for every older transaction to end,
// including a waiter's own statement, so a waiter blocked inside
// pg_advisory_lock made a deadlock and that instance exited.
func TestConcurrentOpen(t *testing.T) {
	url := dbtest.EmptyURL(t)
	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			d, err := db.Open(context.Background(), url)
			errs[i] = err
			if err == nil {
				d.Close()
			}
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("instance %d: %v", i, err)
		}
	}
}
