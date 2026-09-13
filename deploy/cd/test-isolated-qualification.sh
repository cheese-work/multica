#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

source_sha="504078f8ea7fa31f342f195659e93a7f6c3e5a91"
digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
baseline_tuple="deploy/cd/fixtures/c00-tuple-2026-09-12T235517Z.json"
baseline_tuple_sha="$(node deploy/cd/tuple-snapshot.mjs digest --snapshot "$baseline_tuple")"
node deploy/cd/release-manifest.mjs create \
  --kind build-evidence \
  --repository cheese-work/multica \
  --source-sha "$source_sha" \
  --architecture linux/amd64 \
  --configuration-sha256 "sha256:1111111111111111111111111111111111111111111111111111111111111111" \
  --migration-inventory-sha256 "sha256:2222222222222222222222222222222222222222222222222222222222222222" \
  --baseline-tuple-sha256 "$baseline_tuple_sha" \
  --backend-image "ghcr.io/cheese-work/multica-backend@$digest" \
  --web-image "ghcr.io/cheese-work/multica-web@$digest" \
  --output "$tmp_dir/manifest.json"

printf '1\n' >"$tmp_dir/D1_ISOLATED_SYNTHETIC_FIXTURE"
printf ':\n' >"$tmp_dir/candidate-write.sh"
printf ':\n' >"$tmp_dir/old-assert.sh"

printf 'preserved archive bytes\n' >"$tmp_dir/old-image.tar.zst"
printf '{"source_ref":"example/old:1","source_image_id":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","architecture":"amd64","os":"linux","rootfs_diff_ids":["sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]}\n' >"$tmp_dir/old-image.metadata.json"
archive_sha="sha256:$(sha256sum "$tmp_dir/old-image.tar.zst" | awk '{print $1}')"
metadata_sha="sha256:$(sha256sum "$tmp_dir/old-image.metadata.json" | awk '{print $1}')"

bash deploy/cd/qualification.sh \
  --manifest "$tmp_dir/manifest.json" \
  --baseline-tuple "$baseline_tuple" \
  --fixture-dir "$tmp_dir" \
  --old-backend "ghcr.io/cheese-work/multica-backend@$digest" \
  --old-web "ghcr.io/cheese-work/multica-web@$digest" \
  --candidate-write-file "$tmp_dir/candidate-write.sh" \
  --old-assert-file "$tmp_dir/old-assert.sh" \
  --dry-run >/dev/null

bash deploy/cd/qualification.sh \
  --manifest "$tmp_dir/manifest.json" \
  --baseline-tuple "$baseline_tuple" \
  --fixture-dir "$tmp_dir" \
  --old-backend-archive "$tmp_dir/old-image.tar.zst" \
  --old-backend-archive-sha256 "$archive_sha" \
  --old-backend-metadata "$tmp_dir/old-image.metadata.json" \
  --old-backend-metadata-sha256 "$metadata_sha" \
  --old-web-archive "$tmp_dir/old-image.tar.zst" \
  --old-web-archive-sha256 "$archive_sha" \
  --old-web-metadata "$tmp_dir/old-image.metadata.json" \
  --old-web-metadata-sha256 "$metadata_sha" \
  --candidate-write-file "$tmp_dir/candidate-write.sh" \
  --old-assert-file "$tmp_dir/old-assert.sh" \
  --dry-run >/dev/null

if bash deploy/cd/qualification.sh \
  --manifest "$tmp_dir/manifest.json" \
  --baseline-tuple "$baseline_tuple" \
  --fixture-dir "$tmp_dir" \
  --old-backend "ghcr.io/cheese-work/multica-backend@$digest" \
  --old-web-archive "$tmp_dir/old-image.tar.zst" \
  --old-web-archive-sha256 "$archive_sha" \
  --old-web-metadata "$tmp_dir/old-image.metadata.json" \
  --old-web-metadata-sha256 "$metadata_sha" \
  --candidate-write-file "$tmp_dir/candidate-write.sh" \
  --old-assert-file "$tmp_dir/old-assert.sh" \
  --dry-run >/dev/null 2>&1; then
  echo "mixed registry/archive previous-image inputs were accepted" >&2
  exit 1
fi

rm "$tmp_dir/D1_ISOLATED_SYNTHETIC_FIXTURE"
if bash deploy/cd/qualification.sh \
  --manifest "$tmp_dir/manifest.json" \
  --baseline-tuple "$baseline_tuple" \
  --fixture-dir "$tmp_dir" \
  --old-backend "ghcr.io/cheese-work/multica-backend@$digest" \
  --old-web "ghcr.io/cheese-work/multica-web@$digest" \
  --candidate-write-file "$tmp_dir/candidate-write.sh" \
  --old-assert-file "$tmp_dir/old-assert.sh" \
  --dry-run >/dev/null 2>&1; then
  echo "unclassified fixture was accepted" >&2
  exit 1
fi

echo "isolated qualification guard fixtures passed"
