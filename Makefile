.PHONY: build web db test test-go lint run gate bench-test bench-dry-run image clean

# The build's version: when it was built (UTC) and from which commit, e.g.
# 2026.11.25.1435+91faaf7. The connectors send theirs, and the server
# reports its own (internal/version).
VERSION ?= $(shell date -u +%Y.%m.%d.%H%M)+$(shell git describe --always --dirty 2>/dev/null || echo dev)
VERSION := $(VERSION)
LDFLAGS := -ldflags="-s -w -X github.com/wakeshift/server/internal/version.Version=$(VERSION)"

# Postgres from docker-compose.yml (on 127.0.0.1:5439): the server's
# database, owned by an ordinary role, and the superuser the tests make a
# database each with.
DEV_DATABASE_URL ?= postgres://wakeshift:wakeshift@localhost:5439/wakeshift?sslmode=disable
TEST_DATABASE_URL ?= postgres://postgres:postgres@localhost:5439/postgres?sslmode=disable

# `make run`: the server on this machine's loopback, with the console's
# email-only sign-in (WAKESHIFT_DEV_SIGN_IN) and the published development
# secret key, both of which the server allows only there. It prints a
# development sign-in token at startup, which the console's sign-in asks
# for (or set WAKESHIFT_DEV_SIGN_IN_TOKEN). Development only; never use
# these settings anywhere else. WAKESHIFT_APPS registers your apps (go run
# ./cmd/appkey -app <id>).
DEV_ENV = DATABASE_URL="$(DEV_DATABASE_URL)" WAKESHIFT_ADDR=127.0.0.1:8370 WAKESHIFT_PUBLIC_URL=http://localhost:8370 \
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
