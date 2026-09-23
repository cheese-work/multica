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

# build_c00_shape_packet.mjs builds a packet reproducing C00's actual
# pre-cutover shape from run 35745037983's admitted baseline tuple:
# ordered_migrations is the full on-disk set through the renamed files'
# current latest (512_stage_completion_wake_workspace_index — the CHE-650
# upstream v0.5.0 sync moved the fork's own migrations twice: the five
# CHE-548 renames 491-495 -> 504-508, and the CHE-488 stage-completion-wake
# group 496-499 -> 509-512, because upstream now owns 491-499 outright).
# The observed ledger is NOT the same version list: it substitutes all nine
# renames back to their original pre-sync names, and — critically — excludes
# upstream's own 491-499 (491_issue_status_category_backfill and so on)
# entirely, because C00's ledger never applied those; they only exist on
# this branch after the sync. Listing them in ordered_migrations (they are
# real on-disk files) without also listing them in the observed ledger is
# correct — C00 admits as pending, not applied. An earlier version of this
# fixture built the ledger from ALL on-disk versions through 508, which
# silently included upstream's 491-499 as if C00 had run them and made the
# dirty-ledger check vacuous for the fork's own then-unmapped 496-499 rename
# gap (CHE-650 review finding).
build_c00_shape_packet() {
  local out=$1
  node -e '
    const fs = require("fs");
    const crypto = require("crypto");
    const path = require("path");
    const dir = "server/migrations";
    const latest = "512_stage_completion_wake_workspace_index";
    const files = fs.readdirSync(dir).filter((f) => f.endsWith(".up.sql")).sort();
    const latestIdx = files.findIndex((f) => f === `${latest}.up.sql`);
    if (latestIdx === -1) throw new Error(`fixture assumption failed: ${latest}.up.sql not found under ${dir}`);
    const upToLatest = files.slice(0, latestIdx + 1);
    const ordered = upToLatest.map((f) => {
      const bytes = fs.readFileSync(path.join(dir, f));
      return { version: f.replace(/\.up\.sql$/, ""), sha256: "sha256:" + crypto.createHash("sha256").update(bytes).digest("hex") };
    });
    const renames = new Map([
      ["504_github_merge_announcement", "470_github_merge_announcement"],
      ["505_github_merge_announcement_identity_uidx", "471_github_merge_announcement_identity_uidx"],
      ["506_github_merge_announcement_pending_idx", "472_github_merge_announcement_pending_idx"],
      ["507_github_merge_announcement_html_url", "473_github_merge_announcement_html_url"],
      ["508_agent_task_rerun_lineage_unique", "474_agent_task_rerun_lineage_unique"],
      ["509_stage_completion_wake", "496_stage_completion_wake"],
      ["510_stage_completion_wake_unique", "497_stage_completion_wake_unique"],
      ["511_stage_generation_workspace_index", "498_stage_generation_workspace_index"],
      ["512_stage_completion_wake_workspace_index", "499_stage_completion_wake_workspace_index"],
    ]);
    for (const onDiskName of renames.keys()) {
      if (!ordered.some((o) => o.version === onDiskName)) {
        throw new Error(`fixture assumption failed: ${onDiskName} not found through ${latest}`);
      }
    }
    // upstream v0.5.0 own migrations landed at the numeric range the
    // fork pre-sync files used to occupy (491-499). C00 never ran them:
    // they must appear in ordered_migrations (real on-disk files, so the
    // candidate checksum coverage includes them) but NOT in the observed
    // ledger below, or this fixture stops being C00-shaped.
    const upstreamPending = new Set([
      "491_issue_status_category_backfill",
      "492_issue_status_category_contract",
      "493_issue_status_category_validate",
      "494_issue_status_category_read_contract",
      "495_issue_to_label_label_id_index",
      "496_chat_session_agent_id_index",
      "497_agent_task_queue_delegated_failure_evidence_index",
      "498_chat_session_runtime_id_index",
      "499_agent_task_issue_snapshot",
    ]);
    for (const version of upstreamPending) {
      if (!ordered.some((o) => o.version === version)) {
        throw new Error(`fixture assumption failed: upstream pending version ${version} not found on disk`);
      }
    }
    const observedLedger = ordered
      .filter((o) => !upstreamPending.has(o.version))
      .map((o) => renames.get(o.version) ?? o.version);
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
        ledger: { status: "complete", versions: observedLedger },
        indexes: { status: "complete", invalid: [] },
        hooks: { status: "complete", observations: [{ name: "backfill_example", status: "completed" }] },
      },
      previous_image_pair: {
        backend: "ghcr.io/cheese-work/multica-backend:sha-abc123",
        web: "ghcr.io/cheese-work/multica-web:sha-abc123",
        backend_digest: "sha256:" + "a".repeat(64),
        web_digest: "sha256:" + "b".repeat(64),
      },
      minimum_rollback_version: ordered[ordered.length - 1].version,
    };
    fs.writeFileSync(process.argv[1], JSON.stringify(packet, null, 2));
  ' "$out"
}

c00_shape_packet="$work_dir/c00-shape.json"
build_c00_shape_packet "$c00_shape_packet"

# ---------------------------------------------------------------------------
# Scenario 10: a ledger reproducing C00's real pre-cutover shape — the
# on-disk migration set through 501 with all five CHE-548 renames still
# recorded under their pre-rename names — must reconcile every one of them
# and verify. This is the exact case run 35745037983 stopped on; reconciling
# only 470 would still leave 471-474 dirty against this same ledger.
# ---------------------------------------------------------------------------
output="$(node deploy/cd/release-packet.mjs verify --packet "$c00_shape_packet" --manifest "$manifest" 2>&1)"
status=$?
expect_exit 0 "$status" c00-shape-ledger-replay
expect_contains "$output" '"ok":true' c00-shape-ledger-replay

# ---------------------------------------------------------------------------
# Scenario 11 (negative control): a version that does not exist under
# server/migrations/ at all, and is not one of the five known renames, must
# still fail closed as dirty-ledger state — the rename reconciliation is a
# fixed five-entry mapping, not a general amnesty for unrecognized names.
# 999_not_a_migration cannot collide with an on-disk file (three-digit
# migration numbering tops out well below 999) and is not itself a rename
# target, unlike 470_issue_status_icon (a real, differently-numbered on-disk
# migration) which would only fail here because a narrower fixture omits it.
# ---------------------------------------------------------------------------
unknown_rename="$work_dir/unknown-rename.json"
mutate "$c00_shape_packet" "$unknown_rename" '
  const idx = p.observed_state.ledger.versions.indexOf("470_github_merge_announcement");
  if (idx === -1) throw new Error("fixture assumption failed: 470_github_merge_announcement not in c00-shape ledger");
  p.observed_state.ledger.versions[idx] = "999_not_a_migration";
'
set +e
output="$(node deploy/cd/release-packet.mjs verify --packet "$unknown_rename" --manifest "$manifest" 2>&1)"
status=$?
set -e
expect_exit 1 "$status" unknown-ledger-rename
expect_contains "$output" "ledger contains version 999_not_a_migration not present in ordered_migrations — dirty ledger state" unknown-ledger-rename

echo "release-packet.mjs control-flow fixtures passed"
