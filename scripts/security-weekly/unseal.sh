#!/usr/bin/env bash
# CHE-3701: decrypt a weekly run's sealed reports on X99 (the private key never leaves it).
# Usage: unseal.sh <run-id> <out-dir>          downloads weekly-report-* from cheese-work/multica
#        unseal.sh --local <dir> <out-dir>     decrypts every *.gpg under <dir>
set -euo pipefail
export GNUPGHOME="${SECURITY_WEEKLY_GNUPGHOME:-/mnt/ssd2/security-weekly/gnupg}"
if [ "$1" = "--local" ]; then src="$2"; out="$3"; else
  src="$(mktemp -d)"; trap 'rm -rf "$src"' EXIT
  gh run download "$1" -R cheese-work/multica --pattern 'weekly-report-*' --dir "$src"; out="$2"
fi
mkdir -p "$out"
find "$src" -name '*.gpg' | while read -r f; do gpg --batch --quiet --yes --output "$out/$(basename "${f%.gpg}")" --decrypt "$f"; done
[ -n "$(ls -A "$out")" ] || { echo "unseal: no sealed report found" >&2; exit 1; }
