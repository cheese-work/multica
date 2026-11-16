#!/usr/bin/env bash
set -euo pipefail

# Regression coverage for scripts/merge-ledger-check (CHE-6562, plan CHE-6210
# A2b): builds a fixture upstream merge in a scratch repo, writes a correct
# path ledger and checks that every gap in the ledger fails closed.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
check="$root_dir/scripts/merge-ledger-check"
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
mkdir -p docs/fork
printf 'a\nb\nc\n' >shared.txt
printf '1\n2\n3\n4\n5\n6\n7\n8\n' >both-clean.txt
printf 'u\n' >keep-up.txt
printf 'n\n' >named-only.txt
printf 'x\n' >deleted-upstream.txt
git add -A; git commit -qm base
BASE="$(git rev-parse HEAD)"

# Fork side (P): conflicting edit, clean edit, modify side of a modify/delete,
# and a FORK-FEATURES ledger that names named-only.txt.
git checkout -q -b fork
printf 'a\nFORK\nc\n' >shared.txt
printf 'FORK\n2\n3\n4\n5\n6\n7\n8\n' >both-clean.txt
printf 'fork edit\n' >deleted-upstream.txt
printf '| C99 Named feature | Keep ours. Source: `named-only.txt:1` |\n' >docs/fork/FORK-FEATURES.md
git add -A; git commit -qm fork
P="$(git rev-parse HEAD)"

# Upstream side (U): conflicting edit, clean edit, delete, new file, upstream-only edits.
git checkout -q -b upstream "$BASE"
printf 'a\nUP\nc\n' >shared.txt
printf '1\n2\n3\n4\n5\n6\n7\nUP\n' >both-clean.txt
printf 'u2\n' >keep-up.txt
printf 'n2\n' >named-only.txt
git rm -q deleted-upstream.txt
printf 'new\n' >new-file.txt
git add -A; git commit -qm upstream
U="$(git rev-parse HEAD)"
git checkout -q fork

# Computed set at the pins: 5 paths changed upstream plus the new file.
# Fork-touched: shared.txt, both-clean.txt, deleted-upstream.txt.
# Named in FORK-FEATURES at P: named-only.txt.
HDR=$'path\tgroup\tchange\tconflict\tfork_touched\tfork_features\tdisposition\tevidence'
rows=(
  $'both-clean.txt\tA3d\tM\tclean\tyes\t\tclean-both\ttest: both-clean merged hunks read against both parents'
  $'deleted-upstream.txt\tA3d\tD\tmodify/delete\tyes\t\tfork-kept\ttest: fork callers kept, deferred to 5a row'
  $'keep-up.txt\tA3d\tM\tclean\tno\t\tupstream-only\t'
  $'named-only.txt\tA3d\tM\tclean\tno\tC99\tclean-both\ttest: C99 preservation test passes on M'
  $'new-file.txt\tA3d\tA\tclean\tno\t\tupstream-only\t'
  $'shared.txt\tA3d\tM\tcontent\tyes\t\tresolved\ttest: shared.txt keeps FORK line; group note line 1'
)
# mkledger FILE [SKIP_PREFIX]: write a ledger, dropping rows that start with SKIP_PREFIX.
mkledger() {
  local f="$1" skip="${2:-}" r
  { printf '%s\n' "$HDR"
    for r in "${rows[@]}"; do
      [ -n "$skip" ] && [[ "$r" == "$skip"* ]] && continue
      printf '%s\n' "$r"
    done; } >"$f"
}
# run LEDGER: sets out and rc.
run() {
  set +e
  out="$(bash "$check" --ledger "$1" --fork-parent "$P" --upstream-parent "$U" 2>&1)"; rc=$?
  set -e
}
expect() { [ "$rc" = "$1" ] || fail "$2: want exit $1, got $rc: $out"; }

# Green: a complete ledger passes.
mkledger "$work/ok.tsv"
run "$work/ok.tsv"
expect 0 "complete ledger"
has "$out" "ok" "complete ledger reports ok"

# Red target: a ledger that lacks one incoming path fails closed and names it.
mkledger "$work/missing.tsv" $'new-file.txt'
run "$work/missing.tsv"
expect 6 "ledger lacks an incoming path"
has "$out" "new-file.txt" "missing path named"

# A conflicted path is in the computed set too: dropping it fails.
mkledger "$work/missing-conflict.tsv" $'shared.txt'
run "$work/missing-conflict.tsv"
expect 6 "ledger lacks a conflicted path"
has "$out" "shared.txt" "missing conflicted path named"

# A path that is not in the computed set fails (ledger of another pin pair).
{ cat "$work/ok.tsv"; printf 'stray.txt\tA3d\tA\tclean\tno\t\tupstream-only\t\n'; } >"$work/extra.tsv"
run "$work/extra.tsv"
expect 6 "ledger has an extra path"
has "$out" "stray.txt" "extra path named"

# A duplicate row fails.
{ cat "$work/ok.tsv"; printf '%s\n' "${rows[4]}"; } >"$work/dup.tsv"
run "$work/dup.tsv"
expect 6 "duplicate row"
has "$out" "new-file.txt" "duplicate path named"

