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

node deploy/cd/release-manifest.mjs verify \
  --manifest "$manifest" \
  --expected-repository cheese-work/multica \
  --expected-source-sha "$source_sha" \
  --expected-configuration-sha256 "$config_sha" \
  --expected-migration-inventory-sha256 "$migration_sha" >/dev/null

if node deploy/cd/release-manifest.mjs create \
  --kind release-candidate \
  --repository cheese-work/multica \
  --source-sha "$source_sha" \
  --architecture linux/arm64 \
  --configuration-sha256 "$config_sha" \
  --migration-inventory-sha256 "$migration_sha" \
  --backend-image "ghcr.io/cheese-work/multica-backend@$backend_digest" \
  --web-image "ghcr.io/cheese-work/multica-web@$web_digest" \
  --output "$tmp_dir/arm.json" >/dev/null 2>&1; then
  echo "linux/arm64 manifest was accepted" >&2
  exit 1
fi

echo "release manifest fixtures passed"
