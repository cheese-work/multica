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
# Scenario: the durable backup survives a SUCCESSFUL install, not just a
# failed one. A previous review round found the backup staged under
# $work_dir, which the script's own EXIT trap deletes unconditionally —
# including on success — so a successful upgrade silently destroyed its own
# rollback packet. The backup must live at --bin-dir itself and remain after
# a clean install exits 0.
# ---------------------------------------------------------------------------
retention_across_success_dir="$work_dir/bin-retention-across-success"
bash "$script" --repo "$fixture_repo" --ref "$good_sha" --bin-dir "$retention_across_success_dir" >/dev/null 2>&1
if [ -e "$retention_across_success_dir/.multica.previous" ]; then
  echo "scenario retention-across-success: expected no backup after the FIRST install (nothing existed to back up), found one" >&2
  exit 1
fi

second_good_sha="$good_sha"
# A second install of the identical commit still exercises a real
# backup-then-restore-eligible cycle (the script does not special-case
# "same commit already installed"), and confirms the backup left behind is a
# byte-faithful copy of what was actually running, not a placeholder.
before_second_commit="$("$retention_across_success_dir/multica" version --output json | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>process.stdout.write(JSON.parse(d).commit))')"
bash "$script" --repo "$fixture_repo" --ref "$second_good_sha" --bin-dir "$retention_across_success_dir" >/dev/null 2>&1

if [ ! -e "$retention_across_success_dir/.multica.previous" ]; then
  echo "scenario retention-across-success: expected the previous binary to remain as a durable backup after a SUCCESSFUL second install, found none" >&2
  exit 1
fi

backup_commit="$("$retention_across_success_dir/.multica.previous" version --output json | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>process.stdout.write(JSON.parse(d).commit))')"
if [ "$backup_commit" != "$before_second_commit" ]; then
  echo "scenario retention-across-success: expected the retained backup to report the commit that was running before this install ($before_second_commit), got $backup_commit" >&2
  exit 1
fi

if [ ! -x "$retention_across_success_dir/.multica.previous" ]; then
  echo "scenario retention-across-success: the retained backup must remain executable, not just present" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario: an existing backup must survive a failure to CREATE the new
# backup. A previous review round found the script deleting the existing
# .multica.previous up front and only then copying its replacement — if that
# copy failed (disk full, permission error, killed mid-write), the one
# durable rollback artifact was already gone, exactly the failure this
# script exists to protect against. Injects a copy failure via a `cp` stub
# on PATH that fails ONLY for the backup's own staging filename
# (.multica.previous.new.<pid>) and behaves as the real `cp` for every other
# call the script makes (staging the new binary, etc.), so only the one
# operation under test is affected.
# ---------------------------------------------------------------------------
backup_copy_failure_dir="$work_dir/bin-backup-copy-failure"
bash "$script" --repo "$fixture_repo" --ref "$good_sha" --bin-dir "$backup_copy_failure_dir" >/dev/null 2>&1
existing_backup_dir="$work_dir/bin-backup-copy-failure-seed"
mkdir -p "$existing_backup_dir"
# Seed an existing backup by hand: content distinguishable from both the
# currently-installed binary and whatever this run would otherwise produce,
# so "the existing backup was preserved byte-for-byte" is unambiguous rather
# than coincidentally matching a fresh copy.
printf '#!/bin/sh\necho seeded-existing-backup\n' >"$backup_copy_failure_dir/.multica.previous"
chmod +x "$backup_copy_failure_dir/.multica.previous"
seeded_backup_checksum="$(sha256sum "$backup_copy_failure_dir/.multica.previous" 2>/dev/null || shasum -a 256 "$backup_copy_failure_dir/.multica.previous")"

cp_stub_dir="$work_dir/cp-stub"
mkdir -p "$cp_stub_dir"
real_cp="$(command -v cp)"
cat >"$cp_stub_dir/cp" <<EOF
#!/usr/bin/env bash
for arg in "\$@"; do
  case "\$arg" in
    *.multica.previous.new.*)
      echo "stub cp: simulated write failure staging a new backup: \$arg" >&2
      exit 1
      ;;
  esac
done
exec "$real_cp" "\$@"
EOF
chmod +x "$cp_stub_dir/cp"

set +e
backup_copy_failure_output="$(PATH="$cp_stub_dir:$PATH" bash "$script" --repo "$fixture_repo" --ref "$good_sha" --bin-dir "$backup_copy_failure_dir" 2>&1)"
set -e

if [[ "$backup_copy_failure_output" != *"could not stage a new backup"* ]]; then
  echo "scenario backup-copy-failure: expected a diagnostic naming the failed backup staging, got:" >&2
  echo "$backup_copy_failure_output" >&2
  exit 1
fi

if [ ! -e "$backup_copy_failure_dir/.multica.previous" ]; then
  echo "scenario backup-copy-failure: the existing backup must survive a failed backup-refresh attempt, found none" >&2
  exit 1
fi

after_backup_checksum="$(sha256sum "$backup_copy_failure_dir/.multica.previous" 2>/dev/null || shasum -a 256 "$backup_copy_failure_dir/.multica.previous")"
if [ "$after_backup_checksum" != "$seeded_backup_checksum" ]; then
  echo "scenario backup-copy-failure: the seeded backup was modified even though its own copy step failed" >&2
  exit 1
fi

if [ ! -x "$backup_copy_failure_dir/.multica.previous" ]; then
  echo "scenario backup-copy-failure: the surviving backup must remain executable/usable, not just present" >&2
  exit 1
fi

