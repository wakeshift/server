package streams

import "time"

// SetAfterClaim makes every stream call f right after it claims a batch,
// before writing it (nil: stop), so a test can act in between.
func SetAfterClaim(f func()) {
	if f == nil {
		afterClaim.Store(nil)
		return
	}
	afterClaim.Store(&f)
}

// SetBeforeClaim makes every stream's claim transaction call f before it
// claims (nil: stop); an error from f fails the transaction, as the
// database failing would.
func SetBeforeClaim(f func() error) {
	if f == nil {
		beforeClaim.Store(nil)
		return
	}
	beforeClaim.Store(&f)
}

// SetPumpRetryWait sets the first wait before a stream retries a failed
// claim transaction, returning a function that restores it.
func SetPumpRetryWait(d time.Duration) func() {
	old := pumpRetryWait
	pumpRetryWait = d
	return func() { pumpRetryWait = old }
}
