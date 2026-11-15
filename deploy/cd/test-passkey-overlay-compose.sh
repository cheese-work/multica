#!/usr/bin/env bash
set -euo pipefail

# Render test (CHE-4787, CHE-4982): the released flags overlay must forward the WebAuthn
# relying-party config, the enrolment key and the review-admission settings to
# both backend colours. cutover.sh replaces C00's compose files with the released
# ones, so a key missing here silently turns passkey approval (passkey.FromEnv
# returns nil when RP id/origins are empty) or review admission off after a
# cutover. Unset renders empty and must still succeed.
# Real `docker compose config` (no daemon), synthetic values only. Both flag-file
# vars are set: the overlay volume source nests a required legacy fallback that
# some compose versions (CI) evaluate eagerly.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

fail() { echo "test-passkey-overlay-compose: $*" >&2; exit 1; }

keys=(MULTICA_WEBAUTHN_RP_ID MULTICA_WEBAUTHN_ORIGINS MULTICA_PASSKEY_ENROLL_KEY MULTICA_REVIEW_ADMISSION MULTICA_REVIEW_ADMISSION_CHECKS MULTICA_REVIEW_ADMISSION_REVIEWERS)
flags_file="$root_dir/deploy/cd/fixtures/jev-feature-flags.synthetic.yaml"

# render <set|unset> -> JSON on stdout
render() {
  local -a vars=(JWT_SECRET=synthetic-jwt-secret MULTICA_FEATURE_FLAGS_FILE="$flags_file" MULTICA_JEV_FLAGS_FILE="$flags_file")
  if [ "$1" = set ]; then
    vars+=(MULTICA_WEBAUTHN_RP_ID=synthetic.example MULTICA_WEBAUTHN_ORIGINS=https://synthetic.example:1 MULTICA_PASSKEY_ENROLL_KEY=synthetic-enroll-key MULTICA_REVIEW_ADMISSION=true MULTICA_REVIEW_ADMISSION_CHECKS=synthetic-a,synthetic-b MULTICA_REVIEW_ADMISSION_REVIEWERS=synthetic-agent-id)
  fi
  env -i HOME="$HOME" PATH="$PATH" "${vars[@]}" docker compose \
    -f docker-compose.selfhost.yml -f deploy/cd/docker-compose.ab.yml -f deploy/cd/docker-compose.flags.yml \
    config --format json
}

check() { # check <set|unset>
  local json
  json="$(render "$1")" || fail "render failed ($1)"
  printf '%s' "$json" | node -e '
    const m = JSON.parse(require("node:fs").readFileSync(0, "utf8"));
    const [mode, ...keys] = process.argv.slice(1);
    const want = { MULTICA_WEBAUTHN_RP_ID: "synthetic.example", MULTICA_WEBAUTHN_ORIGINS: "https://synthetic.example:1", MULTICA_PASSKEY_ENROLL_KEY: "synthetic-enroll-key", MULTICA_REVIEW_ADMISSION: "true", MULTICA_REVIEW_ADMISSION_CHECKS: "synthetic-a,synthetic-b", MULTICA_REVIEW_ADMISSION_REVIEWERS: "synthetic-agent-id" };
    for (const s of ["backend-blue", "backend-green"]) {
      const e = m.services[s].environment;
      for (const k of keys) {
        const exp = mode === "set" ? want[k] : "";
        if (e[k] !== exp) { console.error(`${s} ${k} (${mode}): got ${JSON.stringify(e[k])}, want ${JSON.stringify(exp)}`); process.exit(1); }
      }
    }
  ' "$1" "${keys[@]}" || fail "$1: overlay does not forward the passkey and review-admission env"
}

check set
check unset

echo "test-passkey-overlay-compose: ok"
