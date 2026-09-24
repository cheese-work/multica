#!/usr/bin/env bash
# Build the `multica` CLI from an exact commit of a given repo and install it
# to an authenticated daemon host's local bin directory.
#
# `multica update` and scripts/install.sh both pull from multica-ai/multica's
# PUBLIC GitHub Releases — there is no automated path from a merged commit on
# a private fork (e.g. cheese-work/multica) to an authenticated daemon host's
# `~/.local/bin/multica`. CD (cd-deploy.yml) only ever updates C00's backend
# container; it never touches a daemon host's own CLI install. Until now the
# only way to get a fork-only commit onto a daemon host was the manual
# `make build` a preflight would run by hand (see CHE-765).
#
# Usage:
#   scripts/install-cli-from-ref.sh --repo <git-url> --ref <full-40-char-commit-sha> [--bin-dir <dir>]
#
# `--ref` must be a full, immutable 40-character commit SHA, not a branch or
# tag: those can move between the moment an operator reads them and the
# moment this script resolves them, silently installing a different commit
# than the one that was reviewed and qualified. `git checkout` on a 40-char
# hex string is already exact; this only rejects anything shorter or
# non-hex before doing any network or filesystem work.
#
# Stages the build in a scratch directory, verifies its reported commit
# BEFORE touching the target bin dir, and only then does one atomic
# replacement (`mv` within the same filesystem, so readers never see a
# partial file). If verification fails, or the atomic replace itself fails,
# the previous binary at --bin-dir is left untouched — this script never
# unlinks the existing multica before its replacement is proven good.
set -euo pipefail

usage() {
  echo "usage: install-cli-from-ref.sh --repo <git-url> --ref <full-40-char-commit-sha> [--bin-dir <dir>]" >&2
}

repo_url=""
ref=""
bin_dir="${MULTICA_BIN_DIR:-$HOME/.local/bin}"

while (($#)); do
  case "$1" in
    --repo) repo_url=${2:?}; shift 2 ;;
    --ref) ref=${2:?}; shift 2 ;;
    --bin-dir) bin_dir=${2:?}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done
[ -n "$repo_url" ] || { usage; exit 2; }
[ -n "$ref" ] || { usage; exit 2; }

if ! [[ "$ref" =~ ^[0-9a-fA-F]{40}$ ]]; then
  echo "install-cli-from-ref: --ref must be a full 40-character commit SHA, not a branch, tag, or short SHA (got: $ref)" >&2
  echo "install-cli-from-ref: a mutable ref can resolve to a different commit than the one reviewed/qualified" >&2
  exit 2
fi

# ---------------------------------------------------------------------------
# Fail closed on every prerequisite before any clone or build work starts —
# a partial failure after cloning wastes the clone and can leave ambiguous
# partial state; checking first makes every failure mode a clean, immediate,
# named exit.
# ---------------------------------------------------------------------------
required_go_version="$(grep -E '^go [0-9]+\.[0-9]+\.[0-9]+$' "$(dirname "${BASH_SOURCE[0]}")/../server/go.mod" 2>/dev/null | awk '{print $2}')"
required_go_version="${required_go_version:-1.26.6}"

command -v git >/dev/null 2>&1 || { echo "install-cli-from-ref: git is required" >&2; exit 1; }
command -v go >/dev/null 2>&1 || { echo "install-cli-from-ref: go is required (server/go.mod needs $required_go_version or newer; none found on PATH)" >&2; exit 1; }
command -v node >/dev/null 2>&1 || { echo "install-cli-from-ref: node is required (used to verify the installed binary's reported commit)" >&2; exit 1; }

# Not re-checked against the local `go version` output here: Go's own
# toolchain resolution (GOTOOLCHAIN=auto by default) transparently fetches a
# newer patch to satisfy server/go.mod's `go 1.26.6` floor even when the
# PATH binary reports an older one — verified on this host (go1.26.2 on
# PATH, `go build` still succeeds by fetching 1.26.6+ on demand). Duplicating
# a version gate here would reject hosts `go build` itself accepts. `go
# build` below is the actual enforcement point: with GOTOOLCHAIN=local (or no
# network to fetch a newer toolchain) it fails closed with its own clear
# "go.mod requires go >= X" diagnostic before anything is staged or replaced.
installed_go_version="$(go version | grep -oE 'go[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1 | tr -d 'go')"
echo "go on PATH: $installed_go_version (server/go.mod requires $required_go_version or newer; GOTOOLCHAIN=${GOTOOLCHAIN:-auto} may fetch a newer one automatically)"

echo "Checking clone access to $repo_url..."
if ! git ls-remote --exit-code "$repo_url" >/dev/null 2>&1; then
  echo "install-cli-from-ref: cannot reach $repo_url — this host needs read access to the private repo (SSH key or credential helper) before it can build a candidate" >&2
  exit 1
fi

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

echo "Cloning $repo_url at $ref..."
git clone --quiet "$repo_url" "$work_dir/src"
git -C "$work_dir/src" checkout --quiet "$ref"
resolved_commit="$(git -C "$work_dir/src" rev-parse HEAD)"
if [ "$resolved_commit" != "$ref" ]; then
  echo "install-cli-from-ref: checked-out commit $resolved_commit does not match requested $ref" >&2
  exit 1
fi

echo "Building multica CLI from $resolved_commit..."
staged_binary="$work_dir/multica"
(
  cd "$work_dir/src/server"
  go build -ldflags "-X main.version=$ref -X main.commit=${resolved_commit:0:9} -X main.date=$(date -u '+%Y-%m-%dT%H:%M:%SZ')" \
    -o "$staged_binary" ./cmd/multica
)

