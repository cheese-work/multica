#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

source_sha="504078f8ea7fa31f342f195659e93a7f6c3e5a91"
config_sha="sha256:1111111111111111111111111111111111111111111111111111111111111111"
migration_sha="sha256:2222222222222222222222222222222222222222222222222222222222222222"
backend_digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
web_digest="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
manifest="$tmp_dir/manifest.json"
event="$tmp_dir/event.json"
checks="$tmp_dir/checks.json"
provenance="$tmp_dir/provenance.json"

node deploy/cd/release-manifest.mjs create \
  --kind release-candidate \
  --repository cheese-work/multica \
  --source-sha "$source_sha" \
  --architecture linux/amd64 \
  --configuration-sha256 "$config_sha" \
  --migration-inventory-sha256 "$migration_sha" \
  --backend-image "ghcr.io/cheese-work/multica-backend@$backend_digest" \
  --web-image "ghcr.io/cheese-work/multica-web@$web_digest" \
  --output "$manifest"

cat >"$event" <<EOF
{"event_name":"push","ref":"refs/heads/main","repository":"cheese-work/multica","sha":"$source_sha"}
EOF
cat >"$checks" <<EOF
{"sha":"$source_sha","contexts":{"backend":"success","frontend":"success","mobile":"success","cd-qualification":"success"}}
EOF
cat >"$provenance" <<EOF
{"images":{"backend":{"digest":"$backend_digest","architecture":"linux/amd64","repository":"cheese-work/multica","source_sha":"$source_sha"},"web":{"digest":"$web_digest","architecture":"linux/amd64","repository":"cheese-work/multica","source_sha":"$source_sha"}}}
EOF

admit() {
  node deploy/cd/admission.mjs verify \
    --manifest "$manifest" \
    --event "$event" \
    --checks "$checks" \
    --provenance "$provenance" \
    --repository cheese-work/multica \
    --source-sha "$source_sha" \
    --configuration-sha256 "$config_sha" \
    --migration-inventory-sha256 "$migration_sha"
}

admit >/dev/null

reject() {
  local name=$1
  if admit >/dev/null 2>&1; then
    echo "negative fixture accepted: $name" >&2
    exit 1
  fi
}

# Wrong repository and a non-main ref must not reuse a valid image pair.
sed -i 's#cheese-work/multica#other-workspace/multica#' "$event"
reject wrong-repository
sed -i 's#other-workspace/multica#cheese-work/multica#' "$event"
sed -i 's#refs/heads/main#refs/heads/feature#' "$event"
reject wrong-event
sed -i 's#refs/heads/feature#refs/heads/main#' "$event"

# A successful check from another revision cannot qualify this candidate.
sed -i 's/504078f8ea7fa31f342f195659e93a7f6c3e5a91/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/' "$checks"
reject stale-checks
sed -i 's/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/504078f8ea7fa31f342f195659e93a7f6c3e5a91/' "$checks"

# Registry provenance must bind the same amd64 digest, source repository and
# source SHA. A copied image with a good tag cannot satisfy this record.
sed -i 's/linux\/amd64/linux\/arm64/' "$provenance"
reject wrong-architecture
sed -i 's/linux\/arm64/linux\/amd64/' "$provenance"
sed -i 's#cheese-work/multica#other-workspace/multica#2' "$provenance"
reject wrong-provenance-repository
sed -i 's#other-workspace/multica#cheese-work/multica#' "$provenance"

# An image pair attached to a non-main source SHA is rejected even if the event
# and check payloads claim success.
sed -i 's/504078f8ea7fa31f342f195659e93a7f6c3e5a91/cccccccccccccccccccccccccccccccccccccccc/' "$manifest"
reject source-mismatch

echo "admission positive and negative fixtures passed"
