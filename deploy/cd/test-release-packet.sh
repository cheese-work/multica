#!/usr/bin/env bash
set -euo pipefail

# Behavioral test for release-packet.mjs (CHE-397 unit 2): the expand/
# contract release-packet contract checks — ordered migration checksums,
# ledger cleanliness, invalid indexes, hook completion, and a compatible
# previous image pair / minimum rollback version. Every negative control
# here must genuinely fail; a negative assertion that cannot fail is a
# defect the issue explicitly calls out.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

expect_exit() {
  local want=$1 got=$2 name=$3
  if [ "$got" -ne "$want" ]; then
    echo "scenario $name: exit $got, want $want" >&2
    exit 1
  fi
}

expect_contains() {
  local haystack=$1 needle=$2 name=$3
  if [[ "$haystack" != *"$needle"* ]]; then
    echo "scenario $name: output missing expected text: $needle" >&2
    echo "--- captured output ---" >&2
    echo "$haystack" >&2
    exit 1
  fi
}

# base_packet.mjs builds a valid packet from the last N real migrations on
# disk, so this test is exercised against real checksums rather than
# fixture data that could drift from server/migrations/ over time.
build_base_packet() {
  local out=$1
  node -e '
    const fs = require("fs");
    const crypto = require("crypto");
    const path = require("path");
    const dir = "server/migrations";
    const files = fs.readdirSync(dir).filter((f) => f.endsWith(".up.sql")).sort();
    const last = files.slice(-5);
    const ordered = last.map((f) => {
      const bytes = fs.readFileSync(path.join(dir, f));
      return { version: f.replace(/\.up\.sql$/, ""), sha256: "sha256:" + crypto.createHash("sha256").update(bytes).digest("hex") };
    });
    const packet = {
      schema_version: 1,
      candidate: {
        source_sha: "504078f8ea7fa31f342f195659e93a7f6c3e5a91",
        backend_image: "ghcr.io/cheese-work/multica-backend@sha256:" + "c".repeat(64),
        web_image: "ghcr.io/cheese-work/multica-web@sha256:" + "d".repeat(64),
        migration_inventory_sha256: "sha256:" + "e".repeat(64),
        baseline_tuple_sha256: "sha256:" + "f".repeat(64),
      },
      ordered_migrations: ordered,
      observed_state: {
        schema_version: 1,
        ledger: { status: "complete", versions: ordered.map((o) => o.version) },
        indexes: { status: "complete", invalid: [] },
        hooks: { status: "complete", observations: [{ name: "backfill_example", status: "completed" }] },
      },
      previous_image_pair: {
        backend: "ghcr.io/cheese-work/multica-backend:sha-abc123",
        web: "ghcr.io/cheese-work/multica-web:sha-abc123",
        backend_digest: "sha256:" + "a".repeat(64),
        web_digest: "sha256:" + "b".repeat(64),
      },
      minimum_rollback_version: ordered[0].version,
    };
    fs.writeFileSync(process.argv[1], JSON.stringify(packet, null, 2));
  ' "$out"
}

mutate() {
  local in=$1 out=$2 js=$3
  node -e "
    const fs = require('fs');
    const p = JSON.parse(fs.readFileSync(process.argv[1], 'utf8'));
    $js
    fs.writeFileSync(process.argv[2], JSON.stringify(p));
  " "$in" "$out"
}

base_packet="$work_dir/base.json"
build_base_packet "$base_packet"
manifest="$work_dir/manifest.json"
cat >"$manifest" <<JSON
{
  "source_sha": "504078f8ea7fa31f342f195659e93a7f6c3e5a91",
  "images": {
    "backend": "ghcr.io/cheese-work/multica-backend@sha256:$(printf 'c%.0s' {1..64})",
    "web": "ghcr.io/cheese-work/multica-web@sha256:$(printf 'd%.0s' {1..64})"
  },
  "migration_inventory_sha256": "sha256:$(printf 'e%.0s' {1..64})",
  "baseline_tuple_sha256": "sha256:$(printf 'f%.0s' {1..64})"
}
JSON

# ---------------------------------------------------------------------------
# Scenario 1: happy path.
# ---------------------------------------------------------------------------
output="$(node deploy/cd/release-packet.mjs verify --packet "$base_packet" --manifest "$manifest" 2>&1)"
status=$?
expect_exit 0 "$status" happy-path
expect_contains "$output" '"ok":true' happy-path

