#!/bin/sh
# Puts a harness release in the bench's state, next to the user's own install
# (never replacing it), and prints the path of its binary:
#
#   bench/releases/install-harness.sh claude-code 2.1.317
#   → ~/.wakeshift-bench/claude-code/2.1.317/claude   (WB_CLAUDE_BIN)
#   bench/releases/install-harness.sh codex 0.167.0
#   → ~/.wakeshift-bench/codex/0.167.0/bin/codex       (WB_CODEX_BIN)
#   bench/releases/install-harness.sh cursor 2026.10.22-27dcf5f
#   → ~/.wakeshift-bench/cursor/<version>/cursor-agent (its directory first on PATH)
#   bench/releases/install-harness.sh opencode 1.19.5
#   → ~/.wakeshift-bench/opencode/<version>/bin/opencode (its directory first on PATH)
#     (1.x is npm's opencode-darwin-arm64; 2.x, @opencode/cli-darwin-arm64: WB_OPENCODE_BIN)
#   bench/releases/install-harness.sh pi 1.5.0
#   → ~/.wakeshift-bench/pi/<version>/node_modules/.bin/pi (WB_PI_BIN)
#   bench/releases/install-harness.sh claude-agent-sdk 0.3.323
#   → ~/.wakeshift-bench/claude-agent-sdk/<version>/node_modules/@anthropic-ai/claude-agent-sdk-darwin-arm64/claude
#     (the SDK and the Claude Code it bundles: the claude-code-sdk lane)
#
# macOS on Apple silicon (the bench's machines). Downloads are checked where
# the vendor publishes a check: npm's integrity (npm pack), Codex's
# SHA256SUMS. Cursor publishes none; its download is the one its own
# installer makes, over HTTPS. Already there: prints the path and does nothing.
set -eu

harness=${1:?harness (claude-code, codex, cursor, opencode, pi, claude-agent-sdk)}
version=${2:?version}
case "$version" in
  *[!0-9A-Za-z.+-]* | "" | [!0-9A-Za-z]*) echo "not a version: $version" >&2; exit 2 ;;
esac
[ "$(uname -s)/$(uname -m)" = "Darwin/arm64" ] || { echo "install-harness.sh runs on macOS arm64 (the bench's machines)" >&2; exit 2; }

state=${WB_STATE_DIR:-$HOME/.wakeshift-bench}
dir=$state/$harness/$version
case "$harness" in
  claude-code) bin=$dir/claude ;;
  codex) bin=$dir/bin/codex ;;
  cursor) bin=$dir/cursor-agent ;;
  opencode) bin=$dir/bin/opencode ;;
  pi) bin=$dir/node_modules/.bin/pi ;;
  claude-agent-sdk) bin=$dir/node_modules/@anthropic-ai/claude-agent-sdk-darwin-arm64/claude ;;
  *) echo "no installer for $harness (claude-code, codex, cursor, opencode, pi, claude-agent-sdk)" >&2; exit 2 ;;
esac
if [ -x "$bin" ]; then
  echo "$bin"
  exit 0
fi

mkdir -p "$state/$harness"
chmod 700 "$state"
tmp=$(mktemp -d "$state/$harness/.tmp-$version-XXXXXX")
trap 'rm -rf "$tmp"' EXIT

case "$harness" in
  claude-code)
    (cd "$tmp" && npm pack --silent "@anthropic-ai/claude-code-darwin-arm64@$version" >/dev/null)
    tar -xzf "$tmp"/*.tgz -C "$tmp"
    mkdir "$tmp/out"
    mv "$tmp/package/claude" "$tmp/out/claude"
    ;;
  codex)
    base="https://github.com/openai/codex/releases/download/rust-v$version"
    asset=codex-package-aarch64-apple-darwin.tar.gz
    curl -fsSL "$base/$asset" -o "$tmp/$asset"
    curl -fsSL "$base/codex-package_SHA256SUMS" -o "$tmp/SHA256SUMS"
    want=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/SHA256SUMS")
    got=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
    [ -n "$want" ] && [ "$want" = "$got" ] || { echo "codex $version: $asset doesn't match its SHA256SUMS" >&2; exit 1; }
    mkdir "$tmp/out"
    tar -xzf "$tmp/$asset" -C "$tmp/out"
    ;;
  cursor)
    mkdir "$tmp/out"
    curl -fsSL "https://downloads.cursor.com/lab/$version/darwin/arm64/agent-cli-package.tar.gz" | tar --strip-components=1 -xzf - -C "$tmp/out"
    ;;
  opencode)
    # OpenCode 1 is npm's opencode-darwin-arm64; OpenCode 2 is
    # @opencode/cli-darwin-arm64. Both packages hold bin/opencode.
    case "$version" in
      0.* | 1.*) pkg=opencode-darwin-arm64 ;;
      *) pkg=@opencode/cli-darwin-arm64 ;;
    esac
    (cd "$tmp" && npm pack --silent "$pkg@$version" >/dev/null)
    tar -xzf "$tmp"/*.tgz -C "$tmp"
    mkdir -p "$tmp/out/bin"
    mv "$tmp/package/bin/opencode" "$tmp/out/bin/opencode"
    ;;
  pi)
    # A Node program with dependencies: npm installs it (checking each
    # package's integrity), scripts off, into a directory of its own.
    mkdir "$tmp/out"
    npm install --silent --prefix "$tmp/out" --ignore-scripts --no-audit --no-fund --no-save "@earendil-works/pi-coding-agent@$version" >/dev/null
    ;;
  claude-agent-sdk)
    # The SDK and its platform package (the Claude Code it bundles), at the
    # same version, scripts off.
    mkdir "$tmp/out"
    npm install --silent --prefix "$tmp/out" --ignore-scripts --no-audit --no-fund --no-save \
      "@anthropic-ai/claude-agent-sdk@$version" "@anthropic-ai/claude-agent-sdk-darwin-arm64@$version" >/dev/null
    ;;
esac

rel=${bin#"$dir"/}
[ -x "$tmp/out/$rel" ] || { echo "$harness $version: no $rel in the package" >&2; exit 1; }
# Gatekeeper: a download keeps its quarantine flag (curl doesn't set one, but be sure).
xattr -dr com.apple.quarantine "$tmp/out" 2>/dev/null || true
rm -rf "$dir"
mv "$tmp/out" "$dir"
echo "$bin"
