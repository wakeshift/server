#!/bin/sh
# Load-tests the try app: starts a core with Go's TestSDKHarness (as e2e.sh
# does), then test/load.ts starts two try instances on one new database and
# runs links posting, live views watching and MCP sessions against them.
# Needs Postgres (make db), Go, and npm install in sdk/typescript and here.
# Sizes: LOAD_LINKS, LOAD_WATCHERS, LOAD_POSTS, LOAD_PARALLEL, LOAD_SESSIONS,
# LOAD_BURST (see test/load.ts).
set -eu
app=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$app/../.." && pwd)
dir=$(mktemp -d)
out="$dir/harness.json"
log="$dir/harness.log"
: "${WAKESHIFT_TEST_DATABASE_URL:=postgres://postgres:postgres@localhost:5439/postgres?sslmode=disable}"
export WAKESHIFT_TEST_DATABASE_URL

(cd "$repo" && WAKESHIFT_SDK_HARNESS="$out" go test -count=1 -run '^TestSDKHarness$' -timeout 30m -v ./internal/coretest/) >"$log" 2>&1 &
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
    exit 1
  fi
  sleep 0.1
done

cd "$app"
npx tsc -p tsconfig.json
WAKESHIFT_SDK_HARNESS="$out" node dist/test/load.js
