#!/usr/bin/env bash
# CHE-3701: encrypt each report to the X99 key and delete the plaintext. The repo is public, so an uploaded artifact is
# world-readable; only <file>.gpg may leave the job. Usage: seal.sh <file>...
# recipient.asc is PR-controlled, so it is never trusted on its own: SECURITY_WEEKLY_FPR (pinned in the workflow, owner-reviewed
# via CODEOWNERS) is the X99 key's primary fingerprint and the imported key must match it, else nothing is encrypted or removed.
set -euo pipefail
pin="${SECURITY_WEEKLY_FPR:-}"
[[ "$pin" =~ ^[0-9A-F]{40}$ ]] || { echo "seal: SECURITY_WEEKLY_FPR must be the 40-hex X99 key fingerprint" >&2; exit 1; }
recipient="${SECURITY_WEEKLY_RECIPIENT:-$(dirname "$0")/recipient.asc}"
GNUPGHOME="$(mktemp -d)"; export GNUPGHOME
trap 'rm -rf "$GNUPGHOME"' EXIT
gpg --batch --quiet --import "$recipient"
fpr="$(gpg --batch --with-colons --list-keys | awk -F: '/^fpr/{print $10; exit}')"
[ "$fpr" = "$pin" ] || { echo "seal: recipient key does not match the pinned X99 fingerprint" >&2; exit 1; }
for f in "$@"; do
  gpg --batch --quiet --yes --trust-model always --recipient "$fpr" --output "$f.gpg" --encrypt "$f"
  rm -f "$f"
done
