#!/bin/sh
# webhooks-env.sh: writes .env at the tree's root, which docker compose reads
# by itself, with what the server and Webhooks (the profile webhooks) need:
# a fresh WAKESHIFT_SECRET_KEY, a fresh key for the Webhooks app with its
# WAKESHIFT_APPS entry (cmd/appkey, from the server's image), and a fresh
# secret that signs Webhooks' links. Nothing is printed; .env is readable by
# you alone. Run it once, then:
#
#   docker compose --profile webhooks up --build
#
# (make webhooks does both.) If .env already has a WEBHOOKS_KEY, it does
# nothing.
set -eu
cd "$(dirname "$0")/.."

if [ -f .env ] && grep -q '^WEBHOOKS_KEY=' .env; then
  echo "webhooks-env: .env already has the Webhooks app's key; nothing to do"
  exit 0
fi
if [ -e .env ]; then
  echo "webhooks-env: .env exists without WEBHOOKS_KEY; move it away, or add the Webhooks lines to it yourself (docs/self-hosting.md, Webhooks)" >&2
  exit 1
fi
for v in WAKESHIFT_APPS WAKESHIFT_SECRET_KEY WEBHOOKS_KEY WEBHOOKS_SECRET; do
  if eval "[ -n \"\${$v:-}\" ]"; then
    echo "webhooks-env: $v is set in your shell, and docker compose would use it instead of .env's; unset it first" >&2
    exit 1
  fi
done

random() { head -c 48 /dev/urandom | base64 | tr -d '\n'; }

# The Webhooks app's key and its WAKESHIFT_APPS entry, as two lines
# (WAKESHIFT_KEY=…, WAKESHIFT_APPS=…), kept in a variable, never shown.
lines=$(docker compose run --build --rm --no-deps --entrypoint /appkey server -app webhooks -name Webhooks)
key=$(printf '%s\n' "$lines" | sed -n 's/^WAKESHIFT_KEY=//p')
apps=$(printf '%s\n' "$lines" | sed -n 's/^WAKESHIFT_APPS=//p')
if [ -z "$key" ] || [ -z "$apps" ]; then
  echo "webhooks-env: cmd/appkey didn't print a key" >&2
  exit 1
fi

umask 077
# A new file of our own (mktemp makes it exclusively, readable by you
# alone), renamed into place once written.
tmp=$(mktemp .env.XXXXXX)
trap 'rm -f "$tmp"' EXIT
{
  echo "# Written by deploy/webhooks-env.sh for docker compose, which reads it by"
  echo "# itself. Secrets: keep this file to yourself, and never commit it."
  echo "WAKESHIFT_SECRET_KEY='$(random)'"
  echo "WAKESHIFT_APPS='$apps'"
  echo "WEBHOOKS_KEY='$key'"
  echo "WEBHOOKS_SECRET='$(random)'"
} >"$tmp"
mv "$tmp" .env
echo "webhooks-env: wrote .env (the server's secret key, the Webhooks app's key and its link secret)"
