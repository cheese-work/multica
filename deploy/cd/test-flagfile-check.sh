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
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT
flags_file="$work_dir/frontend-flags.yaml"
for flag in project_workflows_v1 plugins_v1 composio_mcp_apps billing_workspace_subscriptions; do
  for expected in true false; do
    printf '%s:\n  default: %s\nprivate_flag:\n  default: true\n' "$flag" "$expected" >"$flags_file"
    config="$(printf '{"feature_flags":{"%s":%s}}' "$flag" "$expected")"
    node deploy/cd/flagfile-check.mjs "$flags_file" --config-stdin <<<"$config"
    for actual in missing "\"$expected\"" "$([ "$expected" = true ] && echo false || echo true)"; do
      config='{"feature_flags":{}}'
      [ "$actual" = missing ] || config="$(printf '{"feature_flags":{"%s":%s}}' "$flag" "$actual")"
      if node deploy/cd/flagfile-check.mjs "$flags_file" --config-stdin <<<"$config" >"$work_dir/error" 2>&1; then
        echo "frontend flag check admitted $flag=$actual, expected $expected" >&2
        exit 1
      fi
      grep -q "frontend flag mismatch: $flag" "$work_dir/error"
    done
  done
done
for config in 'not-json' '{}' 'null' '{"feature_flags":null}' '{"feature_flags":[]}'; do
  if node deploy/cd/flagfile-check.mjs "$flags_file" --config-stdin <<<"$config" >"$work_dir/error" 2>&1; then
    echo "frontend flag check admitted malformed config" >&2
    exit 1
  fi
  grep -q 'frontend flag verification:' "$work_dir/error"
done
for percent in 0 100; do
  expected=false
  [ "$percent" -eq 0 ] || expected=true
  printf 'plugins_v1:\n  default: %s\n  percent:\n    percent: %s\n' "$([ "$expected" = true ] && echo false || echo true)" "$percent" >"$flags_file"
  node deploy/cd/flagfile-check.mjs "$flags_file" --config-stdin <<<"{\"feature_flags\":{\"plugins_v1\":$expected}}"
done
printf 'plugins_v1:\n  allow: [internal-user]\n  default: false\n' >"$flags_file"
node deploy/cd/flagfile-check.mjs "$flags_file" --config-stdin <<<'{"feature_flags":{"plugins_v1":false}}'
printf 'plugins_v1:\n  deny: [internal-user]\n  default: true\n' >"$flags_file"
node deploy/cd/flagfile-check.mjs "$flags_file" --config-stdin <<<'{"feature_flags":{"plugins_v1":true}}'
printf 'plugins_v1:\n  percent:\n    percent: 50\n' >"$flags_file"
node deploy/cd/flagfile-check.mjs "$flags_file" --config-stdin <<<'{"feature_flags":{"plugins_v1":true}}'
printf 'project_workflows_v1:\n' >"$flags_file"
node deploy/cd/flagfile-check.mjs "$flags_file" --config-stdin <<<'{"feature_flags":{"project_workflows_v1":false}}'

echo "flagfile-check fixtures passed"
