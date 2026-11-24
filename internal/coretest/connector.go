package coretest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var (
	connectorOnce sync.Once
	connectorDir  string
	connectorErr  error
)

// ConnectorDir builds Wakeshift's connector (cmd/wakeshift-connector) for
// this machine once per test binary and returns the directory it's in,
// named as the server serves it (wakeshift-<os>-<arch>): for
// core.Options.ConnectorDir, so the tests' Grok Bots install and run the
// real binary.
func ConnectorDir(t testing.TB) string {
	t.Helper()
	connectorOnce.Do(func() {
		dir, err := os.MkdirTemp("", "wakeshift-connector-")
		if err != nil {
			connectorErr = err
			return
		}
		out := filepath.Join(dir, "wakeshift-"+runtime.GOOS+"-"+runtime.GOARCH)
		b, err := exec.Command("go", "build", "-o", out, "github.com/wakeshift/server/cmd/wakeshift-connector").CombinedOutput()
		if err != nil {
			connectorErr = &buildError{err: err, out: string(b)}
			return
		}
		connectorDir = dir
	})
	if connectorErr != nil {
		t.Fatalf("building the connector: %v", connectorErr)
	}
	return connectorDir
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.out }
