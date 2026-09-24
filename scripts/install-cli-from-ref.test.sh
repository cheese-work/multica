#!/usr/bin/env bash
set -euo pipefail

# Regression coverage for CHE-765/CHE-745: there was no automated path from a
# merged commit on a fork (cheese-work/multica) to an authenticated daemon
# host's ~/.local/bin/multica — CD only ever updates C00's container. This
# builds from a real local git repo (a minimal `cmd/multica` + `go.mod` that
# only depends on the standard library) at a known commit and installs it,
# so both real `git` and real `go build` are exercised rather than mocked.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$root_dir/scripts/install-cli-from-ref.sh"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

# ---------------------------------------------------------------------------
# A minimal standalone module standing in for server/cmd/multica: only
# `version --output json` needs to exist for install-cli-from-ref.sh's own
# verification step to run against something real.
# ---------------------------------------------------------------------------
fixture_repo="$work_dir/fixture-repo"
mkdir -p "$fixture_repo"
git -C "$fixture_repo" init --quiet -b main
git -C "$fixture_repo" config user.email test@example.com
git -C "$fixture_repo" config user.name "Test"

cat >"$fixture_repo/go.mod" <<'EOF'
module fixture

go 1.21
EOF

mkdir -p "$fixture_repo/server/cmd/multica"
cat >"$fixture_repo/server/cmd/multica/main.go" <<'EOF'
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

var version = "dev"
var commit = "unknown"
var date = "unknown"

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		enc := json.NewEncoder(os.Stdout)
		_ = enc.Encode(map[string]string{"version": version, "commit": commit, "date": date})
		return
	}
	fmt.Println("fixture multica")
}
EOF

git -C "$fixture_repo" add -A
git -C "$fixture_repo" commit --quiet -m "fixture commit"
fixture_sha="$(git -C "$fixture_repo" rev-parse HEAD)"

# ---------------------------------------------------------------------------
# Scenario: builds from the exact ref and installs, verifying the installed
# binary's own reported commit matches what was requested.
# ---------------------------------------------------------------------------
bin_dir="$work_dir/bin"
output="$(bash "$script" --repo "$fixture_repo" --ref "$fixture_sha" --bin-dir "$bin_dir" 2>&1)"
status=$?

if [ "$status" -ne 0 ]; then
  echo "scenario happy-path: expected exit 0, got $status" >&2
  echo "$output" >&2
  exit 1
fi

if [ ! -x "$bin_dir/multica" ]; then
  echo "scenario happy-path: $bin_dir/multica was not installed" >&2
  exit 1
fi

if [[ "$output" != *"${fixture_sha:0:9}"* ]]; then
  echo "scenario happy-path: expected output to report commit ${fixture_sha:0:9}, got:" >&2
  echo "$output" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario: an unknown ref must fail loudly, not install a stale/wrong binary.
# ---------------------------------------------------------------------------
bin_dir2="$work_dir/bin2"
set +e
bad_output="$(bash "$script" --repo "$fixture_repo" --ref does-not-exist --bin-dir "$bin_dir2" 2>&1)"
bad_status=$?
set -e

if [ "$bad_status" -eq 0 ]; then
  echo "scenario bad-ref: expected nonzero exit for an unknown ref, got 0" >&2
  echo "$bad_output" >&2
  exit 1
fi

if [ -e "$bin_dir2/multica" ]; then
  echo "scenario bad-ref: must not install a binary when the ref cannot be resolved" >&2
  exit 1
fi

echo "install-cli-from-ref.sh fixtures passed"
