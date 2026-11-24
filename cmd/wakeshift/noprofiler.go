//go:build !profiler

package main

import "log/slog"

// startProfiler, in a build without the profiler tag (production's): the
// Cloud Profiler client isn't linked in, so WAKESHIFT_PROFILER=1 is logged
// and the server runs on without it. Build with -tags profiler to send
// profiles (profiler.go).
func startProfiler() {
	slog.Warn("wakeshift: WAKESHIFT_PROFILER is set, but this build has no Cloud Profiler (build it with -tags profiler)")
}
