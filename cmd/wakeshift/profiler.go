//go:build profiler

package main

import (
	"log/slog"
	"os"

	"cloud.google.com/go/profiler"
)

// startProfiler sends profiles to Cloud Profiler (WAKESHIFT_PROFILER=1), as
// service "wakeshift", version the Cloud Run revision (K_REVISION), so two
// revisions' profiles compare side by side. What it sends: CPU, heap,
// allocation and goroutine profiles (stack samples: function names and
// source lines), labeled with the service, revision, zone and instance.
// Nothing from requests: the server sets no pprof labels. A failure to
// start is logged and the server runs on without it.
//
// It's built in only with the profiler build tag, which the hosted
// deployment sets for staging's build. The client brings in about 250
// packages (gRPC, protobuf, OpenTelemetry, Google's auth libraries), which
// production's binary leaves out.
func startProfiler() {
	if err := profiler.Start(profiler.Config{Service: "wakeshift", ServiceVersion: os.Getenv("K_REVISION")}); err != nil {
		slog.Warn("wakeshift: Cloud Profiler didn't start", "err", err)
		return
	}
	slog.Info("wakeshift: sending profiles to Cloud Profiler")
}
