#!/usr/bin/env bash
set -euo pipefail

bundle_root=${1:-}
[ -n "$bundle_root" ] || { echo "usage: verify-cutover-bundle.sh BUNDLE_ROOT" >&2; exit 2; }
for path in \
  controller-files.sha256 \
  evidence/manifest.json \
  evidence/release-packet.json \
  deploy/cd/cutover.sh \
  deploy/cd/release-packet.mjs \
  deploy/cd/migration-inventory.mjs \
  server/migrations; do
  [ -e "$bundle_root/$path" ] || { echo "cutover bundle missing exact path: $path" >&2; exit 1; }
done

(
  cd "$bundle_root"
  sha256sum -c controller-files.sha256 >/dev/null
)
expected_inventory="$(node -e 'const fs=require("node:fs");const m=JSON.parse(fs.readFileSync(process.argv[1],"utf8"));process.stdout.write(m.migration_inventory_sha256)' "$bundle_root/evidence/manifest.json")"
actual_inventory="$(node "$bundle_root/deploy/cd/migration-inventory.mjs" "$bundle_root/server/migrations")"
if [ "$actual_inventory" != "$expected_inventory" ]; then
  echo "cutover bundle migration inventory mismatch: expected $expected_inventory, got $actual_inventory" >&2
  exit 1
fi
node "$bundle_root/deploy/cd/release-packet.mjs" verify \
  --packet "$bundle_root/evidence/release-packet.json" \
  --manifest "$bundle_root/evidence/manifest.json" \
  --migrations-dir "$bundle_root/server/migrations" >/dev/null
