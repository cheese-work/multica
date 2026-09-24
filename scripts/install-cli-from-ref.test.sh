#!/usr/bin/env bash
set -euo pipefail

# Regression coverage for CHE-765/CHE-745: there was no automated path from a
# merged commit on a fork (cheese-work/multica) to an authenticated daemon
# host's ~/.local/bin/multica — CD only ever updates C00's container. This
# builds from a real local git repo (a minimal `cmd/multica` + `go.mod` that
# only depends on the standard library) at a known commit and installs it,
# so both real `git` and real `go build` are exercised rather than mocked.
#
# Also covers the independent-review blockers from PR #110 (CHE-765): a full
# 40-char commit SHA is required (not a mutable ref), missing prerequisites
# fail closed with a named diagnostic, and a bad build/staged binary never
# displaces an already-installed good one (stage, verify, THEN atomic
# replace).

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$root_dir/scripts/install-cli-from-ref.sh"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

write_fixture_main() {
  local dest="$1" commit_literal="$2"
  cat >"$dest" <<EOF
package main

import (
	"encoding/json"
	"fmt"
	"os"
)

var version = "dev"
var commit = "$commit_literal"
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
}

# ---------------------------------------------------------------------------
# A minimal standalone module standing in for server/cmd/multica: only
# `version --output json` needs to exist for install-cli-from-ref.sh's own
# verification step to run against something real.
# ---------------------------------------------------------------------------
fixture_repo="$work_dir/fixture-repo"
mkdir -p "$fixture_repo/server/cmd/multica"
git -C "$fixture_repo" init --quiet -b main
git -C "$fixture_repo" config user.email test@example.com
git -C "$fixture_repo" config user.name "Test"

cat >"$fixture_repo/go.mod" <<'EOF'
module fixture

go 1.21
EOF

write_fixture_main "$fixture_repo/server/cmd/multica/main.go" "unknown"
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
# Scenario: a short or symbolic ref must be rejected before any clone/build
# work — a mutable ref (branch, tag, short SHA) can resolve to a different
# commit than the one an operator actually reviewed and qualified.
# ---------------------------------------------------------------------------
bin_dir_short="$work_dir/bin-short-ref"
set +e
short_output="$(bash "$script" --repo "$fixture_repo" --ref "${fixture_sha:0:9}" --bin-dir "$bin_dir_short" 2>&1)"
short_status=$?
set -e

if [ "$short_status" -eq 0 ]; then
  echo "scenario short-ref: expected nonzero exit for a short SHA, got 0" >&2
  echo "$short_output" >&2
  exit 1
fi
if [[ "$short_output" != *"40-character"* ]]; then
  echo "scenario short-ref: expected a 40-character-SHA diagnostic, got:" >&2
  echo "$short_output" >&2
  exit 1
fi
if [ -e "$bin_dir_short/multica" ]; then
  echo "scenario short-ref: must not install anything for a rejected ref" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario: a well-formed but nonexistent full SHA must fail closed at
# checkout, not install a stale/wrong binary.
# ---------------------------------------------------------------------------
bin_dir_missing="$work_dir/bin-missing-ref"
missing_sha="0000000000000000000000000000000000000000"
set +e
missing_output="$(bash "$script" --repo "$fixture_repo" --ref "$missing_sha" --bin-dir "$bin_dir_missing" 2>&1)"
missing_status=$?
set -e

if [ "$missing_status" -eq 0 ]; then
  echo "scenario missing-ref: expected nonzero exit for a nonexistent SHA, got 0" >&2
  echo "$missing_output" >&2
  exit 1
fi
if [ -e "$bin_dir_missing/multica" ]; then
  echo "scenario missing-ref: must not install a binary when the ref cannot be resolved" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario: missing prerequisites fail closed with a named diagnostic before