# A row without a disposition fails.
sed 's/^keep-up.txt\(\t[^\t]*\t[^\t]*\t[^\t]*\t[^\t]*\t[^\t]*\t\)upstream-only/keep-up.txt\1/' "$work/ok.tsv" >"$work/nodisp.tsv"
cmp -s "$work/ok.tsv" "$work/nodisp.tsv" && fail "fixture: nodisp edit did not apply"
run "$work/nodisp.tsv"
expect 7 "row has no disposition"
has "$out" "keep-up.txt" "row without disposition named"

# A disposition outside the allowed values fails.
sed 's/\tupstream-only\t/\ttake-upstream\t/' "$work/ok.tsv" >"$work/baddisp.tsv"
run "$work/baddisp.tsv"
expect 7 "unknown disposition"
has "$out" "take-upstream" "unknown disposition named"

# A malformed header or a short row fails.
{ printf 'path\tdisposition\n'; tail -n +2 "$work/ok.tsv"; } >"$work/badhdr.tsv"
run "$work/badhdr.tsv"
expect 7 "wrong header"
{ cat "$work/ok.tsv"; printf 'short.txt\tA3d\n'; } >"$work/short.tsv"
run "$work/short.tsv"
expect 7 "short row"

# A fork-touched path without preservation evidence fails.
sed 's/\ttest: both-clean merged hunks read against both parents$/\t/' "$work/ok.tsv" >"$work/noevid.tsv"
cmp -s "$work/ok.tsv" "$work/noevid.tsv" && fail "fixture: noevid edit did not apply"
run "$work/noevid.tsv"
expect 8 "fork-touched path without evidence"
has "$out" "both-clean.txt" "fork-touched path named"

# A placeholder is not evidence.
sed 's/\ttest: both-clean merged hunks read against both parents$/\tn\/a/' "$work/ok.tsv" >"$work/placeholder.tsv"
run "$work/placeholder.tsv"
expect 8 "placeholder evidence"

# A FORK-FEATURES path the fork never touched still needs evidence.
sed 's/\ttest: C99 preservation test passes on M$/\t/' "$work/ok.tsv" >"$work/noevid-named.tsv"
cmp -s "$work/ok.tsv" "$work/noevid-named.tsv" && fail "fixture: noevid-named edit did not apply"
run "$work/noevid-named.tsv"
expect 8 "FORK-FEATURES path without evidence"
has "$out" "named-only.txt" "FORK-FEATURES path named"

# The ledger's own fork_touched column is not trusted: git decides.
sed -e 's/^both-clean.txt\tA3d\tM\tclean\tyes/both-clean.txt\tA3d\tM\tclean\tno/' \
    -e 's/\ttest: both-clean merged hunks read against both parents$/\t/' "$work/ok.tsv" >"$work/lie.tsv"
run "$work/lie.tsv"
expect 8 "ledger claims fork-touched=no"

# Several faults in one ledger: all are reported, the path-set fault wins the exit code.
{ cat "$work/noevid.tsv" | grep -v '^new-file.txt'; } >"$work/multi.tsv"
run "$work/multi.tsv"
expect 6 "path-set fault wins the exit code"
has "$out" "new-file.txt" "multi: missing path"
has "$out" "both-clean.txt" "multi: missing evidence"

# Pins: short SHA and unknown commit exit 2.
set +e
out="$(bash "$check" --ledger "$work/ok.tsv" --fork-parent "${P:0:8}" --upstream-parent "$U" 2>&1)"; rc=$?
set -e
expect 2 "short SHA"
has "$out" "40-hex" "short SHA message"
set +e
out="$(bash "$check" --ledger "$work/ok.tsv" --fork-parent "$P" --upstream-parent 0123456789012345678901234567890123456789 2>&1)"; rc=$?
set -e
expect 2 "unknown commit"

# No trustworthy computed set: git too old for merge-tree --write-tree exits 3.
shim="$work/shim"; mkdir -p "$shim"
cat >"$shim/git" <<SHIM
#!/usr/bin/env bash
if [ "\${1:-}" = merge-tree ]; then echo "fatal: unknown option" >&2; exit 129; fi
exec "$(command -v git)" "\$@"
SHIM
chmod +x "$shim/git"
set +e
out="$(PATH="$shim:$PATH" bash "$check" --ledger "$work/ok.tsv" --fork-parent "$P" --upstream-parent "$U" 2>&1)"; rc=$?
set -e
expect 3 "merge-tree failure"

# Usage errors: missing args and an unreadable ledger.
set +e
bash "$check" --ledger "$work/ok.tsv" >/dev/null 2>&1; rc=$?
set -e
expect 64 "missing pins"
set +e
bash "$check" --ledger "$work/none.tsv" --fork-parent "$P" --upstream-parent "$U" >/dev/null 2>&1; rc=$?
set -e
expect 64 "missing ledger file"

echo "merge-ledger-check.test.sh: ok"
