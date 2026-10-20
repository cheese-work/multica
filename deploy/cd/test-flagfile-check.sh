#!/usr/bin/env bash
set -euo pipefail

# flagfile-check.mjs admits only a documented subset of the backend flag-file
# schema. Against the shared corpus (the Go loader classifies the same files in
# server/internal/featureflags): valid/ must pass; invalid/ (the loader rejects)
# and strict/ (the loader accepts, the controller does not) must be refused.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"
corpus=deploy/cd/fixtures/flags

for dir in valid invalid strict; do
  [ -n "$(ls "$corpus/$dir"/*.yaml 2>/dev/null)" ] || { echo "no corpus files in $corpus/$dir" >&2; exit 1; }
done

for file in "$corpus"/valid/*.yaml deploy/cd/fixtures/jev-feature-flags.synthetic.yaml; do
  node deploy/cd/flagfile-check.mjs "$file" 2>&1 || { echo "valid file refused: $file" >&2; exit 1; }
done
for file in "$corpus"/invalid/*.yaml "$corpus"/strict/*.yaml; do
  if err="$(node deploy/cd/flagfile-check.mjs "$file" 2>&1)"; then
    echo "file admitted but must be refused: $file" >&2
    exit 1
  fi
  case "$err" in *"flag file"*) ;; *) echo "refusal for $file has no 'flag file' message: $err" >&2; exit 1 ;; esac
done
echo "flagfile-check fixtures passed"
