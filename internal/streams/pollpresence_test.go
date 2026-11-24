package streams

import (
	"testing"
	"time"
)

// A poll renews its presence in the check it runs on its tick, once Renew
// is (all but a tenth) due, not in a transaction of its own.
func TestPresenceDue(t *testing.T) {
	renew := 30 * time.Second
	for _, c := range []struct {
		ago  time.Duration
		want bool
	}{{0, false}, {20 * time.Second, false}, {27 * time.Second, true}, {30 * time.Second, true}, {time.Minute, true}} {
		if got := presenceDue(time.Now().Add(-c.ago), renew); got != c.want {
			t.Errorf("presenceDue(%v ago) = %v, want %v", c.ago, got, c.want)
		}
	}
}
