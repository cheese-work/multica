#!/usr/bin/env bash
# Multica CLI installer, served by your Multica server (CHE-2641).
#
#   curl -fsSL <server>/api/daemon/install.sh | bash
#
# Installs the server's own daemon build: the same download, sha256 check and
# --version probe as `multica update`. This script holds no secret. The binary
# endpoint needs a token (a personal access token, Settings > API Tokens):
# set MULTICA_TOKEN, or type it at the prompt. It is never put in a command
# line and never printed. When the install is done, run `multica login`.
#
# Optional: MULTICA_SERVER_URL (override the server), MULTICA_INSTALL_DIR
# (default ~/.local/bin).
set -euo pipefail

DEFAULT_SERVER_URL='__MULTICA_SERVER_URL__'
SERVER_URL="${MULTICA_SERVER_URL:-$DEFAULT_SERVER_URL}"
INSTALL_DIR="${MULTICA_INSTALL_DIR:-$HOME/.local/bin}"
MAX_BYTES=268435456

fail() { printf '✗ %s\n' "$*" >&2; exit 1; }
info() { printf '==> %s\n' "$*"; }
# Phase lines and the download bar appear only when stderr is a terminal.
phase() { if [ -t 2 ]; then printf '%s\n' "$*" >&2; fi; }

for tool in curl uname mktemp sed tr awk wc; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done
if command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "sha256sum or shasum is required"
fi

SERVER_URL="${SERVER_URL%/}"
case "$SERVER_URL" in
  http://*|https://*) ;;
  *) fail "server URL '$SERVER_URL' is not an http(s) URL" ;;
esac

case "$(uname -s)" in
  Linux) OS=linux ;;
  Darwin) OS=darwin ;;
  *) fail "unsupported OS $(uname -s): the server builds Linux and macOS only" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) fail "unsupported CPU $(uname -m)" ;;
esac
KEY="$OS-$ARCH"

TOKEN="${MULTICA_TOKEN:-}"
unset MULTICA_TOKEN
if [ -z "$TOKEN" ]; then
  # stdin is the script itself under `curl | bash`; ask on the terminal.
  if ( : </dev/tty ) 2>/dev/null; then
    printf 'Multica token (input hidden): ' >/dev/tty
    IFS= read -rs TOKEN </dev/tty || true
    printf '\n' >/dev/tty
  fi
fi
[ -n "$TOKEN" ] || fail "no token: set MULTICA_TOKEN (Settings > API Tokens) and run again"
case "$TOKEN" in
  *[!A-Za-z0-9._~+/=-]*) fail "the token has characters a Multica token never contains" ;;
esac

work="$(mktemp -d)"
bg_pid=""
cleanup() {
  if [ -n "$bg_pid" ]; then kill "$bg_pid" >/dev/null 2>&1 || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

# Redraw one stderr line from the size of file $2 while process $1 runs, then
# end the line. $3 is the expected byte count, empty when unknown.
# ponytail: the rate uses whole seconds from $SECONDS, so it shows after 1 s.
render_progress() {
  local pid=$1 file=$2 total=${3:-} start=$SECONDS alive n
  while :; do
    alive=1
    kill -0 "$pid" 2>/dev/null || alive=0
    n="$({ wc -c <"$file"; } 2>/dev/null || echo 0)"
    awk -v n="$n" -v t="${total:-0}" -v s="$((SECONDS - start))" 'BEGIN {
      m = n / 1048576
      if (t > 0) {
        p = n * 100 / t; if (p > 100) p = 100
        bar = ""; for (i = 0; i < 20; i++) bar = bar (i < int(p / 5) ? "#" : "-")
        printf "\r[%s] %3d%%  %.1f / %.1f MB\033[K", bar, p, m, t / 1048576
      } else {
        printf "\r%.1f MB", m
        if (s > 0) printf "  %.1f MB/s", m / s
        printf "\033[K"
      }
    }' >&2
    [ "$alive" -eq 1 ] || break
    sleep 0.2
  done
  printf '\n' >&2
}

