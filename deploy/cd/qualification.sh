#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

usage() {
  cat <<'EOF'
usage: qualification.sh --manifest PATH --fixture-dir PATH \
  --old-backend IMAGE@sha256:... --old-web IMAGE@sha256:... \
  --candidate-write-file PATH --old-assert-file PATH [--dry-run]

Only an isolated fixture explicitly marked D1_ISOLATED_SYNTHETIC_FIXTURE=1 is
accepted. This command never reads C00 configuration, credentials, keys, or
restored production fixtures.
EOF
}

manifest=""
fixture_dir=""
old_backend=""
old_web=""
candidate_write_file=""
old_assert_file=""
dry_run=false

while (($#)); do
  case "$1" in
    --manifest) manifest=${2:?}; shift 2 ;;
    --fixture-dir) fixture_dir=${2:?}; shift 2 ;;
    --old-backend) old_backend=${2:?}; shift 2 ;;
    --old-web) old_web=${2:?}; shift 2 ;;
    --candidate-write-file) candidate_write_file=${2:?}; shift 2 ;;
    --old-assert-file) old_assert_file=${2:?}; shift 2 ;;
    --dry-run) dry_run=true; shift ;;
    --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

require() {
  if [ -z "$2" ]; then
    echo "missing $1" >&2
    usage >&2
    exit 2
  fi
}

require --manifest "$manifest"
require --fixture-dir "$fixture_dir"
require --old-backend "$old_backend"
require --old-web "$old_web"
require --candidate-write-file "$candidate_write_file"
require --old-assert-file "$old_assert_file"

if [ ! -f "$fixture_dir/D1_ISOLATED_SYNTHETIC_FIXTURE" ] || \
  [ "$(tr -d '[:space:]' <"$fixture_dir/D1_ISOLATED_SYNTHETIC_FIXTURE")" != "1" ]; then
  echo "fixture is not explicitly classified as isolated synthetic data" >&2
  exit 1
fi

for path in "$candidate_write_file" "$old_assert_file"; do
  if [ ! -f "$path" ]; then
    echo "qualification command file does not exist: $path" >&2
    exit 1
  fi
done

for ref in "$old_backend" "$old_web"; do
  if [[ ! "$ref" =~ ^ghcr\.io/.+@sha256:[a-f0-9]{64}$ ]]; then
    echo "previous image must be an immutable ghcr.io digest: $ref" >&2
    exit 1
  fi
done

candidate_backend="$(node -e 'const m=require(process.argv[1]); console.log(m.images.backend)' "$manifest")"
candidate_web="$(node -e 'const m=require(process.argv[1]); console.log(m.images.web)' "$manifest")"
node deploy/cd/release-manifest.mjs verify --manifest "$manifest" >/dev/null

if [ "$dry_run" = true ]; then
  printf 'isolated qualification admitted for synthetic fixture %s\n' "$(realpath "$fixture_dir")"
  exit 0
fi

project="che372-d1-${RANDOM}${RANDOM}"
compose=(docker compose --project-name "$project" -f deploy/cd/qualification.compose.yml)
cleanup() {
  "${compose[@]}" --profile old --profile candidate down --volumes --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

export D1_OLD_BACKEND_IMAGE="$old_backend"
export D1_OLD_WEB_IMAGE="$old_web"
export D1_CANDIDATE_BACKEND_IMAGE="$candidate_backend"
export D1_CANDIDATE_WEB_IMAGE="$candidate_web"

wait_ready() {
  local port=$1
  for _ in $(seq 1 60); do
    if curl --fail --silent --show-error "http://127.0.0.1:${port}/readyz" >/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "backend did not become ready on port $port" >&2
  return 1
}

"${compose[@]}" --profile old up --detach postgres old-backend old-web
wait_ready "${D1_OLD_BACKEND_PORT:-18881}"
"${compose[@]}" --profile old stop old-backend old-web
"${compose[@]}" --profile candidate up --detach candidate-backend candidate-web
wait_ready "${D1_CANDIDATE_BACKEND_PORT:-18882}"
"${compose[@]}" --profile candidate exec --no-TTY candidate-backend sh -ceu "$(<"$candidate_write_file")"
"${compose[@]}" --profile candidate stop candidate-backend candidate-web
"${compose[@]}" --profile old up --detach old-backend old-web
wait_ready "${D1_OLD_BACKEND_PORT:-18881}"
"${compose[@]}" --profile old exec --no-TTY old-backend sh -ceu "$(<"$old_assert_file")"

echo "isolated upgrade/rollback qualification passed for $project"
