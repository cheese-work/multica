#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

usage() {
  cat <<'EOF'
usage: premerge-synthetic-qualification.sh --baseline-tuple PATH

Creates a non-deployable build-evidence manifest for the checked-out source and
runs the D1 dry-run gate. It uses only temporary synthetic files and never
contacts C00, a registry, deployment credentials, recovery keys, or production
data.
EOF
}

baseline_tuple=""
while (($#)); do
  case "$1" in
    --baseline-tuple) baseline_tuple=${2:?}; shift 2 ;;
    --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ -z "$baseline_tuple" ]; then
  echo "missing --baseline-tuple" >&2
  usage >&2
  exit 2
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

source_sha="$(git rev-parse HEAD)"
baseline_tuple_sha="$(node deploy/cd/tuple-snapshot.mjs digest --snapshot "$baseline_tuple")"
config_sha="sha256:$(sha256sum deploy/cd/fixtures/isolated-config.env | awk '{print $1}')"
migration_sha="$(node deploy/cd/migration-inventory.mjs)"
digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

node deploy/cd/release-manifest.mjs create \
  --kind build-evidence \
  --repository cheese-work/multica \
  --source-sha "$source_sha" \
  --architecture linux/amd64 \
  --configuration-sha256 "$config_sha" \
  --migration-inventory-sha256 "$migration_sha" \
  --baseline-tuple-sha256 "$baseline_tuple_sha" \
  --backend-image "ghcr.io/cheese-work/multica-backend@$digest" \
  --web-image "ghcr.io/cheese-work/multica-web@$digest" \
  --output "$tmp_dir/build-evidence.json"

printf '1\n' >"$tmp_dir/D1_ISOLATED_SYNTHETIC_FIXTURE"
printf ':\n' >"$tmp_dir/candidate-write.sh"
printf ':\n' >"$tmp_dir/old-assert.sh"

bash deploy/cd/qualification.sh \
  --manifest "$tmp_dir/build-evidence.json" \
  --baseline-tuple "$baseline_tuple" \
  --fixture-dir "$tmp_dir" \
  --old-backend "ghcr.io/cheese-work/multica-backend@$digest" \
  --old-web "ghcr.io/cheese-work/multica-web@$digest" \
  --candidate-write-file "$tmp_dir/candidate-write.sh" \
  --old-assert-file "$tmp_dir/old-assert.sh" \
  --dry-run

printf 'pre-merge synthetic qualification passed for source %s against %s\n' \
  "$source_sha" "$baseline_tuple_sha"