# any clone/build work, instead of failing deep inside git/go with a
# confusing error.
# ---------------------------------------------------------------------------
stub_path_dir="$work_dir/stub-path"
mkdir -p "$stub_path_dir"
for tool in go node; do
  # Put every real tool except the one under test on a stub PATH, so the
  # missing-prerequisite check is what actually fires, not a real absence
  # of git/coreutils this sandbox also needs for its own bookkeeping.
  for real in git go node awk grep sed sort tr mktemp cp mv mkdir chmod date rm cat basename dirname head bash env; do
    [ "$real" = "$tool" ] && continue
    real_path="$(command -v "$real" 2>/dev/null || true)"
    [ -n "$real_path" ] && ln -sf "$real_path" "$stub_path_dir/$real"
  done
  set +e
  missing_tool_output="$(PATH="$stub_path_dir" bash "$script" --repo "$fixture_repo" --ref "$fixture_sha" --bin-dir "$work_dir/bin-$tool" 2>&1)"
  missing_tool_status=$?
  set -e
  rm -rf "$stub_path_dir"
  mkdir -p "$stub_path_dir"

  if [ "$missing_tool_status" -eq 0 ]; then
    echo "scenario missing-$tool: expected nonzero exit when $tool is absent, got 0" >&2
    echo "$missing_tool_output" >&2
    exit 1
  fi
  if [[ "$missing_tool_output" != *"$tool"* ]]; then
    echo "scenario missing-$tool: expected the diagnostic to name '$tool', got:" >&2
    echo "$missing_tool_output" >&2
    exit 1
  fi
done
rm -rf "$stub_path_dir"

# ---------------------------------------------------------------------------
# Scenario: stage-verify-then-atomic-replace. A good binary is installed
# first; a second, incompatible ref (whose `version` command cannot be
# parsed the same way) must fail the staged-binary verification and leave
# the already-installed good binary completely untouched — never partially
# replaced, never deleted before its replacement is proven good.
# ---------------------------------------------------------------------------
retain_bin_dir="$work_dir/bin-retain"
# Reuses the initial fixture commit as "good" — its main.go already matches
# write_fixture_main's default content, so a redundant identical commit here
# would fail with "nothing to commit".
good_sha="$fixture_sha"

bash "$script" --repo "$fixture_repo" --ref "$good_sha" --bin-dir "$retain_bin_dir" >/dev/null 2>&1
good_installed_commit="$("$retain_bin_dir/multica" version --output json | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>process.stdout.write(JSON.parse(d).commit))')"
good_mtime="$(stat -c %Y "$retain_bin_dir/multica" 2>/dev/null || stat -f %m "$retain_bin_dir/multica")"

# An incompatible commit: `version` no longer emits JSON at all, simulating
# a build whose command surface changed under it (or a build artifact from
# the wrong package entirely) — install-cli-from-ref.sh's own staged-binary
# verification must catch this before touching --bin-dir.
cat >"$fixture_repo/server/cmd/multica/main.go" <<'EOF'
package main

import "fmt"

func main() { fmt.Println("this build predates --output json support") }
EOF
git -C "$fixture_repo" add -A
git -C "$fixture_repo" commit --quiet -m "incompatible commit"
incompatible_sha="$(git -C "$fixture_repo" rev-parse HEAD)"

set +e
retain_output="$(bash "$script" --repo "$fixture_repo" --ref "$incompatible_sha" --bin-dir "$retain_bin_dir" 2>&1)"
retain_status=$?
set -e

if [ "$retain_status" -eq 0 ]; then
  echo "scenario atomic-replace-retention: expected nonzero exit for an incompatible staged binary, got 0" >&2
  echo "$retain_output" >&2
  exit 1
fi

after_commit="$("$retain_bin_dir/multica" version --output json | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>process.stdout.write(JSON.parse(d).commit))')"
if [ "$after_commit" != "$good_installed_commit" ]; then
  echo "scenario atomic-replace-retention: expected the previous binary (commit $good_installed_commit) to survive a failed replace, found commit $after_commit" >&2
  exit 1
fi

after_mtime="$(stat -c %Y "$retain_bin_dir/multica" 2>/dev/null || stat -f %m "$retain_bin_dir/multica")"
if [ "$after_mtime" != "$good_mtime" ]; then
  echo "scenario atomic-replace-retention: the installed binary's mtime changed even though the replace failed — it was touched, not left alone" >&2
  exit 1
fi

leftover="$(find "$retain_bin_dir" -maxdepth 1 -name '.multica.new.*' 2>/dev/null)"
if [ -n "$leftover" ]; then
  echo "scenario atomic-replace-retention: a staging temp file was left behind: $leftover" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario: post-replace exec failure (noexec-equivalent). The scenario above