backup_copy_failure_leftover="$(find "$backup_copy_failure_dir" -maxdepth 1 -name '.multica.previous.new.*' 2>/dev/null)"
if [ -n "$backup_copy_failure_leftover" ]; then
  echo "scenario backup-copy-failure: a failed backup-staging temp file was left behind: $backup_copy_failure_leftover" >&2
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

# restore_previous renames the backup back onto the live path (mv, not a
# copy that leaves the source behind) -- after a successful restore, the
# backup file itself must be gone: the live binary IS the former backup now,
# not a separate copy of it.
if [ -e "$postreplace_bin_dir/.multica.previous" ]; then
  echo "scenario post-replace-exec-failure: expected the backup to be consumed by the restore (renamed, not copied), but .multica.previous still exists" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Static check: the restore path must use a same-filesystem rename, never an
# in-place copy onto the live binary. A `cp` overwrites the target's bytes
# while a concurrent reader (or the daemon that execs this exact path) could
# still have it open, risking a partial read; `mv`/rename(2) instead swaps
# the directory entry as a single atomic operation, so a reader always sees
# either the complete old file or the complete new one. This is checked
# directly against the script source because the failure mode a `cp`-based
# restore risks -- a reader observing a torn write -- is a race that cannot
# be forced to reproduce deterministically in a portable test; asserting the
# primitive itself is the reliable half of this property this harness can
# exercise. The dynamic scenario above (post-replace-exec-failure) already
# proves restore_previous's OBSERABLE effect (right commit, right mtime, old
# backup consumed); this proves it uses the atomic primitive to get there.
restore_previous_body="$(awk '/^restore_previous\(\) \{/,/^\}/' "$script")"
if [[ "$restore_previous_body" != *'mv -f "$backup_binary" "$bin_dir/multica"'* ]]; then
  echo 'scenario restore-uses-atomic-rename: expected restore_previous() to restore via mv -f "$backup_binary" "$bin_dir/multica", got:' >&2
  echo "$restore_previous_body" >&2
  exit 1
fi
if [[ "$restore_previous_body" == *'cp -p "$backup_binary" "$bin_dir/multica"'* ]] || [[ "$restore_previous_body" == *'cp "$backup_binary" "$bin_dir/multica"'* ]]; then
  echo "scenario restore-uses-atomic-rename: restore_previous() must not cp onto the live binary path" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Scenario: concurrent-reader boundary, as far as a portable harness can
# exercise it. A background loop continuously execs the installed binary
# throughout an entire install-then-post-replace-failure-then-restore cycle;
# every single invocation must either fully succeed (valid JSON, matching
# one of the two commits that were ever legitimately installed at this path)
# or the binary must be transiently absent between the atomic replace and
# the restore -- never truncated output, a corrupt exec, or content from
# neither commit. A real forced interleaving (pausing the writer mid-`mv`)
# is not possible from outside the process without a debugger; this proves
# the property observable from a well-behaved reader's side, which is what
# an actual daemon holding this path open would experience.
# ---------------------------------------------------------------------------
# Named with the same "bin-postreplace" substring the shared fixture main.go
# (written above) keys its fault injection on -- this scenario's bad build IS
# that same fixture, and needs the same trigger to actually fail here.
concurrent_bin_dir="$work_dir/bin-postreplace-concurrent"
bash "$script" --repo "$fixture_repo" --ref "$good_sha" --bin-dir "$concurrent_bin_dir" >/dev/null 2>&1
concurrent_good_commit="$("$concurrent_bin_dir/multica" version --output json | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>process.stdout.write(JSON.parse(d).commit))')"

reader_log="$work_dir/concurrent-reader.log"
: >"$reader_log"
reader_stop="$work_dir/concurrent-reader.stop"
rm -f "$reader_stop"
(
  while [ ! -e "$reader_stop" ]; do
    out="$("$concurrent_bin_dir/multica" version --output json 2>/dev/null)"
    status=$?
    if [ "$status" -eq 0 ]; then
      commit="$(printf '%s' "$out" | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>{try{process.stdout.write(JSON.parse(d).commit||"")}catch{process.stdout.write("PARSE_ERROR")}})' 2>/dev/null)"
      if [ "$commit" != "$concurrent_good_commit" ] && [[ "$postreplace_bad_sha" != "$commit"* ]] && [ -n "$commit" ]; then
        echo "UNEXPECTED_COMMIT:$commit" >>"$reader_log"
      fi
      if [ "$commit" = "PARSE_ERROR" ]; then
        echo "TORN_READ:$out" >>"$reader_log"
      fi
    fi
    # A nonzero exit is acceptable ONLY in the narrow window between the
    # atomic replace and this script's own post-replace verification+
    # restore -- not logged as a failure here, since "briefly absent or
    # mid-transition" is expected; the assertions below instead check that
    # the FINAL state afterward is fully consistent.
  done
) &
reader_pid=$!

set +e
concurrent_output="$(bash "$script" --repo "$fixture_repo" --ref "$postreplace_bad_sha" --bin-dir "$concurrent_bin_dir" 2>&1)"
set -e

touch "$reader_stop"
wait "$reader_pid" 2>/dev/null || true

if [ -s "$reader_log" ]; then
  echo "scenario concurrent-reader: a concurrent reader observed corrupt or unexpected content during install+restore:" >&2
  cat "$reader_log" >&2
  exit 1
fi

concurrent_final_commit="$("$concurrent_bin_dir/multica" version --output json | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>process.stdout.write(JSON.parse(d).commit))')"
if [ "$concurrent_final_commit" != "$concurrent_good_commit" ]; then
  echo "scenario concurrent-reader: expected the final state to be the restored good commit ($concurrent_good_commit), got $concurrent_final_commit" >&2
  echo "$concurrent_output" >&2
  exit 1
fi

echo "install-cli-from-ref.sh fixtures passed"
