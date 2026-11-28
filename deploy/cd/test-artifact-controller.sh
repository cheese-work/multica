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
test -f "$work_dir/unpack/cutover-controller/deploy/cd/docker-compose.flags.yml"
test -f "$work_dir/unpack/cutover-controller/deploy/cd/docker-compose.ab.yml"
test -f "$work_dir/unpack/cutover-controller/deploy/cd/docker-compose.jev.yml"
test -f "$work_dir/unpack/cutover-controller/deploy/cd/drift-receipt.mjs"
test -f "$work_dir/unpack/cutover-controller/deploy/cd/contract-lib.sh"
# The released base compose file travels in the checksummed bundle, byte for byte.
cmp docker-compose.selfhost.yml "$work_dir/unpack/cutover-controller/docker-compose.selfhost.yml"

# A bundle without the base compose file is refused (exact-path contract).
mv "$work_dir/unpack/cutover-controller/docker-compose.selfhost.yml" "$work_dir/unpack/cutover-controller/base.moved"
if bash "$work_dir/unpack/cutover-controller/deploy/cd/verify-cutover-bundle.sh" "$work_dir/unpack/cutover-controller" >/dev/null 2>&1; then
  echo "bundle without the base compose file was accepted" >&2
  exit 1
fi
mv "$work_dir/unpack/cutover-controller/base.moved" "$work_dir/unpack/cutover-controller/docker-compose.selfhost.yml"

# CHE-7486 N5b-1: cutover.sh sources contract-lib.sh, so a bundle without it is refused.
mv "$work_dir/unpack/cutover-controller/deploy/cd/contract-lib.sh" "$work_dir/unpack/cutover-controller/lib.moved"
if bash "$work_dir/unpack/cutover-controller/deploy/cd/verify-cutover-bundle.sh" "$work_dir/unpack/cutover-controller" >/dev/null 2>&1; then
  echo "bundle without contract-lib.sh was accepted" >&2
  exit 1
fi
mv "$work_dir/unpack/cutover-controller/lib.moved" "$work_dir/unpack/cutover-controller/deploy/cd/contract-lib.sh"

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
  find docker-compose.selfhost.yml deploy server evidence -type f ! -name controller-files.sha256 -print0 | sort -z | xargs -0 sha256sum >controller-files.sha256
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

# make_admission LEDGER_FILE PREFIX: baseline tuple and manifest that admit that ledger.
make_admission() {
  local ledger=$1 prefix=$2 count last digest base_digest
  count="$(wc -l <"$ledger" | tr -d ' ')"
  last="$(tail -n 1 "$ledger")"
  digest="sha256:$(sha256sum "$ledger" | awk '{print $1}')"
  cat >"$prefix-baseline.json" <<JSON
{"images":{"backend":{"digest":"$backend_digest"},"web":{"digest":"$web_digest"}},"migration_ledger":{"row_count":$count,"latest":{"version":"$last"},"ordered_sha256":"$digest"}}
JSON
  base_digest="sha256:$(sha256sum "$prefix-baseline.json" | awk '{print $1}')"
  cat >"$prefix-manifest.json" <<JSON
{"source_sha":"$source_sha","images":{"backend":"registry.example/backend@$backend_digest","web":"registry.example/web@$web_digest"},"migration_inventory_sha256":"$inventory","baseline_tuple_sha256":"$base_digest"}
JSON
}

# CHE-7486 N5a: with every version in the ledger the packet lists no contract
# migration. A marked migration that is not in the ledger: with its restore
# file in the tree the bundle carries the file and verifies from its own tree;
# without it the bundle still builds and the log names the file (B3c-1 brings
# the requirement back).
node -e 'const p=JSON.parse(require("fs").readFileSync(process.argv[1],"utf8"));if(JSON.stringify(p.contract_migrations)!=="[]"){console.error("want contract_migrations [] for an all-applied ledger");process.exit(1)}' \
  "$work_dir/unpack/cutover-controller/evidence/release-packet.json"