# ---------------------------------------------------------------------------
# Scenario 1b: a packet is never valid by itself; it must be bound to the
# exact admitted manifest, and a stale candidate binding must fail closed.
# ---------------------------------------------------------------------------
set +e
output="$(node deploy/cd/release-packet.mjs verify --packet "$base_packet" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" unbound-packet
expect_contains "$output" "missing --manifest" unbound-packet

stale="$work_dir/stale.json"
mutate "$base_packet" "$stale" 'p.candidate.source_sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";'
set +e
output="$(node deploy/cd/release-packet.mjs verify --packet "$stale" --manifest "$manifest" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" stale-packet
expect_contains "$output" "candidate.source_sha does not match manifest" stale-packet

# Create produces release-specific evidence from the admitted manifest and
# its digest-bound baseline tuple; the result immediately verifies.
create_baseline="$work_dir/create-baseline.json"
node -e '
  const fs = require("fs");
  const versions = fs.readdirSync("server/migrations").filter((f) => f.endsWith(".up.sql")).sort().slice(-5);
  const baseline = {
    images: {
      backend: { digest: "sha256:" + "a".repeat(64) },
      web: { digest: "sha256:" + "b".repeat(64) },
    },
    migration_ledger: { latest: { version: versions[0].replace(/\.up\.sql$/, "") } },
  };
  fs.writeFileSync(process.argv[1], JSON.stringify(baseline));
' "$create_baseline"
create_baseline_digest="$(node -e 'const fs=require("fs"),c=require("crypto");process.stdout.write("sha256:"+c.createHash("sha256").update(fs.readFileSync(process.argv[1])).digest("hex"))' "$create_baseline")"
create_migration_digest="$(node deploy/cd/migration-inventory.mjs)"
create_manifest="$work_dir/create-manifest.json"
node -e '
  const fs = require("fs");
  const [out, baselineDigest, migrationDigest] = process.argv.slice(1);
  fs.writeFileSync(out, JSON.stringify({
    source_sha: "504078f8ea7fa31f342f195659e93a7f6c3e5a91",
    images: {
      backend: "ghcr.io/cheese-work/multica-backend@sha256:" + "c".repeat(64),
      web: "ghcr.io/cheese-work/multica-web@sha256:" + "d".repeat(64),
    },
    migration_inventory_sha256: migrationDigest,
    baseline_tuple_sha256: baselineDigest,
  }));
' "$create_manifest" "$create_baseline_digest" "$create_migration_digest"
created_packet="$work_dir/created-packet.json"
create_observed="$work_dir/create-observed.json"
node -e '
  const fs = require("fs");
  const versions = fs.readdirSync("server/migrations").filter((f) => f.endsWith(".up.sql")).sort().map((f) => f.replace(/\.up\.sql$/, ""));
  fs.writeFileSync(process.argv[1], JSON.stringify({
    schema_version: 1,
    ledger: { status: "complete", versions: versions.slice(0, versions.indexOf(process.argv[2]) + 1) },
    indexes: { status: "complete", invalid: [] },
    hooks: { status: "complete", observations: [
      { name: "task_usage_hourly_rollup", status: "completed" },
      { name: "attribution_strict_backfill", status: "completed" },
      { name: "chat_explicit_origin_backfill", status: "completed" },
    ] },
  }));
' "$create_observed" "$(node -e 'const fs=require("fs");console.log(fs.readdirSync("server/migrations").filter(f=>f.endsWith(".up.sql")).sort().slice(-5)[0].replace(/\.up\.sql$/, ""))')"
node -e '
  const fs=require("fs"),c=require("crypto");
  const p=process.argv[1], o=process.argv[2];
  const x=JSON.parse(fs.readFileSync(p)); const s=JSON.parse(fs.readFileSync(o));
  x.migration_ledger.row_count=s.ledger.versions.length;
  x.migration_ledger.ordered_sha256="sha256:"+c.createHash("sha256").update(s.ledger.versions.join("\n")+"\n").digest("hex");
  fs.writeFileSync(p,JSON.stringify(x));
' "$create_baseline" "$create_observed"
# Refresh the manifest binding after adding the real observed ledger digest.
create_baseline_digest="$(node -e 'const fs=require("fs"),c=require("crypto");process.stdout.write("sha256:"+c.createHash("sha256").update(fs.readFileSync(process.argv[1])).digest("hex"))' "$create_baseline")"
node -e 'const fs=require("fs");const p=process.argv[1];const x=JSON.parse(fs.readFileSync(p));x.baseline_tuple_sha256=process.argv[2];fs.writeFileSync(p,JSON.stringify(x))' "$create_manifest" "$create_baseline_digest"
node deploy/cd/release-packet.mjs create --manifest "$create_manifest" --baseline-tuple "$create_baseline" --observed-state "$create_observed" --output "$created_packet"
output="$(node deploy/cd/release-packet.mjs verify --packet "$created_packet" --manifest "$create_manifest" 2>&1)"
expect_contains "$output" '"ok":true' create-bound-packet

