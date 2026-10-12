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
baseline_tuple="deploy/cd/fixtures/c00-tuple-2026-09-12T235517Z.json"
baseline_tuple_sha="$(node deploy/cd/tuple-snapshot.mjs digest --snapshot "$baseline_tuple")"
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
  --baseline-tuple-sha256 "$baseline_tuple_sha" \
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
    --baseline-tuple "$baseline_tuple" \
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
sed -i 's/cccccccccccccccccccccccccccccccccccccccc/504078f8ea7fa31f342f195659e93a7f6c3e5a91/' "$manifest"

# --- path-filtered required checks -----------------------------------------
#
# `mobile` only runs when a commit touches mobile-relevant paths, so a
# docs-only commit produces no `mobile` check at all. Admission must tell that
# apart from a check that is missing for any other reason: absence is excused
# only when the commit provably matches none of the filter's paths, and the
# proof is re-derived here from the raw file list, never asserted by the
# recorder.

write_checks() {
  # $1 = mobile conclusion as a JSON fragment (`null` omits the key entirely)
  # $2 = changed_files JSON array
  # $3 = extra top-level JSON fields, may be empty
  local mobile_entry=""
  [ "$1" != "omit" ] && mobile_entry="\"mobile\":$1,"
  cat >"$checks" <<EOF
{
  "sha": "$source_sha",
  "contexts": {$mobile_entry "backend":"success","frontend":"success","cd-qualification":"success"},
  "changed_files": $2,
  "changed_files_truncated": false,
  "path_filters": {"mobile": ["apps/mobile/**", "packages/core/**", "package.json"]}
  $3
}
EOF
}

# Absent + no matching file -> admitted. This is the real 3afcfb94 case: a
# docs-only commit under server/.
write_checks omit '["server/internal/service/builtin_skills/multica-platform/references/issues.md"]' ""
admit >/dev/null

# Absent + a file the filter DOES cover -> refused. The check should have run;
# its absence is a real gap, not an exclusion.
write_checks omit '["apps/mobile/app/index.tsx"]' ""
reject path-filtered-check-absent-but-required

# A `**` filter must match at any depth, and a bare filename must not match a
# same-named file in a subdirectory.
write_checks omit '["packages/core/api/schema.ts"]' ""
reject path-filter-doublestar-matches-nested
write_checks omit '["apps/mobile/package.json"]' ""
reject path-filter-matches-nested-package-json

# Having RUN and failed is never excusable, whatever the commit touched.
write_checks '"failure"' '["server/only.md"]' ""
reject path-filtered-check-actually-failed
write_checks '"cancelled"' '["server/only.md"]' ""
reject path-filtered-check-cancelled

# A truncated file list cannot prove absence of a match, so it must not excuse.
write_checks omit '["server/only.md"]' ',"changed_files_truncated": true'
reject truncated-file-list-cannot-excuse

# Without the recorded evidence the check stays unconditionally required —
# an older evidence set must not be admitted by the new code path.
cat >"$checks" <<EOF
{"sha":"$source_sha","contexts":{"backend":"success","frontend":"success","cd-qualification":"success"}}
EOF
reject legacy-checks-without-path-filter-evidence

# --- pending required checks -----------------------------------------------
#
# Run 35822016641 snapshotted the checks while CI's `backend` was still in
# flight, and admission refused it as a failure. The recorder now asks
# `pending` which checks to wait for: a check is pending while it runs (null
# conclusion) or before its run exists (absent and not path-excluded).

expect_pending() {
  local name=$1 want=$2 got
  got="$(node deploy/cd/admission.mjs pending --checks "$checks" | tr '\n' ' ')"
  if [ "$got" != "$want" ]; then
    echo "pending fixture $name: want '$want', got '$got'" >&2
    exit 1
  fi
}

write_checks '"success"' '["server/only.md"]' ""
expect_pending all-settled ""

# The real 5c834a2f snapshot: backend's needs-gated run did not exist yet.
cat >"$checks" <<EOF
{"sha":"$source_sha","contexts":{"frontend":null,"cd-qualification":"success"},
 "changed_files":["server/only.md"],"changed_files_truncated":false,
 "path_filters":{"mobile":["apps/mobile/**"]}}
EOF
expect_pending backend-not-created-frontend-running "backend frontend "
reject pending-check-at-wait-deadline
# A check still running at the deadline is refused under its real name.
sed -i 's/"frontend":null/"backend":null,"frontend":"success"/' "$checks"
if admit >/dev/null 2>"$tmp_dir/err.txt"; then
  echo "negative fixture accepted: running-check-at-wait-deadline" >&2
  exit 1
fi
grep -q "required check backend has not completed" "$tmp_dir/err.txt"

# Terminal results are settled, so the wait ends, and they are still refused.
write_checks '"failure"' '["server/only.md"]' ""
expect_pending terminal-failure-is-settled ""
reject terminal-failure-after-wait
write_checks '"cancelled"' '["server/only.md"]' ""
expect_pending terminal-cancelled-is-settled ""
reject terminal-cancelled-after-wait

# An absent check its path filter covers is waited for, never excused.
write_checks omit '["apps/mobile/app/index.tsx"]' ""
expect_pending path-filtered-check-not-created-yet "mobile "
write_checks omit '["server/only.md"]' ""
expect_pending path-filtered-check-excluded ""

echo "admission positive and negative fixtures passed"
