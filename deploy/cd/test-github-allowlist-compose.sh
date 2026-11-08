#!/usr/bin/env bash
set -euo pipefail

# Render test: GITHUB_ALLOWED_ACCOUNT_IDS must reach every backend container,
# in the base self-host file and in the A/B overlay. Unset means policy off
# (empty value), and the render must still succeed.
# Uses the real `docker compose config` (no daemon, no containers) with a
# dummy JWT secret only.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

fail() { echo "test-github-allowlist-compose: $*" >&2; exit 1; }

ids="210346002,283571768,218119340"

# render <files-label> <value|unset> -> prints "service=value" per backend service
render() {
  local label=$1 value=$2; shift 2
  local -a files=(-f docker-compose.selfhost.yml)
  local services=(backend)
  if [ "$label" = ab ]; then
    files+=(-f deploy/cd/docker-compose.ab.yml)
    services=(backend backend-blue backend-green)
  fi
  local -a vars=(JWT_SECRET=synthetic-jwt-secret)
  [ "$value" = unset ] || vars+=(GITHUB_ALLOWED_ACCOUNT_IDS="$value")
  local json
  json="$(env -i HOME="$HOME" PATH="$PATH" "${vars[@]}" docker compose "${files[@]}" config --format json)" \
    || fail "render failed ($label, $value)"
  local s got
  for s in "${services[@]}"; do
    got="$(printf '%s' "$json" | node -e '
      const m = JSON.parse(require("node:fs").readFileSync(0, "utf8"));
      const e = (m.services[process.argv[1]] || {}).environment || {};
      process.stdout.write("GITHUB_ALLOWED_ACCOUNT_IDS" in e ? "[" + e.GITHUB_ALLOWED_ACCOUNT_IDS + "]" : "MISSING");
    ' "$s")" || fail "parse failed ($label, $value, $s)"
    printf '%s=%s\n' "$s" "$got"
  done
}

check() { # check <label> <value|unset> <expected-env-value>
  local label=$1 value=$2 want=$3 out line
  # Capture first: a failed render must fail the test, not vanish in a pipe.
  out="$(render "$label" "$value")" || exit 1
  [ -n "$out" ] || fail "$label ${value}: no services rendered"
  while IFS= read -r line; do
    [ "${line#*=}" = "[$want]" ] || fail "$label ${value}: ${line%%=*} got ${line#*=}, want [$want]"
  done <<<"$out"
}

for label in base ab; do
  check "$label" "$ids" "$ids"
  check "$label" unset ""
done

echo "test-github-allowlist-compose: ok"
