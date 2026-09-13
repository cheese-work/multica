#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

snapshot="deploy/cd/fixtures/c00-tuple-2026-09-12T235517Z.json"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

node deploy/cd/tuple-snapshot.mjs verify --snapshot "$snapshot" >/dev/null
digest="$(node deploy/cd/tuple-snapshot.mjs digest --snapshot "$snapshot")"
[[ "$digest" =~ ^sha256:[a-f0-9]{64}$ ]]

node -e '
  const fs = require("node:fs");
  const snapshot = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
  snapshot.role = "local-fixture";
  fs.writeFileSync(process.argv[2], `${JSON.stringify(snapshot)}\n`);
' "$snapshot" "$tmp_dir/not-a-baseline.json"

if node deploy/cd/tuple-snapshot.mjs verify --snapshot "$tmp_dir/not-a-baseline.json" >/dev/null 2>&1; then
  echo "non-baseline tuple metadata was accepted" >&2
  exit 1
fi

echo "C00 tuple snapshot fixtures passed"
