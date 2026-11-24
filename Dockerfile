# The Wakeshift server and its console, in one image:
#
#   docker build -t wakeshift-server .
#
# Base images are pinned by digest (the tag stays for reading). npm ci runs
# with --ignore-scripts, so no dependency runs code at install.

# The console (web/), embedded into the server.
FROM --platform=$BUILDPLATFORM node:26@sha256:6d2c7d5d074bf72f81ddc252b4fbb5d625bc028eeda49cb4b8c2ff8eda1ee617 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts --no-fund --no-audit
COPY web/ ./
RUN npm run build

# The server (cmd/wakeshift), cross-compiled for the image's platform on
# the builder's own (no emulation). VERSION is the build's version, which the
# server reports (internal/version).
FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:08bcb99a38f85a265f9a93d1d3323e2440eec333380f3ea26ee0dec9264bb41c AS build
ARG VERSION=dev
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X github.com/wakeshift/server/internal/version.Version=${VERSION}" -o /out/ ./cmd/wakeshift ./cmd/appkey

# Webhooks (apps/try), on the TypeScript SDK (sdk/typescript) from this
# tree: docker-compose.yml's profile webhooks builds it (target webhooks).
# `docker build .` builds the server alone, the last stage.
FROM node:26@sha256:6d2c7d5d074bf72f81ddc252b4fbb5d625bc028eeda49cb4b8c2ff8eda1ee617 AS webhooks-build
WORKDIR /src/sdk/typescript
COPY sdk/typescript/package.json sdk/typescript/package-lock.json ./
RUN npm ci --ignore-scripts --no-fund --no-audit
COPY sdk/typescript/ ./
RUN npm run build
WORKDIR /src/apps/try
COPY apps/try/package.json apps/try/package-lock.json apps/try/.npmrc ./
RUN npm ci --ignore-scripts --no-fund --no-audit
COPY apps/try/ ./
RUN npm run build && npm prune --omit=dev --no-fund --no-audit && rm -rf dist/test

FROM gcr.io/distroless/nodejs26-debian13:nonroot@sha256:1d736cf8b5716c07a245301f92717143215d248fd55771ee1936ef4b3302e531 AS webhooks
WORKDIR /app
COPY --from=webhooks-build /src/apps/try/package.json ./
COPY --from=webhooks-build /src/apps/try/node_modules ./node_modules
COPY --from=webhooks-build /src/apps/try/dist/src ./dist/src
COPY --from=webhooks-build /src/apps/try/migrations ./migrations
COPY --from=webhooks-build /src/apps/try/static ./static
EXPOSE 8380
ENV NODE_ENV=production PORT=8380
CMD ["dist/src/main.js"]

# The server, and cmd/appkey for making an app's key without Go:
#   docker compose run --build --rm --no-deps --entrypoint /appkey server -app example
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/wakeshift /server
COPY --from=build /out/appkey /appkey
EXPOSE 8370
ENV WAKESHIFT_ADDR=:8370
ENTRYPOINT ["/server"]
