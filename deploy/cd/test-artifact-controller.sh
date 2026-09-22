#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

source_sha="504078f8ea7fa31f342f195659e93a7f6c3e5a91"
backend_digest="sha256:$(printf 'a%.0s' {1..64})"
web_digest="sha256:$(printf 'b%.0s' {1..64})"
mapfile -t versions < <(find server/migrations -maxdepth 1 -name '*.up.sql' -printf '%f\n' | sort | sed 's/\.up\.sql$//')
latest="${versions[${#versions[@]}-1]}"
ledger_file="$work_dir/ledger.txt"
printf '%s\n' "${versions[@]}" >"$ledger_file"
ledger_digest="sha256:$(sha256sum "$ledger_file" | awk '{print $1}')"

baseline="$work_dir/baseline-tuple.json"
cat >"$baseline" <<JSON
{"images":{"backend":{"digest":"$backend_digest"},"web":{"digest":"$web_digest"}},"migration_ledger":{"row_count":${#versions[@]},"latest":{"version":"$latest"},"ordered_sha256":"$ledger_digest"}}
JSON
baseline_digest="sha256:$(sha256sum "$baseline" | awk '{print $1}')"
inventory="$(node deploy/cd/migration-inventory.mjs)"
manifest="$work_dir/manifest.json"
cat >"$manifest" <<JSON
{"source_sha":"$source_sha","images":{"backend":"ghcr.io/cheese-work/multica-backend@$backend_digest","web":"ghcr.io/cheese-work/multica-web@$web_digest"},"migration_inventory_sha256":"$inventory","baseline_tuple_sha256":"$baseline_digest"}
JSON
observed="$work_dir/observed-state.json"
node -e '
const fs=require("node:fs"); const versions=fs.readFileSync(process.argv[1],"utf8").trim().split("\n");
fs.writeFileSync(process.argv[2],JSON.stringify({schema_version:1,ledger:{status:"complete",versions},indexes:{status:"complete",invalid:[]},hooks:{status:"complete",observations:[{name:"task_usage_hourly_rollup",status:"completed"},{name:"attribution_strict_backfill",status:"completed"},{name:"chat_explicit_origin_backfill",status:"completed"}]}}));
' "$ledger_file" "$observed"

bundle="$work_dir/cutover-controller.tar"
checksum="$work_dir/cutover-controller.tar.sha256"
bash deploy/cd/build-cutover-bundle.sh --manifest "$manifest" --baseline-tuple "$baseline" --observed-state "$observed" --output "$bundle" --checksum-output "$checksum"
mkdir "$work_dir/unpack"
tar -C "$work_dir/unpack" -xf "$bundle"
bash "$work_dir/unpack/cutover-controller/deploy/cd/verify-cutover-bundle.sh" "$work_dir/unpack/cutover-controller"

test -f "$work_dir/unpack/cutover-controller/evidence/manifest.json"
test -f "$work_dir/unpack/cutover-controller/evidence/release-packet.json"

# Layout mismatch: the consumer's exact paths are mandatory.
mv "$work_dir/unpack/cutover-controller/evidence/manifest.json" "$work_dir/unpack/cutover-controller/evidence/wrong-name.json"
if bash "$work_dir/unpack/cutover-controller/deploy/cd/verify-cutover-bundle.sh" "$work_dir/unpack/cutover-controller" >/dev/null 2>&1; then
  echo "layout mismatch was accepted" >&2
  exit 1
fi
mv "$work_dir/unpack/cutover-controller/evidence/wrong-name.json" "$work_dir/unpack/cutover-controller/evidence/manifest.json"

# Controller/migration mismatch: even a self-consistent checksum manifest is
# insufficient when the migration inventory no longer matches the manifest.
printf '\n-- drift\n' >>"$work_dir/unpack/cutover-controller/server/migrations/$(find server/migrations -maxdepth 1 -name '*.up.sql' -printf '%f\n' | sort | tail -1)"
(
  cd "$work_dir/unpack/cutover-controller"
  find deploy server evidence -type f ! -name controller-files.sha256 -print0 | sort -z | xargs -0 sha256sum >controller-files.sha256
)
if bash "$work_dir/unpack/cutover-controller/deploy/cd/verify-cutover-bundle.sh" "$work_dir/unpack/cutover-controller" >/dev/null 2>&1; then
  echo "controller/migration mismatch was accepted" >&2
  exit 1
fi

# Incomplete observation: packet creation must fail instead of inventing
# ledger/index/hook defaults.
incomplete="$work_dir/incomplete.json"
node -e 'const fs=require("fs");const x=JSON.parse(fs.readFileSync(process.argv[1]));delete x.indexes.status;fs.writeFileSync(process.argv[2],JSON.stringify(x))' "$observed" "$incomplete"
if node deploy/cd/release-packet.mjs create --manifest "$manifest" --baseline-tuple "$baseline" --observed-state "$incomplete" --output "$work_dir/should-not-exist.json" >/dev/null 2>&1; then
  echo "incomplete observed state was accepted" >&2
  exit 1
fi

echo "artifact-to-controller replay passed"
