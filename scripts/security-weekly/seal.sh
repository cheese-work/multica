#!/usr/bin/env bash
# CHE-3701: encrypt each report to the X99 key and delete the plaintext. The repo is public, so an uploaded artifact is
# world-readable; only <file>.gpg may leave the job. Usage: seal.sh <file>...
set -euo pipefail
recipient="${SECURITY_WEEKLY_RECIPIENT:-$(dirname "$0")/recipient.asc}"
GNUPGHOME="$(mktemp -d)"; export GNUPGHOME
trap 'rm -rf "$GNUPGHOME"' EXIT
gpg --batch --quiet --import "$recipient"
fpr="$(gpg --batch --with-colons --list-keys | awk -F: '/^fpr/{print $10; exit}')"
for f in "$@"; do
  gpg --batch --quiet --yes --trust-model always --recipient "$fpr" --output "$f.gpg" --encrypt "$f"
  rm -f "$f"
done
