#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

usage() {
  cat <<'EOF'
usage: qualification.sh --manifest PATH --baseline-tuple PATH --fixture-dir PATH \
  --old-backend IMAGE@sha256:... --old-web IMAGE@sha256:... \
  --candidate-write-file PATH --old-assert-file PATH [--dry-run]

Or, for a verified off-host preservation archive pair:
  --old-backend-archive PATH --old-backend-archive-sha256 sha256:... \
  --old-backend-metadata PATH --old-backend-metadata-sha256 sha256:... \
  --old-web-archive PATH --old-web-archive-sha256 sha256:... \
  --old-web-metadata PATH --old-web-metadata-sha256 sha256:...

Only an isolated fixture explicitly marked D1_ISOLATED_SYNTHETIC_FIXTURE=1 is
accepted. This command never reads C00 configuration, credentials, keys, or
restored production fixtures. Archive mode verifies the saved file, OCI index,
config, and RootFS layer identity before importing only a local throwaway tag.
EOF
}

manifest=""
baseline_tuple=""
fixture_dir=""
old_backend=""
old_web=""
old_backend_archive=""
old_backend_archive_sha256=""
old_backend_metadata=""
old_backend_metadata_sha256=""
old_web_archive=""
old_web_archive_sha256=""
old_web_metadata=""
old_web_metadata_sha256=""
candidate_write_file=""
old_assert_file=""
dry_run=false

while (($#)); do
  case "$1" in
    --manifest) manifest=${2:?}; shift 2 ;;
    --baseline-tuple) baseline_tuple=${2:?}; shift 2 ;;
    --fixture-dir) fixture_dir=${2:?}; shift 2 ;;
    --old-backend) old_backend=${2:?}; shift 2 ;;
    --old-web) old_web=${2:?}; shift 2 ;;
    --old-backend-archive) old_backend_archive=${2:?}; shift 2 ;;
    --old-backend-archive-sha256) old_backend_archive_sha256=${2:?}; shift 2 ;;
    --old-backend-metadata) old_backend_metadata=${2:?}; shift 2 ;;
    --old-backend-metadata-sha256) old_backend_metadata_sha256=${2:?}; shift 2 ;;
    --old-web-archive) old_web_archive=${2:?}; shift 2 ;;
    --old-web-archive-sha256) old_web_archive_sha256=${2:?}; shift 2 ;;
    --old-web-metadata) old_web_metadata=${2:?}; shift 2 ;;
    --old-web-metadata-sha256) old_web_metadata_sha256=${2:?}; shift 2 ;;
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
require --baseline-tuple "$baseline_tuple"
require --fixture-dir "$fixture_dir"
require --candidate-write-file "$candidate_write_file"
require --old-assert-file "$old_assert_file"

archive_mode=false
if [ -n "$old_backend_archive$old_web_archive" ]; then
  archive_mode=true
  if [ -n "$old_backend$old_web" ]; then
    echo "previous images must use either registry digests or a complete archive pair" >&2
    exit 2
  fi
  for name in \
    old_backend_archive old_backend_archive_sha256 \
    old_backend_metadata old_backend_metadata_sha256 \
    old_web_archive old_web_archive_sha256 \
    old_web_metadata old_web_metadata_sha256; do
    require "--${name//_/-}" "${!name}"
  done
else
  require --old-backend "$old_backend"
  require --old-web "$old_web"
fi

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

if [ "$archive_mode" = false ]; then
  for ref in "$old_backend" "$old_web"; do
    if [[ ! "$ref" =~ ^ghcr\.io/.+@sha256:[a-f0-9]{64}$ ]]; then
      echo "previous image must be an immutable ghcr.io digest: $ref" >&2
      exit 1
    fi
  done
fi

for digest in \
  "$old_backend_archive_sha256" "$old_backend_metadata_sha256" \
  "$old_web_archive_sha256" "$old_web_metadata_sha256"; do
  if [ -n "$digest" ] && [[ ! "$digest" =~ ^sha256:[a-f0-9]{64}$ ]]; then
    echo "archive identity must be a sha256 digest: $digest" >&2
    exit 1
  fi
done

validate_archive_input() {
  local archive=$1
  local expected_archive_sha256=$2
  local metadata=$3
  local expected_metadata_sha256=$4

  for path in "$archive" "$metadata"; do
    if [ ! -f "$path" ]; then
      echo "archive input does not exist: $path" >&2
      exit 1
    fi
  done
  if [ "sha256:$(sha256sum "$archive" | awk '{print $1}')" != "$expected_archive_sha256" ]; then
    echo "archive checksum does not match: $archive" >&2
    exit 1
  fi
  if [ "sha256:$(sha256sum "$metadata" | awk '{print $1}')" != "$expected_metadata_sha256" ]; then
    echo "archive metadata checksum does not match: $metadata" >&2
    exit 1
  fi
  if ! jq -e '
    (.source_ref | type == "string" and length > 0) and
    (.source_image_id | type == "string" and test("^sha256:[a-f0-9]{64}$")) and
    (.architecture == "amd64") and (.os == "linux") and
    (.rootfs_diff_ids | type == "array" and length > 0) and
    all(.rootfs_diff_ids[]; type == "string" and test("^sha256:[a-f0-9]{64}$"))
  ' "$metadata" >/dev/null; then
    echo "archive metadata lacks a valid source reference or RootFS identity: $metadata" >&2
    exit 1
  fi
}

