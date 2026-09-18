#!/usr/bin/env bash
# Fixture checks for the path-filter reader that feeds the admission evidence
# set. The failure this guards against is not a wrong filter but an empty one:
# a runner without PyYAML once produced `{}`, which made `mobile` look
# unconditionally required and refused a deploy whose commit touched no mobile
# path.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

bash deploy/cd/ensure-pyyaml.sh

# `mobile` is the path-filtered required check admission.mjs has to excuse, so
# its filter must be derivable from the workflow that declares it.
filters="$tmp_dir/path-filters.json"
python3 deploy/cd/workflow-path-filters.py >"$filters"
node -e '
  const filters = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
  const mobile = filters.mobile;
  if (!Array.isArray(mobile) || mobile.length === 0) {
    throw new Error(`mobile path filter missing from ${JSON.stringify(filters)}`);
  }
  for (const glob of ["apps/mobile/**", "packages/core/**"]) {
    if (!mobile.includes(glob)) throw new Error(`mobile filter lost ${glob}`);
  }
' "$filters"

# Without PyYAML the script must fail loudly at the step that owns the
# dependency, rather than emitting an empty object that only explains itself
# three jobs later. A stub that raises on import stands in for its absence.
printf 'raise ImportError("simulated missing PyYAML")\n' >"$tmp_dir/yaml.py"
status=0
PYTHONPATH="$tmp_dir" python3 deploy/cd/workflow-path-filters.py \
  >"$tmp_dir/out.json" 2>"$tmp_dir/err.txt" || status=$?
test "$status" -ne 0 || {
  echo "expected a non-zero exit without PyYAML, got 0: $(cat "$tmp_dir/out.json")" >&2
  exit 1
}
grep -q "requires PyYAML" "$tmp_dir/err.txt" || {
  echo "expected the missing-PyYAML message on stderr, got: $(cat "$tmp_dir/err.txt")" >&2
  exit 1
}

echo "workflow path-filter fixtures passed"