marked_version="$(grep -l '^-- multica:contract ' server/migrations/*.up.sql | head -1 | xargs -r basename | sed 's/\.up\.sql$//')"
if [ -n "$marked_version" ]; then
  restore_path="$(head -n 1 "server/migrations/$marked_version.up.sql" | sed -E 's/.* restore=(\S+)$/\1/')"
  open_ledger="$work_dir/open-ledger.txt"
  grep -vx "$marked_version" "$ledger_file" >"$open_ledger"
  open_observed="$work_dir/open-observed.json"
  node -e '
  const fs=require("node:fs"); const x=JSON.parse(fs.readFileSync(process.argv[1],"utf8"));
  x.ledger.versions=fs.readFileSync(process.argv[2],"utf8").trim().split("\n");
  fs.writeFileSync(process.argv[3],JSON.stringify(x));' "$observed" "$open_ledger" "$open_observed"
  make_admission "$open_ledger" "$work_dir/open"
  # Restore file absent from the tree: the bundle still builds, the log names
  # the file, and the bundle does not carry it (the requirement returns in B3c-1).
  scratch_absent="$work_dir/scratch-absent"
  mkdir -p "$scratch_absent/server"
  cp -r deploy "$scratch_absent/deploy"
  cp docker-compose.selfhost.yml "$scratch_absent/"
  cp -r server/migrations "$scratch_absent/server/migrations"
  rm -f "$scratch_absent/$restore_path"
  absent_log="$work_dir/absent-build.log"
  if ! bash "$scratch_absent/deploy/cd/build-cutover-bundle.sh" --manifest "$work_dir/open-manifest.json" --baseline-tuple "$work_dir/open-baseline.json" --observed-state "$open_observed" --output "$work_dir/no-restore.tar" --checksum-output "$work_dir/no-restore.sha256" >/dev/null 2>"$absent_log"; then
    echo "a marked migration without its restore file did not bundle" >&2
    cat "$absent_log" >&2
    exit 1
  fi
  if ! grep -q "restore file not in tree, bundle built without it: $restore_path" "$absent_log"; then
    echo "bundle build did not log the missing restore file $restore_path" >&2
    cat "$absent_log" >&2
    exit 1
  fi
  mkdir "$work_dir/unpack-absent"
  tar -C "$work_dir/unpack-absent" -xf "$work_dir/no-restore.tar"
  if [ -e "$work_dir/unpack-absent/cutover-controller/$restore_path" ]; then
    echo "bundle carries a restore file that is not in the tree" >&2
    exit 1
  fi
  # State of the real tree: every declared restore file that is not in it yet.
  # Informational only: an absent file no longer blocks the bundle (B3c-1 restores the check).
  for marked in $(grep -l '^-- multica:contract ' server/migrations/*.up.sql | xargs -n1 basename | sed 's/\.up\.sql$//'); do
    declared="$(head -n 1 "server/migrations/$marked.up.sql" | sed -E 's/.* restore=(\S+)$/\1/')"
    if [ -f "$declared" ]; then echo "restore file present in tree: $declared"; else echo "restore file not in this tree yet: $declared ($marked)"; fi
  done
  scratch="$work_dir/scratch-repo"
  mkdir -p "$scratch/server"
  cp -r deploy "$scratch/deploy"
  cp docker-compose.selfhost.yml "$scratch/"
  cp -r server/migrations "$scratch/server/migrations"
  mkdir -p "$scratch/$(dirname "$restore_path")"
  printf -- '-- restore stub\n' >"$scratch/$restore_path"
  # The real tree has more than one marked migration; put them all in the ledger but this one.
  for other in $(grep -l '^-- multica:contract ' server/migrations/*.up.sql | xargs -n1 basename | sed 's/\.up\.sql$//' | grep -vx "$marked_version"); do
    other_restore="$(head -n 1 "server/migrations/$other.up.sql" | sed -E 's/.* restore=(\S+)$/\1/')"
    mkdir -p "$scratch/$(dirname "$other_restore")"
    printf -- '-- restore stub\n' >"$scratch/$other_restore"
    grep -vx "$other" "$open_ledger" >"$open_ledger.next" || true
    mv "$open_ledger.next" "$open_ledger"
    node -e '
    const fs=require("node:fs"); const x=JSON.parse(fs.readFileSync(process.argv[1],"utf8"));
    x.ledger.versions=fs.readFileSync(process.argv[2],"utf8").trim().split("\n");
    fs.writeFileSync(process.argv[1],JSON.stringify(x));' "$open_observed" "$open_ledger"
  done
  make_admission "$open_ledger" "$work_dir/open"
  bash "$scratch/deploy/cd/build-cutover-bundle.sh" --manifest "$work_dir/open-manifest.json" --baseline-tuple "$work_dir/open-baseline.json" --observed-state "$open_observed" --output "$work_dir/with-restore.tar" --checksum-output "$work_dir/with-restore.sha256"
  mkdir "$work_dir/unpack-restore"
  tar -C "$work_dir/unpack-restore" -xf "$work_dir/with-restore.tar"
  test -f "$work_dir/unpack-restore/cutover-controller/$restore_path"
  test -f "$work_dir/unpack-restore/cutover-controller/deploy/cd/contract-snapshot.mjs"
  bash "$work_dir/unpack-restore/cutover-controller/deploy/cd/verify-cutover-bundle.sh" "$work_dir/unpack-restore/cutover-controller"
  rm "$work_dir/unpack-restore/cutover-controller/$restore_path"
  if bash "$work_dir/unpack-restore/cutover-controller/deploy/cd/verify-cutover-bundle.sh" "$work_dir/unpack-restore/cutover-controller" >/dev/null 2>&1; then
    echo "a bundle without its restore file verified" >&2
    exit 1
  fi
fi

echo "artifact-to-controller replay passed"
