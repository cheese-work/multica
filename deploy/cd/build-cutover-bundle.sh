#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: build-cutover-bundle.sh --manifest PATH --baseline-tuple PATH
  --observed-state PATH --output PATH --checksum-output PATH
EOF
}

manifest=""
baseline=""
observed=""
output=""
checksum_output=""
while (($#)); do
  case "$1" in
    --manifest) manifest=${2:?}; shift 2 ;;
    --baseline-tuple) baseline=${2:?}; shift 2 ;;
    --observed-state) observed=${2:?}; shift 2 ;;
    --output) output=${2:?}; shift 2 ;;
    --checksum-output) checksum_output=${2:?}; shift 2 ;;
    --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done
for pair in "manifest:$manifest" "baseline-tuple:$baseline" "observed-state:$observed" "output:$output" "checksum-output:$checksum_output"; do
  [ -n "${pair#*:}" ] || { echo "missing --${pair%%:*}" >&2; exit 2; }
done

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
bundle="$work_dir/cutover-controller"
mkdir -p "$bundle/deploy/cd/router" "$bundle/server" "$bundle/evidence"

controller_files=(
  cutover.sh
  deploy-lib.sh
  docker-compose.ab.yml
  migration-inventory.mjs
  quiescence.mjs
  release-packet.mjs
  router.sh
  verify-cutover-bundle.sh
)
for file in "${controller_files[@]}"; do
  cp "$root_dir/deploy/cd/$file" "$bundle/deploy/cd/$file"
done
cp "$root_dir/deploy/cd/router/nginx.conf.template" "$bundle/deploy/cd/router/nginx.conf.template"
cp -a "$root_dir/server/migrations" "$bundle/server/migrations"
cp "$manifest" "$bundle/evidence/manifest.json"
cp "$baseline" "$bundle/evidence/baseline-tuple.json"
cp "$observed" "$bundle/evidence/observed-state.json"

node "$root_dir/deploy/cd/release-packet.mjs" create \
  --manifest "$bundle/evidence/manifest.json" \
  --baseline-tuple "$bundle/evidence/baseline-tuple.json" \
  --observed-state "$bundle/evidence/observed-state.json" \
  --migrations-dir "$bundle/server/migrations" \
  --output "$bundle/evidence/release-packet.json"

(
  cd "$bundle"
  find deploy server evidence -type f ! -name controller-files.sha256 -print0 \
    | sort -z \
    | xargs -0 sha256sum > controller-files.sha256
)

tar -C "$work_dir" -cf "$output" cutover-controller
printf '%s  %s\n' "$(sha256sum "$output" | awk '{print $1}')" "$(basename "$output")" >"$checksum_output"
