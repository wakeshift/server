package streams

import (
	"errors"
	"testing"
)

// Push streams take at most PushAppShare of an instance's MaxStreams per
// app and PushShare in all, so one app can't take every slot and the rest
// is always there for connectors and agents' own streams.
func TestPushShares(t *testing.T) {
	s := &Server{MaxStreams: 8} // push: 4 in all, 2 an app
	for i := range 2 {
		if err := s.AdmitPush("a"); err != nil {
			t.Fatalf("a's push %d: %v", i, err)
		}
	}
	if err := s.AdmitPush("a"); !errors.Is(err, errBusy) {
		t.Fatalf("a's third push: %v", err)
	}
	for i := range 2 {
		if err := s.AdmitPush("b"); err != nil {
			t.Fatalf("b's push %d: %v", i, err)
		}
	}
	if err := s.AdmitPush("c"); !errors.Is(err, errBusy) {
		t.Fatalf("c's push past push's share: %v", err)
	}
	// The reserve: other streams still get in, up to MaxStreams.
	for i := range 4 {
		if err := s.enter("link:x"); err != nil {
			t.Fatalf("a connector's stream %d: %v", i, err)
		}
	}
	if err := s.enter("link:y"); !errors.Is(err, errBusy) {
		t.Fatalf("past MaxStreams: %v", err)
	}
	// Leaving gives an app's room back.
	s.LeavePush("a")
	s.leave()
	if err := s.AdmitPush("c"); err != nil {
		t.Fatalf("c after a left: %v", err)
	}
	if s.pushes != 4 || s.pushesByApp["a"] != 1 || s.pushesByApp["c"] != 1 {
		t.Fatalf("counts: %d %v", s.pushes, s.pushesByApp)
	}
}
