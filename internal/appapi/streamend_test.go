package appapi

import (
	"context"
	"testing"

	"github.com/wakeshift/server/internal/streams"
)

// A link whose target isn't a session ID (not a uuid) has no session to
// end, and isn't looked up.
func TestStreamEndingWithoutASession(t *testing.T) {
	for _, target := range []string{"", "not-a-uuid", "cursor#2"} {
		for _, valid := range []bool{true, false} {
			if e, ok, err := (&API{}).StreamEnding(context.Background(), nil, streams.Link{Target: target}, valid); ok || err != nil || e != (streams.Ending{}) {
				t.Fatalf("%q (valid %v): %+v %v %v", target, valid, e, ok, err)
			}
		}
	}
}
