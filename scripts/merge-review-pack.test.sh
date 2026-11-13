#!/usr/bin/env bash
set -euo pipefail

# Regression coverage for scripts/merge-review-pack (CHE-5577): builds fixture
# upstream merges in a scratch repo and checks the pinned parents, the
# reproducible baseline B, the resolution edits (git diff B M), the incoming
# delta report, the class checks and every fail-closed exit.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pack="$root_dir/scripts/merge-review-pack"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
has() { grep -qF -- "$2" <<<"$1" || fail "$3: missing '$2' in: $1"; }
hasnt() { if grep -qF -- "$2" <<<"$1"; then fail "$3: unexpected '$2' in: $1"; fi; }

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

repo="$work/repo"
git init -q -b main "$repo"
cd "$repo"
mkdir -p server/migrations server/internal/handler server/internal/auth .github/workflows
printf 'a\nb\nc\n' >shared.txt
printf 'x\n' >deleted-upstream.txt
printf 'CREATE;\n' >server/migrations/001_init.up.sql
printf 'DROP;\n' >server/migrations/001_init.down.sql
printf '{\n  "dependencies": {\n    "left": "1.0.0"\n  }\n}\n' >package.json
git add -A; git commit -qm base
BASE="$(git rev-parse HEAD)"

# Fork side (P): own migration 002, edits shared.txt line b, fork-only file.
git checkout -q -b fork
printf 'a\nFORK\nc\n' >shared.txt
printf 'CREATE fork;\n' >server/migrations/002_fork.up.sql
printf 'DROP fork;\n' >server/migrations/002_fork.down.sql
printf 'fork\n' >fork-only.txt
printf 'fork edit\n' >deleted-upstream.txt
git add -A; git commit -qm fork
P="$(git rev-parse HEAD)"

# Upstream side (U): edits the same line, migration 002 (collision), a workflow,
# an auth file, a new route, a new env read and a new dependency.
git checkout -q -b upstream "$BASE"
printf 'a\nUP\nc\n' >shared.txt
printf 'CREATE up;\n' >server/migrations/002_upstream.up.sql
printf 'DROP up;\n' >server/migrations/002_upstream.down.sql
printf 'name: x\n' >.github/workflows/new.yml
printf 'package auth\n' >server/internal/auth/token.go
printf 'func f(r R) { r.Get("/api/new-endpoint", h); _ = os.Getenv("NEW_SECRET") }\n' >server/internal/handler/new.go
printf '{\n  "dependencies": {\n    "left": "1.0.0",\n    "brand-new": "^2.0.0"\n  }\n}\n' >package.json
git rm -q deleted-upstream.txt
git add -A; git commit -qm upstream
U="$(git rev-parse HEAD)"

# M1: merge with the conflict resolved and the colliding migration renumbered.
git checkout -q fork
git merge -q --no-commit --no-ff "$U" >/dev/null 2>&1 || true
printf 'a\nFORK\nUP\nc\n' >shared.txt
git add shared.txt deleted-upstream.txt
git mv server/migrations/002_upstream.up.sql server/migrations/003_upstream.up.sql
git mv server/migrations/002_upstream.down.sql server/migrations/003_upstream.down.sql
git add -A
git commit -qm "merge upstream"
M1="$(git rev-parse HEAD)"

out="$(bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M1" 2>&1)" \
  || fail "valid pack must exit 0, got: $out"

# Pins and baseline.
has "$out" "P $P" "pins"
has "$out" "U $U" "pins"
has "$out" "M $M1" "pins"
has "$out" "Baseline B" "baseline"
has "$out" "shared.txt" "conflicted file named"
# Resolution edits: the shared.txt resolution and the renumber must show up.
has "$out" "-<<<<<<<" "resolution diff drops the conflict markers"
has "$out" "003_upstream" "resolution diff shows renumber"
# Incoming delta: whole delta by file and the four minimum classes.
has "$out" "server/internal/auth/token.go" "incoming delta file"
has "$out" "new-endpoint" "new endpoint"
has "$out" "NEW_SECRET" "new config"
has "$out" "brand-new" "new dependency"
has "$out" ".github/workflows/new.yml" "workflow class"
has "$out" "server/internal/auth/token.go" "trust-boundary class"
has "$out" "shared.txt" "fork overlap class"
# Migration number collision between U and P is reported (002 on both sides).
has "$out" "COLLISION" "migration collision"
has "$out" "002" "migration collision number"
# Checks section tells the reviewer what is still owed.
has "$out" "migration-prefix-check" "migration lint named"
has "$out" "NOT CHECKED" "required checks not queried without --github-repo"

# Same bytes twice: the pack is deterministic.
out2="$(bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M1" 2>&1)"
[ "$out" = "$out2" ] || fail "pack must be deterministic"

# M2: independent fork edit hidden in the merge shows in git diff B M.
git checkout -q -b m2 "$M1"
printf 'sneaky\n' >sneaky.txt
git add -A; git commit -q --amend -m "merge upstream (extra)" 
M2="$(git rev-parse HEAD)"
out="$(bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M2" 2>&1)" \
  || fail "valid-parent pack must exit 0, got: $out"
has "$out" "sneaky.txt" "extra edit visible in resolution diff"

# Rejections: an extra parent, a wrong parent and an unknown commit.
EXTRA="$(git commit-tree "$(git rev-parse "$M1^{tree}")" -p "$P" -p "$U" -p "$BASE" -m octopus)"
set +e
out="$(bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$EXTRA" 2>&1)"; rc=$?
set -e
[ "$rc" = 2 ] || fail "extra parent must exit 2, got $rc: $out"
has "$out" "parents" "extra parent message"