# ---------------------------------------------------------------------------
# Scenario 2 (negative control): checksum drift must fail.
# ---------------------------------------------------------------------------
drift="$work_dir/drift.json"
mutate "$base_packet" "$drift" 'p.ordered_migrations[0].sha256 = "sha256:" + "f".repeat(64);'
set +e
output="$(node deploy/cd/release-packet.mjs verify --packet "$drift" --manifest "$manifest" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" checksum-drift
expect_contains "$output" "checksum drift" checksum-drift

# ---------------------------------------------------------------------------
# Scenario 3 (negative control): a ledger row for a version outside
# ordered_migrations is dirty state and must fail.
# ---------------------------------------------------------------------------
dirty="$work_dir/dirty.json"
mutate "$base_packet" "$dirty" 'p.observed_state.ledger.versions.push("999_unknown");'
set +e
output="$(node deploy/cd/release-packet.mjs verify --packet "$dirty" --manifest "$manifest" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" dirty-ledger
expect_contains "$output" "dirty ledger state" dirty-ledger

# ---------------------------------------------------------------------------
# Scenario 4 (negative control): incomplete observed ledger state must fail.
# ---------------------------------------------------------------------------
writer="$work_dir/writer.json"
mutate "$base_packet" "$writer" 'delete p.observed_state.ledger.status;'
set +e
output="$(node deploy/cd/release-packet.mjs verify --packet "$writer" --manifest "$manifest" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" unknown-ledger-state
expect_contains "$output" "observed ledger state is unknown or incomplete" unknown-ledger-state

# ---------------------------------------------------------------------------
# Scenario 5 (negative control): an invalid index must fail.
# ---------------------------------------------------------------------------
idx="$work_dir/idx.json"
mutate "$base_packet" "$idx" 'p.observed_state.indexes.invalid = ["public.idx_example"];'
set +e
output="$(node deploy/cd/release-packet.mjs verify --packet "$idx" --manifest "$manifest" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" invalid-index
expect_contains "$output" "invalid indexes present" invalid-index

# ---------------------------------------------------------------------------
# Scenario 6 (negative control): an incomplete hook/backfill must fail.
# ---------------------------------------------------------------------------
hook="$work_dir/hook.json"
mutate "$base_packet" "$hook" 'p.observed_state.hooks.observations[0].status = "pending";'
set +e
output="$(node deploy/cd/release-packet.mjs verify --packet "$hook" --manifest "$manifest" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" incomplete-hook
expect_contains "$output" "incomplete hooks/backfills" incomplete-hook

# ---------------------------------------------------------------------------
# Scenario 7 (negative control): a missing/malformed previous image pair
# digest must fail — a cutover packet cannot be admitted without a checked
# rollback target.
# ---------------------------------------------------------------------------
badpair="$work_dir/badpair.json"
mutate "$base_packet" "$badpair" 'p.previous_image_pair.backend_digest = "not-a-digest";'
set +e
output="$(node deploy/cd/release-packet.mjs verify --packet "$badpair" --manifest "$manifest" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" bad-previous-pair-digest
expect_contains "$output" "backend_digest must be" bad-previous-pair-digest

# ---------------------------------------------------------------------------
# Scenario 8 (negative control): a minimum_rollback_version that names no
# real migration must fail.
# ---------------------------------------------------------------------------
badversion="$work_dir/badversion.json"
mutate "$base_packet" "$badversion" 'p.minimum_rollback_version = "999_does_not_exist";'
set +e
output="$(node deploy/cd/release-packet.mjs verify --packet "$badversion" --manifest "$manifest" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" unknown-rollback-version
expect_contains "$output" "is not a known migration version" unknown-rollback-version

# ---------------------------------------------------------------------------
# Scenario 9 (negative control): out-of-order ordered_migrations must fail —
# a packet cannot assert an application sequence the migrator would never
# actually produce.
# ---------------------------------------------------------------------------
reordered="$work_dir/reordered.json"
mutate "$base_packet" "$reordered" 'p.ordered_migrations.reverse();'
set +e
output="$(node deploy/cd/release-packet.mjs verify --packet "$reordered" --manifest "$manifest" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" out-of-order-migrations
expect_contains "$output" "not in on-disk applied order" out-of-order-migrations