# ---------------------------------------------------------------------------
# Verify the STAGED binary in place, before it ever touches --bin-dir. Only
# once this passes does the existing installed binary become eligible for
# replacement — a build that produces a binary reporting the wrong commit
# (or that fails to run at all) never displaces a working install.
# ---------------------------------------------------------------------------
staged_commit="$("$staged_binary" version --output json | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>process.stdout.write(JSON.parse(d).commit))')"
if [ "${resolved_commit:0:9}" != "$staged_commit" ]; then
  echo "install-cli-from-ref: staged binary reports commit $staged_commit, expected ${resolved_commit:0:9} — leaving any existing install at $bin_dir/multica untouched" >&2
  exit 1
fi

mkdir -p "$bin_dir"
chmod 0755 "$staged_binary"

# Preserve whatever is currently installed BEFORE the replace, so a failure
# that only manifests after the rename (the new binary can't even exec on
# this filesystem — e.g. noexec, wrong architecture, truncated copy — not
# just "wrong commit") still has something to restore. Backing this up is
# unconditional: even a $bin_dir/multica this script cannot itself verify
# (e.g. it was already broken) is still ours to put back exactly as found,
# never ours to silently drop.
#
# The backup lives in --bin-dir itself, NOT $work_dir: $work_dir is removed
# unconditionally on exit (the EXIT trap above) — including on a SUCCESSFUL
# run — so a backup staged there would already be gone by the time anyone
# needed to roll back. Living next to the install target also guarantees the
# backup and the live binary share one filesystem, which restore_previous
# below needs for its rename(2) to be atomic rather than a cross-device copy.
backup_binary="$bin_dir/.multica.previous"
rm -f "$backup_binary"
if [ -e "$bin_dir/multica" ]; then
  cp -p "$bin_dir/multica" "$backup_binary"
fi

restore_previous() {
  if [ -e "$backup_binary" ]; then
    # Same-filesystem rename(2), not an in-place copy: a copy overwrites the
    # live path byte-by-byte, so a concurrent reader (or the daemon that
    # execs this exact path) can observe a partially written file mid-copy.
    # A rename instead swaps the directory entry atomically — any reader
    # either sees the broken binary being replaced or the fully-restored
    # previous one, never a partial file.
    mv -f "$backup_binary" "$bin_dir/multica"
    echo "install-cli-from-ref: restored the previous binary at $bin_dir/multica" >&2
  else
    rm -f "$bin_dir/multica"
    echo "install-cli-from-ref: removed the partially installed binary at $bin_dir/multica (nothing was installed there before)" >&2
  fi
}

# Atomic replacement: `mv` within the same filesystem is a single rename(2),
# so a reader (including a daemon that execs this path) either sees the old
# binary or the fully-staged new one, never a partially written file. Staging
# inside --bin-dir itself (not $work_dir, which trap will delete regardless
# of outcome) keeps the rename on one filesystem even when $TMPDIR is a
# separate mount from $bin_dir.
replace_tmp="$bin_dir/.multica.new.$$"
cp "$staged_binary" "$replace_tmp"
if ! mv -f "$replace_tmp" "$bin_dir/multica"; then
  # The rename never happened, so $bin_dir/multica is exactly what it was
  # before this run — $backup_binary is redundant right now but is left in
  # place rather than deleted: it is still a truthful, durable copy of what
  # is currently installed, and this script's job is never to remove a
  # rollback artifact it did not just supersede with a newer one.
  echo "install-cli-from-ref: atomic replace of $bin_dir/multica failed — previous binary (if any) is untouched" >&2
  rm -f "$replace_tmp"
  exit 1
fi

# Verify the binary AT ITS FINAL INSTALLED PATH, not just the staged copy:
# this is the only way to catch a failure mode staging can't see, such as
# --bin-dir being on a noexec mount. Any failure here — exec itself failing,
# or the exec succeeding but reporting the wrong commit — restores the
# previous binary before this script exits, so a bad replacement never
# leaves the host worse off than before the attempt.
set +e
installed_commit="$("$bin_dir/multica" version --output json 2>/dev/null | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>{try{process.stdout.write(JSON.parse(d).commit)}catch{}})' 2>/dev/null)"
verify_status=$?
set -e

if [ "$verify_status" -ne 0 ] || [ -z "$installed_commit" ]; then
  echo "install-cli-from-ref: FATAL — $bin_dir/multica failed to execute after replacement (exit $verify_status)" >&2
  restore_previous
  exit 1
fi
if [ "${resolved_commit:0:9}" != "$installed_commit" ]; then
  echo "install-cli-from-ref: FATAL — $bin_dir/multica reports commit $installed_commit after replacement, expected ${resolved_commit:0:9}" >&2
  restore_previous
  exit 1
fi

# Success: deliberately NOT deleting $backup_binary here. The verification
# above only proves the new binary starts and reports the right commit — it
# cannot prove the new commit is actually correct for this host's workload,
# so the previous binary stays at $bin_dir/.multica.previous as the durable
# rollback packet for whatever this script cannot itself catch. A future
# install run's own unconditional `rm -f "$backup_binary"` (above, before
# taking its own backup) is what retires it, once a NEWER successful install
# makes it stale.
if [ -e "$backup_binary" ]; then
  echo "Previous binary kept at $backup_binary for rollback." >&2
fi

echo "Installed multica ($installed_commit) to $bin_dir/multica"