# GET $1 into $2 with the token. The Authorization header goes through curl's
# config on stdin: printf is a shell builtin, so it never reaches any argv.
# A third argument (the expected size, possibly empty) draws a download bar
# when stderr is a terminal; otherwise the call is exactly the silent one.
fetch() {
  local path=$1 out=$2 code
  if [ $# -ge 3 ] && [ -t 2 ]; then
    printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" |
      curl -sS --config - --max-filesize "$MAX_BYTES" -o "$out" -w '%{http_code}' "$SERVER_URL$path" >"$out.code" 2>"$out.err" &
    bg_pid=$!
    render_progress "$bg_pid" "$out" "$3"
    if wait "$bg_pid"; then code="$(cat "$out.code")"; else bg_pid=""; cat "$out.err" >&2; fail "cannot reach $SERVER_URL"; fi
    bg_pid=""
  else
    code="$(printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" |
      curl -sS --config - --max-filesize "$MAX_BYTES" -o "$out" -w '%{http_code}' "$SERVER_URL$path")" ||
      fail "cannot reach $SERVER_URL"
  fi
  case "$code" in
    200) ;;
    401|403) fail "the server rejected the token (HTTP $code)" ;;
    404) fail "the server has no daemon build for $KEY (HTTP 404)" ;;
    *) fail "the server answered HTTP $code for $path" ;;
  esac
}

info "Reading the server's build"
fetch /api/daemon/update "$work/manifest.json"
manifest="$(tr -d '\r\n' <"$work/manifest.json")"
commit="$(printf '%s' "$manifest" | sed -n 's/.*"commit"[[:space:]]*:[[:space:]]*"\([0-9a-f]\{40\}\)".*/\1/p')"
digest="$(printf '%s' "$manifest" | sed -n 's/.*"'"$KEY"'"[[:space:]]*:[[:space:]]*{[^}]*"sha256"[[:space:]]*:[[:space:]]*"\([0-9a-fA-F]\{64\}\)".*/\1/p' | tr 'A-F' 'a-f')"
[ -n "$commit" ] || fail "the server reported no build commit"
[ -n "$digest" ] || fail "the server has no $KEY build or no sha256 for it"
size="$(printf '%s' "$manifest" | sed -n 's/.*"'"$KEY"'"[[:space:]]*:[[:space:]]*{[^}]*"size"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p')"

info "Downloading $KEY build (commit ${commit:0:7})"
fetch "/api/daemon/update/binary?os=$OS&arch=$ARCH" "$work/multica" "$size"
phase "Verifying sha256"
actual="$(sha256_of "$work/multica")"
[ "$actual" = "$digest" ] || fail "sha256 mismatch for $KEY: expected $digest, got $actual. Nothing was installed."

chmod 0700 "$work/multica"
phase "Probing --version"
# The probe must exit 0 within PROBE_TIMEOUT seconds and report the manifest's
# commit; a candidate that prints the right commit but fails is not installed.
# `timeout` is not on stock macOS, so a background watchdog bounds the probe.
PROBE_TIMEOUT="${MULTICA_PROBE_TIMEOUT:-10}"
MULTICA_TOKEN='' "$work/multica" --version >"$work/probe.out" 2>/dev/null </dev/null &
probe_pid=$!
# Fork the watchdog without the EXIT trap: bash runs an inherited EXIT trap when
# a fresh subshell is killed, and that would delete $work under the installer.
trap - EXIT
{ sleep "$PROBE_TIMEOUT"; kill -9 "$probe_pid"; } >/dev/null 2>&1 &
watchdog_pid=$!
trap cleanup EXIT
probe_ok=1
wait "$probe_pid" 2>/dev/null && probe_ok=0
kill "$watchdog_pid" >/dev/null 2>&1 || true
reported=""
if [ "$probe_ok" -eq 0 ]; then
  reported="$(sed -n 's/.*(commit: \([0-9a-f]*\),.*/\1/p' "$work/probe.out" | head -n1)"
fi
if [ "$probe_ok" -ne 0 ] || [ "${#reported}" -lt 7 ] || [ "${commit#"$reported"}" = "$commit" ]; then
  fail "the downloaded binary does not run here (--version must exit 0 within ${PROBE_TIMEOUT}s) or does not report commit ${commit:0:7} (got '${reported}'). Nothing was installed."
fi

phase "Installing"
mkdir -p "$INSTALL_DIR"
staged="$INSTALL_DIR/.multica.new.$$"
cp "$work/multica" "$staged"
chmod 0755 "$staged"
mv -f "$staged" "$INSTALL_DIR/multica"
info "Installed $INSTALL_DIR/multica (commit ${commit:0:7})"
phase "Installed"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) printf 'Add %s to your PATH to run `multica`.\n' "$INSTALL_DIR" ;;
esac

cat <<NEXT

Next:
  multica config set server_url $SERVER_URL
  multica login --token
  multica daemon start
NEXT