set +e
out="$(bash "$pack" --fork-parent "$BASE" --upstream-parent "$U" --merge "$M1" 2>&1)"; rc=$?
set -e
[ "$rc" = 2 ] || fail "wrong parent must exit 2, got $rc: $out"

set +e
out="$(bash "$pack" --fork-parent "$P" --upstream-parent 0123456789012345678901234567890123456789 --merge "$M1" 2>&1)"; rc=$?
set -e
[ "$rc" = 2 ] || fail "unknown commit must exit 2, got $rc: $out"

# Short SHAs are not pins.
set +e
out="$(bash "$pack" --fork-parent "${P:0:8}" --upstream-parent "$U" --merge "$M1" 2>&1)"; rc=$?
set -e
[ "$rc" = 2 ] || fail "short SHA must exit 2, got $rc: $out"
has "$out" "40-hex" "short SHA message"

# Provenance: U must be an ancestor of the fetched upstream ref.
up="$work/upstream.git"
git clone -q --bare "$repo" "$up"
git -C "$up" update-ref refs/heads/main "$U"
out="$(bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M1" \
  --upstream-url "$up" --upstream-ref main 2>&1)" || fail "provenance ok must exit 0: $out"
has "$out" "Provenance: OK" "provenance ok"
git -C "$up" update-ref refs/heads/main "$BASE"
set +e
out="$(bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M1" \
  --upstream-url "$up" --upstream-ref main 2>&1)"; rc=$?
set -e
[ "$rc" = 4 ] || fail "U not in fetched ref must exit 4, got $rc: $out"
# A tag must equal U exactly (no ancestry shortcut).
git -C "$up" update-ref refs/tags/v9 "$BASE"
set +e
out="$(bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M1" \
  --upstream-url "$up" --upstream-ref v9 2>&1)"; rc=$?
set -e
[ "$rc" = 4 ] || fail "tag != U must exit 4, got $rc: $out"
git -C "$up" update-ref refs/tags/v10 "$U"
bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M1" \
  --upstream-url "$up" --upstream-ref v10 >/dev/null 2>&1 || fail "tag == U must pass"

# Non-text conflict (modify/delete on deleted-upstream.txt) is flagged, not hidden.
has "$(bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M1")" \
  "NON-TEXT CONFLICT" "modify/delete flagged"
has "$(bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M1")" \
  "deleted-upstream.txt" "non-text conflict names the file"

# Clean merge: empty resolution edits, whole incoming delta still listed.
git checkout -q -b clean1 "$BASE"
printf 'only upstream\n' >clean-up.txt; git add -A; git commit -qm cu
CU="$(git rev-parse HEAD)"
git checkout -q -b clean2 "$BASE"
printf 'only fork\n' >clean-fork.txt; git add -A; git commit -qm cf
CF="$(git rev-parse HEAD)"
git merge -q --no-ff -m merge "$CU" >/dev/null
CM="$(git rev-parse HEAD)"
out="$(bash "$pack" --fork-parent "$CF" --upstream-parent "$CU" --merge "$CM" 2>&1)" \
  || fail "clean merge must exit 0: $out"
has "$out" "Resolution edits: none" "clean merge has no edits"
has "$out" "clean-up.txt" "clean merge still lists incoming delta"

# Stop when no trustworthy baseline exists (git too old for merge-tree --write-tree).
shim="$work/shim"; mkdir -p "$shim"
cat >"$shim/git" <<SHIM
#!/usr/bin/env bash
if [ "\${1:-}" = merge-tree ]; then echo "fatal: unknown option" >&2; exit 129; fi
exec "$(command -v git)" "\$@"
SHIM
chmod +x "$shim/git"
set +e
out="$(PATH="$shim:$PATH" bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M1" 2>&1)"; rc=$?
set -e
[ "$rc" = 3 ] || fail "baseline failure must exit 3, got $rc: $out"
hasnt "$out" "Resolution edits" "no pack without baseline"

# Required checks on the exact head M, read through gh (shimmed).
ghbin="$work/ghbin"; mkdir -p "$ghbin"
cat >"$ghbin/gh" <<'SHIM'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$GH_LOG"
printf '%b' "$GH_RUNS"
SHIM
chmod +x "$ghbin/gh"
export GH_LOG="$work/gh.log"
set +e
out="$(GH_RUNS='build\tcompleted\tsuccess\nlint\tcompleted\tskipped\n' PATH="$ghbin:$PATH" \
  bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M1" --github-repo o/r 2>&1)"; rc=$?
set -e
[ "$rc" = 0 ] || fail "green checks must exit 0, got $rc: $out"
has "$out" "all 2 check runs green" "green checks"
has "$(cat "$GH_LOG")" "repos/o/r/commits/$M1/check-runs" "checks read for the exact head"
set +e
out="$(GH_RUNS='build\tcompleted\tsuccess\ntest\tcompleted\tfailure\n' PATH="$ghbin:$PATH" \
  bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M1" --github-repo o/r 2>&1)"; rc=$?
set -e
[ "$rc" = 5 ] || fail "red check must exit 5, got $rc: $out"
has "$out" "NOT GREEN" "red check named"
has "$out" "test" "failing check named"
set +e
out="$(GH_RUNS='' PATH="$ghbin:$PATH" bash "$pack" --fork-parent "$P" --upstream-parent "$U" --merge "$M1" --github-repo o/r 2>&1)"; rc=$?
set -e
[ "$rc" = 5 ] || fail "no check runs must exit 5, got $rc: $out"

# Usage errors.
set +e
bash "$pack" --merge "$M1" >/dev/null 2>&1; rc=$?
set -e
[ "$rc" = 64 ] || fail "missing args must exit 64, got $rc"

echo "merge-review-pack.test.sh: ok"