# only exercises a bad build being caught BEFORE the atomic replace, at the
# staged copy in $work_dir. A real noexec target filesystem, wrong
# architecture, or truncated copy can only be observed by executing the
# binary from its FINAL installed path, which staging structurally cannot
# do (staging never runs from $bin_dir). Creating a real noexec mount needs
# root/mount-namespace privileges this sandbox and many CI runners don't
# grant, so this fixture simulates the same failure class deterministically:
# the binary itself detects it is running from a directory literally named
# "bin-postreplace" and refuses to execute only then — succeeding identically
# to every other scenario in this file everywhere else, including when
# staged in $work_dir. This is fault injection at the same observable
# boundary (does `$bin_dir/multica version` succeed?), not a weaker proxy for
# it.
# ---------------------------------------------------------------------------
postreplace_bin_dir="$work_dir/bin-postreplace"
bash "$script" --repo "$fixture_repo" --ref "$good_sha" --bin-dir "$postreplace_bin_dir" >/dev/null 2>&1
postreplace_good_commit="$("$postreplace_bin_dir/multica" version --output json | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>process.stdout.write(JSON.parse(d).commit))')"
postreplace_good_mtime="$(stat -c %Y "$postreplace_bin_dir/multica" 2>/dev/null || stat -f %m "$postreplace_bin_dir/multica")"

cat >"$fixture_repo/server/cmd/multica/main.go" <<'EOF'
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var version = "dev"
var commit = "unknown"
var date = "unknown"

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "version" {
		if strings.Contains(filepath.Base(filepath.Dir(os.Args[0])), "bin-postreplace") {
			fmt.Fprintln(os.Stderr, "simulated post-replace exec failure (stands in for noexec/wrong-arch/truncated-copy)")
			os.Exit(1)
		}
		enc := json.NewEncoder(os.Stdout)
		_ = enc.Encode(map[string]string{"version": version, "commit": commit, "date": date})
		return
	}
	fmt.Println("fixture multica")
}
EOF
git -C "$fixture_repo" add -A
git -C "$fixture_repo" commit --quiet -m "commit that only fails once installed to bin-postreplace"
postreplace_bad_sha="$(git -C "$fixture_repo" rev-parse HEAD)"

set +e
postreplace_output="$(bash "$script" --repo "$fixture_repo" --ref "$postreplace_bad_sha" --bin-dir "$postreplace_bin_dir" 2>&1)"
postreplace_status=$?
set -e

if [ "$postreplace_status" -eq 0 ]; then
  echo "scenario post-replace-exec-failure: expected nonzero exit when the installed binary fails to execute, got 0" >&2
  echo "$postreplace_output" >&2
  exit 1
fi
if [[ "$postreplace_output" != *"restored the previous binary"* ]]; then
  echo "scenario post-replace-exec-failure: expected an explicit restore message, got:" >&2
  echo "$postreplace_output" >&2
  exit 1
fi

postreplace_after_commit="$("$postreplace_bin_dir/multica" version --output json | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>process.stdout.write(JSON.parse(d).commit))')"
if [ "$postreplace_after_commit" != "$postreplace_good_commit" ]; then
  echo "scenario post-replace-exec-failure: expected the previous binary (commit $postreplace_good_commit) to be restored, found commit $postreplace_after_commit" >&2
  exit 1
fi

# `cp -p` preserves mtime, so the backup carries the original install's
# mtime through the whole backup -> failed-replace -> restore cycle: the
# restored file's mtime matching the original is proof restore ran and
# restored a byte-identical copy, not evidence it didn't. It does not, on
# its own, distinguish "restored" from "the original file, never touched" —
# the commit check above and the "restored the previous binary" message
# check together already establish that a replace attempt happened; this
# assertion only additionally confirms the restore reproduced the original
# exactly, rather than e.g. a fresh build that happens to match the commit.
postreplace_after_mtime="$(stat -c %Y "$postreplace_bin_dir/multica" 2>/dev/null || stat -f %m "$postreplace_bin_dir/multica")"
if [ "$postreplace_after_mtime" != "$postreplace_good_mtime" ]; then
  echo "scenario post-replace-exec-failure: expected the restored binary's mtime ($postreplace_good_mtime) to match the original install, got $postreplace_after_mtime" >&2
  exit 1
fi

postreplace_leftover="$(find "$postreplace_bin_dir" -maxdepth 1 -name '.multica.new.*' 2>/dev/null)"
if [ -n "$postreplace_leftover" ]; then
  echo "scenario post-replace-exec-failure: a staging temp file was left behind: $postreplace_leftover" >&2
  exit 1
fi

if [ ! -x "$postreplace_bin_dir/multica" ]; then
  echo "scenario post-replace-exec-failure: the restored binary must remain executable" >&2
  exit 1
fi

echo "install-cli-from-ref.sh fixtures passed"
