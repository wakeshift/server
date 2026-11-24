#!/bin/sh
# The bench's own OpenClaw and Hermes state (bench/README.md, "Container
# harnesses"): made by each harness's own non-interactive onboarding, in
# $WB_STATE_DIR (default ~/.wakeshift-bench), never from the user's own
# state. No conversations, channels, MCP servers, memory or personal skills,
# and no secrets: the model key and OpenClaw's gateway token are references
# to environment variables the bench sets per run (the key from
# $WB_STATE_DIR/env, which the user writes).
#
#   bench/state.sh              make what's missing
#   bench/state.sh --refresh    remake both (each run copies the state anyway)
set -eu
root=${WB_STATE_DIR:-$HOME/.wakeshift-bench}
model=${WB_MODEL:-gpt-6-luna}
refresh=0
[ "${1:-}" = "--refresh" ] && refresh=1
umask 077
mkdir -p "$root"
chmod 700 "$root"

oc_image=ghcr.io/openclaw/openclaw:latest
# The release the bench runs (bench/drivers/hermes: HERMES_IMAGE; keep them the same).
hermes_image=nousresearch/hermes-agent:v0.22.5

# fresh <dir>: true when <dir> should be (re)made, which happens in
# <dir>.new and replaces <dir> only once it's complete (done_state), so a failed
# remake keeps the working state. A dir the bench didn't make (no
# .wakebench marker) is never removed.
build=""
trap '[ -n "$build" ] && rm -rf "$build"' EXIT
fresh() {
  if [ -e "$1" ] && [ ! -f "$1/.wakebench" ]; then
    echo "$1 exists but wasn't made by bench/state.sh; move it away first" >&2
    exit 1
  fi
  if [ -f "$1/.wakebench" ] && [ "$refresh" = 0 ]; then
    echo "$1: already made ($(cat "$1/.wakebench")); --refresh to remake"
    return 1
  fi
  build=$1.new
  rm -rf "$build"
  mkdir -p "$build"
}

# done_state <dir> <marker text>: marks the build complete and puts it in place.
done_state() {
  echo "$2" >"$build/.wakebench"
  rm -rf "$1"
  mv "$build" "$1"
  build=""
  echo "made $1"
}

# Placeholders: onboarding checks the env vars it references exist, and
# stores only the references.
placeholders="-e WS_TOKEN=wakebench-placeholder -e OPENAI_API_KEY=wakebench-placeholder"

oc_env="-e HOME=/home/node -e OPENCLAW_HOME=/home/node -e OPENCLAW_STATE_DIR=/home/node/.openclaw -e OPENCLAW_CONFIG_PATH=/home/node/.openclaw/openclaw.json -e OPENCLAW_WORKSPACE_DIR=/home/node/.openclaw/workspace -e OPENCLAW_DISABLE_BONJOUR=1"

dir=$root/openclaw
if fresh "$dir"; then
  # shellcheck disable=SC2086 # word splitting of the -e lists is intended
  docker run --rm $oc_env $placeholders -v "$build:/home/node/.openclaw" --entrypoint node "$oc_image" dist/index.js \
    onboard --non-interactive --accept-risk --mode local --flow quickstart \
    --auth-choice openai-api-key --secret-input-mode ref \
    --gateway-auth token --gateway-token-ref-env WS_TOKEN --gateway-bind lan --gateway-port 18789 \
    --skip-bootstrap --skip-channels --skip-skills --skip-search --skip-hooks --skip-daemon --skip-health --skip-ui \
    --tailscale off --json >/dev/null
  # shellcheck disable=SC2086
  echo "{agents: {defaults: {model: {primary: 'openai/$model'}, models: {'openai/$model': {}}}}}" |
    docker run --rm -i $oc_env $placeholders -v "$build:/home/node/.openclaw" --entrypoint node "$oc_image" dist/index.js \
      config patch --stdin --replace-path agents.defaults.models >/dev/null
  version=$(docker run --rm --entrypoint node "$oc_image" dist/index.js --version 2>/dev/null | grep -o '[0-9][0-9.]*' | head -1)
  done_state "$dir" "openclaw $version, openai/$model, $(date -u +%Y-%m-%dT%H:%MZ)"
fi

dir=$root/hermes
if fresh "$dir"; then
  # shellcheck disable=SC2086
  docker run --rm $placeholders -v "$build:/opt/data" "$hermes_image" setup --non-interactive >/dev/null 2>&1
  # telemetry.shared_metrics: "No thanks", recorded with the offer version
  # Hermes asks for (2); without it every new chat opens on the question.
  for kv in "model.provider openai" "model.base_url https://api.openai.com/v1" "model.default $model" \
    "telemetry.shared_metrics.enabled false" "telemetry.shared_metrics.send false" "telemetry.shared_metrics.offer_version 2"; do
    # shellcheck disable=SC2086
    docker run --rm -v "$build:/opt/data" "$hermes_image" config set $kv >/dev/null 2>&1
  done
  version=$(docker run --rm --entrypoint /opt/hermes/bin/hermes "$hermes_image" --version 2>/dev/null | grep -o -m1 'v[0-9][0-9.]*' || echo "?")
  done_state "$dir" "hermes $version, openai/$model, $(date -u +%Y-%m-%dT%H:%MZ)"
fi

if ! grep -q '^OPENAI_API_KEY=.' "$root/env" 2>/dev/null; then
  cat >&2 <<EOF

The bench needs a model key: put OPENAI_API_KEY=<key> in $root/env (mode 0600), e.g.
  umask 077; printf 'OPENAI_API_KEY=%s\n' 'sk-…' > $root/env
EOF
fi
