#!/usr/bin/env bash
set -euo pipefail

# Regression coverage for the CHE-2641 upstream-update-source guard: one
# violating fixture must fail with the offending file named; allowlisted and
# test-only hits must pass; the real tree must be green.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
guard="$root_dir/scripts/check-no-upstream-update-source.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

mk() { mkdir -p "$work/$(dirname "$1")"; printf '%s\n' "$2" >"$work/$1"; }

# Clean tree: Go import path and a plain server URL are not violations.
mk server/internal/ok.go 'import "github.com/multica-ai/multica/server/internal/x"'
mk packages/core/ok.ts 'const u = "/api/daemon/install.sh";'
CHECK_ROOT="$work" bash "$guard" >/dev/null || fail "clean tree must pass"

# Test files and docs may name the upstream URLs.
mk packages/core/thing.test.ts 'const u = "https://github.com/multica-ai/multica/releases/latest";'
mk server/internal/cli/x_test.go 'const u = "https://github.com/multica-ai/multica/releases/latest"'
mk apps/docs/content/a.mdx 'curl https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh'
CHECK_ROOT="$work" bash "$guard" >/dev/null || fail "tests and docs must pass"

# Violations: each pattern in each scanned extension fails and is named.
for case in \
  'apps/desktop/src/main/a.ts|https://github.com/multica-ai/multica/releases/latest/download' \
  'packages/views/b.tsx|https://raw.githubusercontent.com/multica-ai/multica/main/scripts/install.sh' \
  'server/internal/c.go|https://api.github.com/repos/multica-ai/multica/releases/latest'; do
  file="${case%%|*}"; line="${case#*|}"
  mk "$file" "const u = \"$line\""
  if out="$(CHECK_ROOT="$work" bash "$guard" 2>&1)"; then
    fail "violation in $file must fail"
  fi
  grep -q "$file" <<<"$out" || fail "output must name $file, got: $out"
  rm "$work/$file"
done

# The real tree is green.
bash "$guard" >/dev/null || fail "repository tree must pass"

echo "check-no-upstream-update-source.test.sh: ok"