# base_rename_packet.mjs builds a packet whose ordered_migrations explicitly
# includes 491_github_merge_announcement (not necessarily in the plain
# last-5 slice build_base_packet uses), so the rename-reconciliation
# scenarios below have a real on-disk target version to reconcile onto.
build_base_rename_packet() {
  local out=$1
  node -e '
    const fs = require("fs");
    const crypto = require("crypto");
    const path = require("path");
    const dir = "server/migrations";
    const target = "491_github_merge_announcement";
    const files = fs.readdirSync(dir).filter((f) => f.endsWith(".up.sql")).sort();
    const targetIdx = files.findIndex((f) => f === `${target}.up.sql`);
    if (targetIdx === -1) throw new Error(`fixture assumption failed: ${target}.up.sql not found under ${dir}`);
    const last = files.slice(0, targetIdx + 1).slice(-5);
    const ordered = last.map((f) => {
      const bytes = fs.readFileSync(path.join(dir, f));
      return { version: f.replace(/\.up\.sql$/, ""), sha256: "sha256:" + crypto.createHash("sha256").update(bytes).digest("hex") };
    });
    const packet = {
      schema_version: 1,
      candidate: {
        source_sha: "504078f8ea7fa31f342f195659e93a7f6c3e5a91",
        backend_image: "ghcr.io/cheese-work/multica-backend@sha256:" + "c".repeat(64),
        web_image: "ghcr.io/cheese-work/multica-web@sha256:" + "d".repeat(64),
        migration_inventory_sha256: "sha256:" + "e".repeat(64),
        baseline_tuple_sha256: "sha256:" + "f".repeat(64),
      },
      ordered_migrations: ordered,
      observed_state: {
        schema_version: 1,
        ledger: { status: "complete", versions: ordered.map((o) => o.version) },
        indexes: { status: "complete", invalid: [] },
        hooks: { status: "complete", observations: [{ name: "backfill_example", status: "completed" }] },
      },
      previous_image_pair: {
        backend: "ghcr.io/cheese-work/multica-backend:sha-abc123",
        web: "ghcr.io/cheese-work/multica-web:sha-abc123",
        backend_digest: "sha256:" + "a".repeat(64),
        web_digest: "sha256:" + "b".repeat(64),
      },
      minimum_rollback_version: ordered[0].version,
    };
    fs.writeFileSync(process.argv[1], JSON.stringify(packet, null, 2));
  ' "$out"
}

base_rename_packet="$work_dir/base-rename.json"
build_base_rename_packet "$base_rename_packet"

# ---------------------------------------------------------------------------
# Scenario 10: a ledger row under the known pre-CHE-548 historical name
# (470_github_merge_announcement) must reconcile to the on-disk
# 491_github_merge_announcement version and verify — the exact case
# C00's ledger reported before the renumber merged.
# ---------------------------------------------------------------------------
renamed="$work_dir/renamed.json"
mutate "$base_rename_packet" "$renamed" '
  const idx = p.observed_state.ledger.versions.indexOf("491_github_merge_announcement");
  if (idx === -1) throw new Error("fixture assumption failed: 491_github_merge_announcement not in base packet ledger");
  p.observed_state.ledger.versions[idx] = "470_github_merge_announcement";
'
output="$(node deploy/cd/release-packet.mjs verify --packet "$renamed" --manifest "$manifest" 2>&1)"
status=$?
expect_exit 0 "$status" known-ledger-rename
expect_contains "$output" '"ok":true' known-ledger-rename

# ---------------------------------------------------------------------------
# Scenario 11 (negative control): an unrelated unknown historical version
# must still fail closed as dirty-ledger state — the rename reconciliation
# is a single fixed mapping, not a general amnesty for unrecognized names.
# ---------------------------------------------------------------------------
unknown_rename="$work_dir/unknown-rename.json"
mutate "$base_rename_packet" "$unknown_rename" '
  const idx = p.observed_state.ledger.versions.indexOf("491_github_merge_announcement");
  if (idx === -1) throw new Error("fixture assumption failed: 491_github_merge_announcement not in base packet ledger");
  p.observed_state.ledger.versions[idx] = "470_issue_status_icon";
'
set +e
output="$(node deploy/cd/release-packet.mjs verify --packet "$unknown_rename" --manifest "$manifest" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" unknown-ledger-rename
expect_contains "$output" "ledger contains version 470_issue_status_icon not present in ordered_migrations — dirty ledger state" unknown-ledger-rename

echo "release-packet.mjs control-flow fixtures passed"
