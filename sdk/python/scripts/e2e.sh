#!/bin/sh
# Runs the SDK's end-to-end tests against a real core: starts one with Go's
# TestSDKHarness (a self-serve app with a test key, on a test database),
# waits for it to write its URL and key, runs tests/test_e2e.py, then stops it.
# Needs Postgres (make db), Go and uv.
set -eu
sdk=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$sdk/../.." && pwd)
dir=$(mktemp -d)
out="$dir/harness.json"
log="$dir/harness.log"
: "${WAKESHIFT_TEST_DATABASE_URL:=postgres://postgres:postgres@localhost:5439/postgres?sslmode=disable}"
export WAKESHIFT_TEST_DATABASE_URL

# Compile the core first, so the start deadline below times its start and
# not a cold build on a busy runner.
trap 'rm -rf "$dir"' EXIT
(cd "$repo" && go test -c -o "$dir/coretest.test" ./internal/coretest/)
(cd "$repo/internal/coretest" && WAKESHIFT_SDK_HARNESS="$out" && export WAKESHIFT_SDK_HARNESS && exec "$dir/coretest.test" -test.count=1 -test.run '^TestSDKHarness$' -test.timeout 15m -test.v) >"$log" 2>&1 &
pid=$!
trap 'rm -f "$out"; wait $pid 2>/dev/null || true; rm -rf "$dir"' EXIT

i=0
while [ ! -f "$out" ]; do
  if ! kill -0 "$pid" 2>/dev/null; then
    echo "the core didn't start:" >&2
    cat "$log" >&2
    exit 1
  fi
  i=$((i + 1))
  if [ "$i" -gt 1200 ]; then
    echo "the core took over two minutes to start" >&2
    kill "$pid" 2>/dev/null # or it starts later, and waits ten minutes for us to stop it
    exit 1
  fi
  sleep 0.1
done

cd "$sdk"
WAKESHIFT_SDK_HARNESS="$out" uv run pytest -m e2e -q || {
  status=$?
  echo "core log: $log" >&2
  tail -50 "$log" >&2
  exit $status
}
