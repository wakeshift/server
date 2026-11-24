package apps

import "testing"

// One cap for every app, whatever its mode, unless we've overridden it;
// the hourly rate follows it.
func TestWorkspaceCap(t *testing.T) {
	if WorkspaceCap(App{}) != 10_000 || WorkspacesPerHour(App{}) != 2_000 {
		t.Fatal("one cap")
	}
	n, zero := 500, 0
	if WorkspaceCap(App{WorkspaceCapOverride: &n}) != 500 || WorkspacesPerHour(App{WorkspaceCapOverride: &n}) != 100 {
		t.Fatal("an override")
	}
	if WorkspaceCap(App{WorkspaceCapOverride: &zero}) != 0 || WorkspacesPerHour(App{WorkspaceCapOverride: &zero}) != 1 {
		t.Fatal("an override of 0")
	}
	if err := (WorkspaceLimitError{Cap: 5}); !IsWorkspaceLimit(err) || err.Error() == "" {
		t.Fatal("the limit error")
	}
}
