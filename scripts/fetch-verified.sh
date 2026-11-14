#!/usr/bin/env bash
# Download <url> to <out> and verify its sha256 (CHE-4877). Usage: fetch-verified.sh <url> <sha256> <out>
# A file already at <out> (e.g. restored by actions/cache) is re-verified and reused; a stale one is replaced.
# Retries only HTTP 5xx and curl transport failures (up to 4 attempts); a 4xx or a checksum mismatch fails at once.
set -euo pipefail
url="${1:?usage: fetch-verified.sh <url> <sha256> <out>}"; sha="${2:?sha256}"; out="${3:?out}"
verify() { echo "$sha  $1" | sha256sum -c - >/dev/null 2>&1; }

if [[ -f "$out" ]] && verify "$out"; then exit 0; fi
mkdir -p "$(dirname "$out")"; tmp="$out.part"; trap 'rm -f "$tmp"' EXIT
for attempt in 1 2 3 4; do
  rc=0; code="$(curl -sSL -o "$tmp" -w '%{http_code}' --connect-timeout 20 --max-time 300 "$url")" || rc=$?
  if [[ $rc -eq 0 && "$code" == 2* ]]; then
    verify "$tmp" || { echo "fetch-verified: checksum mismatch for $url" >&2; exit 1; }
    mv "$tmp" "$out"; exit 0
  fi
  if [[ $rc -eq 0 && "$code" != 5* ]]; then echo "fetch-verified: HTTP $code for $url" >&2; exit 1; fi
  echo "fetch-verified: attempt $attempt/4 failed (HTTP ${code:-none}, curl exit $rc)" >&2
  [[ $attempt -lt 4 ]] && sleep "${FETCH_RETRY_DELAY:-$((attempt * 5))}"
done
echo "fetch-verified: giving up on $url" >&2; exit 1
