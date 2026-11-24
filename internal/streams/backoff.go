package streams

import (
	"errors"
	mrand "math/rand/v2"
	"strconv"
	"sync"
	"time"
)

// Refusing at capacity: a refused client is told when to come back
// (Retry-After), and told to wait longer each time it's refused again, so
// a full instance isn't answering hundreds of retries a second. Counted per
// link (a poll: per stream), in memory on this instance; a stream it
// admits starts again from the first wait.

// refusalWaits are the waits for the first, second and later refusals of
// the same link; each is spread up to half again, so refused clients don't
// all come back at once. The longest (90s) stays inside what the clients
// honor (the connectors and plugins cap Retry-After at 120s).
var refusalWaits = []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second}

const (
	// refusalMemory: a link not refused for this long starts again from
	// the first wait.
	refusalMemory = 5 * time.Minute
	// refusalsKept bounds the memory: past it, forgotten refusals are
	// dropped, and if that's not enough, all of them.
	refusalsKept = 50_000
)

// busyError is an at-capacity refusal (errors.Is errBusy) with its wait;
// first: its key's first refusal since it last got in (or was forgotten).
type busyError struct {
	after time.Duration
	first bool
}

func (e busyError) Error() string        { return errBusy.Error() }
func (e busyError) Is(target error) bool { return target == errBusy }

// retryAfterOf is the Retry-After for a refusal (whole seconds).
func retryAfterOf(err error) string {
	var b busyError
	if errors.As(err, &b) {
		return strconv.Itoa(int(b.after.Seconds()))
	}
	return strconv.Itoa(int(refusalWaits[0].Seconds()))
}

type refusal struct {
	n    int
	last time.Time
}

// refusals counts recent at-capacity refusals per key.
type refusals struct {
	mu sync.Mutex
	m  map[string]refusal
}

// refused records a refusal of key and returns how long its client should
// wait.
func (r *refusals) refused(key string, now time.Time) time.Duration {
	d, _ := r.refusedN(key, now)
	return d
}

// refusedN is refused, also returning which refusal of key in a row this
// is (1: the first).
func (r *refusals) refusedN(key string, now time.Time) (time.Duration, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]refusal{}
	}
	f, known := r.m[key]
	if now.Sub(f.last) > refusalMemory {
		f.n = 0
	}
	f.n++
	f.last = now
	if !known && len(r.m) >= refusalsKept {
		for k, v := range r.m {
			if now.Sub(v.last) > refusalMemory {
				delete(r.m, k)
			}
		}
		if len(r.m) >= refusalsKept {
			clear(r.m)
		}
	}
	r.m[key] = f
	d := refusalWaits[min(f.n, len(refusalWaits))-1]
	return d + time.Duration(mrand.Int64N(int64(d/2))), f.n
}

// admitted forgets key's refusals: it got in.
func (r *refusals) admitted(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, key)
}
