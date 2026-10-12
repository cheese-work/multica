#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: capture-release-state.sh --compose-dir PATH --output PATH" >&2
}

compose_dir=""
output=""
while (($#)); do
  case "$1" in
    --compose-dir) compose_dir=${2:?}; shift 2 ;;
    --output) output=${2:?}; shift 2 ;;
    --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage; exit 2 ;;
  esac
done
[ -n "$compose_dir" ] || { echo "missing --compose-dir" >&2; exit 2; }
[ -n "$output" ] || { echo "missing --output" >&2; exit 2; }

compose=(docker compose -f docker-compose.selfhost.yml -f deploy/cd/docker-compose.ab.yml)
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

(
  cd "$compose_dir"
  "${compose[@]}" exec -T postgres psql \
    -U "${POSTGRES_USER:-multica}" -d "${POSTGRES_DB:-multica}" -tA -c \
    "SELECT version FROM schema_migrations ORDER BY version"
) >"$work_dir/ledger.txt"

(
  cd "$compose_dir"
  "${compose[@]}" exec -T postgres psql \
    -U "${POSTGRES_USER:-multica}" -d "${POSTGRES_DB:-multica}" -tA -c \
    "SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname)
       FROM pg_index i
       JOIN pg_class c ON c.oid = i.indexrelid
       JOIN pg_namespace n ON n.oid = c.relnamespace
      WHERE i.indisvalid = false
        AND n.nspname NOT IN ('pg_catalog', 'information_schema')
      ORDER BY 1"
) >"$work_dir/invalid-indexes.txt"

node - "$work_dir/ledger.txt" "$work_dir/invalid-indexes.txt" "$output" <<'NODE'
const fs = require("node:fs");
const [ledgerPath, indexesPath, outputPath] = process.argv.slice(2);
const lines = (path) => fs.readFileSync(path, "utf8").split("\n").filter(Boolean);
const versions = lines(ledgerPath);
if (versions.length === 0) throw new Error("schema_migrations observation returned no rows");
const applied = new Set(versions);
const hooks = [
  ["task_usage_hourly_rollup", "103_drop_legacy_daily_rollups"],
  ["attribution_strict_backfill", "198_agent_task_attribution_strict_constraint_validate"],
  ["chat_explicit_origin_backfill", "431_chat_explicit_origin_backfill"],
].map(([name, version]) => ({
  name,
  version,
  status: applied.has(version) ? "completed" : "pending",
}));
const state = {
  schema_version: 1,
  observed_at: new Date().toISOString().replace(/\.\d+Z$/, "Z"),
  ledger: { status: "complete", versions },
  indexes: { status: "complete", invalid: lines(indexesPath) },
  hooks: { status: "complete", observations: hooks },
};
fs.writeFileSync(outputPath, `${JSON.stringify(state, null, 2)}\n`);
NODE