if [ "$archive_mode" = true ]; then
  validate_archive_input "$old_backend_archive" "$old_backend_archive_sha256" \
    "$old_backend_metadata" "$old_backend_metadata_sha256"
  validate_archive_input "$old_web_archive" "$old_web_archive_sha256" \
    "$old_web_metadata" "$old_web_metadata_sha256"
fi

candidate_backend="$(node -e 'const m=require(process.argv[1]); console.log(m.images.backend)' "$manifest")"
candidate_web="$(node -e 'const m=require(process.argv[1]); console.log(m.images.web)' "$manifest")"
baseline_tuple_sha="$(node deploy/cd/tuple-snapshot.mjs digest --snapshot "$baseline_tuple")"
node deploy/cd/release-manifest.mjs verify \
  --manifest "$manifest" \
  --expected-baseline-tuple-sha256 "$baseline_tuple_sha" >/dev/null

if [ "$dry_run" = true ]; then
  printf 'isolated qualification admitted for synthetic fixture %s against tuple %s\n' \
    "$(realpath "$fixture_dir")" "$baseline_tuple_sha"
  exit 0
fi

project="che372-d1-${RANDOM}${RANDOM}"
compose=(docker compose --project-name "$project" -f deploy/cd/qualification.compose.yml)
archive_local_refs=()
archive_imported_image_ids=()
cleanup() {
  "${compose[@]}" --profile old --profile candidate down --volumes --remove-orphans >/dev/null 2>&1 || true
  for ref in "${archive_local_refs[@]}"; do
    docker image rm "$ref" >/dev/null 2>&1 || true
  done
  for image_id in "${archive_imported_image_ids[@]}"; do
    docker image rm "$image_id" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

import_archive_image() {
  local role=$1
  local archive=$2
  local metadata=$3
  local archive_sha256=$4
  local archive_tar
  local archive_source_image_id
  local archive_config_image_id
  local local_ref="che372-qualification/${role}:sha-${archive_sha256#sha256:}"
  local actual_rootfs
  local expected_rootfs
  local archive_rootfs
  local had_config_image=false

  archive_tar="$(mktemp)"
  if ! zstd -dc "$archive" >"$archive_tar"; then
    rm -f "$archive_tar"
    echo "could not decompress preserved archive: $archive" >&2
    exit 1
  fi
  archive_source_image_id="$(tar -xOf "$archive_tar" index.json | jq -r '
    .manifests | if length == 1 then .[0].digest else empty end
  ')"
  if [ "$archive_source_image_id" != "$(jq -r '.source_image_id' "$metadata")" ]; then
    rm -f "$archive_tar"
    echo "archive OCI index does not match preserved source identity: $archive" >&2
    exit 1
  fi
  archive_config_image_id="sha256:$(tar -xOf "$archive_tar" manifest.json | jq -r '
    if length == 1 then .[0].Config | split("/") | last else empty end
  ')"
  archive_rootfs="$(tar -xOf "$archive_tar" manifest.json | jq -c '
    if length == 1 then .[0].Layers | map("sha256:" + (split("/") | last)) else empty end
  ')"
  expected_rootfs="$(jq -c '.rootfs_diff_ids' "$metadata")"
  if [[ ! "$archive_config_image_id" =~ ^sha256:[a-f0-9]{64}$ ]] || [ "$archive_rootfs" != "$expected_rootfs" ]; then
    rm -f "$archive_tar"
    echo "archive config or RootFS does not match preserved identity: $archive" >&2
    exit 1
  fi
  if docker image inspect "$archive_config_image_id" >/dev/null 2>&1; then
    had_config_image=true
  fi
  if ! docker load --quiet --input "$archive_tar" >/dev/null; then
    rm -f "$archive_tar"
    echo "could not import preserved archive: $archive" >&2
    exit 1
  fi
  rm -f "$archive_tar"
  if [ "$had_config_image" = false ]; then
    archive_imported_image_ids+=("$archive_config_image_id")
  fi
  docker tag "$archive_config_image_id" "$local_ref"
  actual_rootfs="$(docker image inspect "$local_ref" --format '{{json .RootFS.Layers}}')"
  if [ "$actual_rootfs" != "$expected_rootfs" ]; then
    echo "imported image RootFS does not match preserved identity: $archive" >&2
    exit 1
  fi
  archive_local_refs+=("$local_ref")
  imported_archive_image_ref="$local_ref"
}

if [ "$archive_mode" = true ]; then
  import_archive_image backend "$old_backend_archive" "$old_backend_metadata" "$old_backend_archive_sha256"
  old_backend="$imported_archive_image_ref"
  import_archive_image web "$old_web_archive" "$old_web_metadata" "$old_web_archive_sha256"
  old_web="$imported_archive_image_ref"
fi

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
