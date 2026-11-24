.PHONY: build web db test test-go lint run gate gate-webhooks webhooks try-test bench-test bench-dry-run image clean

# The build's version: when it was built (UTC) and from which commit, e.g.
# 2026.11.20.1435+91faaf7. The connectors send theirs, and the server
# reports its own (internal/version).
VERSION ?= $(shell date -u +%Y.%m.%d.%H%M)+$(shell git describe --always --dirty 2>/dev/null || echo dev)
VERSION := $(VERSION)
LDFLAGS := -ldflags="-s -w -X github.com/wakeshift/server/internal/version.Version=$(VERSION)"

# Postgres from docker-compose.yml (on 127.0.0.1, port WAKESHIFT_DB_PORT,
# default 5439): the server's database, with its two ordinary roles
# (deploy/postgres-init.sql), the owner that migrates it and the server's,
# which only reads and writes, and the superuser the tests make a database
# each with. If port 5439 is taken, pick another, as in
# `WAKESHIFT_DB_PORT=5440 make run`; docker compose reads it too.
WAKESHIFT_DB_PORT ?= 5439
export WAKESHIFT_DB_PORT
DEV_DATABASE_URL ?= postgres://wakeshift_server:wakeshift_server@localhost:$(WAKESHIFT_DB_PORT)/wakeshift?sslmode=disable
DEV_MIGRATE_DATABASE_URL ?= postgres://wakeshift:wakeshift@localhost:$(WAKESHIFT_DB_PORT)/wakeshift?sslmode=disable
TEST_DATABASE_URL ?= postgres://postgres:postgres@localhost:$(WAKESHIFT_DB_PORT)/postgres?sslmode=disable

# `make run`: the server on this machine's loopback, with the console's
# email-only sign-in (WAKESHIFT_DEV_SIGN_IN) and the published development
# secret key, both of which the server allows only there. It prints a
# development sign-in token at startup, which the console's sign-in asks
# for (or set WAKESHIFT_DEV_SIGN_IN_TOKEN). Development only; never use
# these settings anywhere else. WAKESHIFT_APPS registers your apps (go run
# ./cmd/appkey -app <id>).
DEV_ENV = DATABASE_URL="$(DEV_DATABASE_URL)" WAKESHIFT_MIGRATE_DATABASE_URL="$(DEV_MIGRATE_DATABASE_URL)" WAKESHIFT_ADDR=127.0.0.1:8370 WAKESHIFT_PUBLIC_URL=http://localhost:8370 \
	WAKESHIFT_SECRET_KEY=dev-secret-key-at-least-32-bytes-long WAKESHIFT_DEV_SIGN_IN=1 WAKESHIFT_MCP_EVENTS_PUSH=1 \
	WAKESHIFT_LOG_FORMAT=text WAKESHIFT_APPS='$(WAKESHIFT_APPS)'

# The console (web/), then the server with it embedded, in bin/wakeshift.
build: web
	go build $(LDFLAGS) -o bin/wakeshift ./cmd/wakeshift

web:
	cd web && npm ci --ignore-scripts --no-fund --no-audit && npm run build

# Postgres only (docker compose), waiting until it accepts connections.
db:
	docker compose up -d --wait postgres

# Every Go test, against the Postgres `make db` starts, and the console's
# type check.
test: db test-go
	cd web && npm ci --ignore-scripts --no-fund --no-audit && npm run lint

test-go:
	WAKESHIFT_TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -race ./...

lint:
	test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...
	cd web && npm run lint

# The server on http://localhost:8370, with the console, on Postgres from
# `make db`. It migrates the database at startup.
run: build db
	$(DEV_ENV) ./bin/wakeshift

# The release gate (internal/releasegate): docker compose up, then an
# event and a signed webhook delivered on push to an MCP Events client.
gate:
	go run ./internal/releasegate

# The same with Webhooks (apps/try, the profile webhooks) too: a link from
# its page, an MCP client that calls watch as Claude Code does, a curl-style
# POST to the webhook URL, and the event arriving on the client's wait.
gate-webhooks:
	go run ./internal/releasegate -webhooks

# Webhooks with the server, at http://localhost:8380: .env's keys and
# secrets once (deploy/webhooks-env.sh), then docker compose with the
# profile webhooks.
webhooks:
	sh deploy/webhooks-env.sh
	docker compose --profile webhooks up --build

# Webhooks' own tests (apps/try, Node 26 or newer): type check, unit tests,
# then end to end against a server built from this tree.
try-test: db
	cd sdk/typescript && npm ci --ignore-scripts --no-fund --no-audit && npm run build
	cd apps/try && npm ci --ignore-scripts --no-fund --no-audit && npm run lint && WAKESHIFT_TEST_DATABASE_URL="$(TEST_DATABASE_URL)" npm test && WAKESHIFT_TEST_DATABASE_URL="$(TEST_DATABASE_URL)" sh scripts/e2e.sh

# The harness runner (bench/, Node 26 or newer; bench/README.md): its unit
# tests, and a dry run on its fake harness, which needs no agent, account
# or key. Exit code 3 is its check that a run would touch your real agent
# configs.
bench-test:
	cd sdk/typescript && npm ci --ignore-scripts --no-fund --no-audit && npm run build
	cd bench && npm ci --ignore-scripts --no-fund --no-audit && npm test

bench-dry-run:
	bench/run --harness fake --target fake --scenarios S1-S9,S6n,S18 --out-root $${TMPDIR:-/tmp}/wakebench-dry --expect-pass

# The container image, as docker-compose.yml builds it.
image:
	docker build --build-arg VERSION=$(VERSION) -t wakeshift-server .

clean:
	rm -rf bin
