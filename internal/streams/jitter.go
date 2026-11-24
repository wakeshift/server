package streams

import (
	"math/rand/v2"
	"time"
)

// phase is a random point in [0, d): when a periodic timer first fires, so
// timers started together (every stream after a deploy or a failover)
// don't stay in step.
func phase(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return rand.N(d)
}

// jitterDown is d shortened by up to a fifth, at random: for limits (a
// stream's or a round's age) that mustn't grow past what they promise.
func jitterDown(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - rand.N(d/5+1)
}

// jitter is d moved by up to a fifth either way, at random: for retries.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - d/5 + rand.N(2*(d/5)+1)
}
