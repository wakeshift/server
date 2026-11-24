package apps

import (
	"errors"
	"fmt"
	"testing"
)

// The refusals that tell app owners to write name the address each server
// is configured with (WAKESHIFT_SUPPORT_EMAIL), today's by default, with no
// package-wide setting; ErrSuspended still matches with errors.Is however
// it's wrapped.
func TestSupportMessages(t *testing.T) {
	for support, want := range map[string]string{"": "support@wakeshift.ai", "help@example.com": "help@example.com"} {
		if got := SuspendedMessage(support); got != "this app is suspended; its owners can write to "+want {
			t.Errorf("%q: suspended: %q", support, got)
		}
		if got := (WorkspaceLimitError{Cap: 5}).Message(support); got != "this app has reached its limit of 5 workspaces; forget users it no longer needs, or write to "+want+" to raise it" {
			t.Errorf("%q: limit: %q", support, got)
		}
	}
	if !errors.Is(fmt.Errorf("authenticating: %w", ErrSuspended), ErrSuspended) || errors.Is(ErrUnauthenticated, ErrSuspended) {
		t.Fatal("errors.Is")
	}
	var limit WorkspaceLimitError
	if err := fmt.Errorf("making a workspace: %w", WorkspaceLimitError{Cap: 7}); !errors.As(err, &limit) || limit.Cap != 7 || !IsWorkspaceLimit(err) {
		t.Fatal("errors.As")
	}
}
