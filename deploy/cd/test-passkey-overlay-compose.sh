#!/usr/bin/env bash
set -euo pipefail

# Render test (CHE-4787, CHE-4982, CHE-7507): the released flags overlay must forward the WebAuthn
# relying-party config, the enrolment key, the review-admission settings and the
# merge-receipt OIDC settings to both backend colours, and bind-mount /etc/multica
# read-only (the policy file path lives there). cutover.sh replaces C00's compose files with the released
# ones, so a key missing here silently turns passkey approval (passkey.FromEnv
# returns nil when RP id/origins are empty) or review admission off after a
# cutover. Unset renders empty and must still succeed.
# Real `docker compose config` (no daemon), synthetic values only. Both flag-file
# vars are set: the overlay volume source nests a required legacy fallback that
# some compose versions (CI) evaluate eagerly.

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root_dir"

fail() { echo "test-passkey-overlay-compose: $*" >&2; exit 1; }

keys=(MULTICA_WEBAUTHN_RP_ID MULTICA_WEBAUTHN_ORIGINS MULTICA_PASSKEY_ENROLL_KEY MULTICA_REVIEW_ADMISSION MULTICA_REVIEW_ADMISSION_CHECKS MULTICA_REVIEW_ADMISSION_REVIEWERS MULTICA_MERGE_RECEIPT_OIDC_POLICY MULTICA_MERGE_RECEIPT_WORKSPACE_ID)
flags_file="$root_dir/deploy/cd/fixtures/jev-feature-flags.synthetic.yaml"

# render <set|unset> -> JSON on stdout
render() {
  local -a vars=(JWT_SECRET=synthetic-jwt-secret MULTICA_FEATURE_FLAGS_FILE="$flags_file" MULTICA_JEV_FLAGS_FILE="$flags_file")
  if [ "$1" = set ]; then
    vars+=(MULTICA_WEBAUTHN_RP_ID=synthetic.example MULTICA_WEBAUTHN_ORIGINS=https://synthetic.example:1 MULTICA_PASSKEY_ENROLL_KEY=synthetic-enroll-key MULTICA_REVIEW_ADMISSION=true MULTICA_REVIEW_ADMISSION_CHECKS=synthetic-a,synthetic-b MULTICA_REVIEW_ADMISSION_REVIEWERS=synthetic-agent-id MULTICA_MERGE_RECEIPT_OIDC_POLICY=/etc/multica/synthetic-policy.json MULTICA_MERGE_RECEIPT_WORKSPACE_ID=synthetic-workspace-id)
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
    const want = { MULTICA_WEBAUTHN_RP_ID: "synthetic.example", MULTICA_WEBAUTHN_ORIGINS: "https://synthetic.example:1", MULTICA_PASSKEY_ENROLL_KEY: "synthetic-enroll-key", MULTICA_REVIEW_ADMISSION: "true", MULTICA_REVIEW_ADMISSION_CHECKS: "synthetic-a,synthetic-b", MULTICA_REVIEW_ADMISSION_REVIEWERS: "synthetic-agent-id", MULTICA_MERGE_RECEIPT_OIDC_POLICY: "/etc/multica/synthetic-policy.json", MULTICA_MERGE_RECEIPT_WORKSPACE_ID: "synthetic-workspace-id" };
    for (const s of ["backend-blue", "backend-green"]) {
      const e = m.services[s].environment;
      for (const k of keys) {
        const exp = mode === "set" ? want[k] : "";
        if (e[k] !== exp) { console.error(`${s} ${k} (${mode}): got ${JSON.stringify(e[k])}, want ${JSON.stringify(exp)}`); process.exit(1); }
      }
      // /etc/multica: read-only bind of the host dir at the same path, in both
      // modes. Docker creates a missing host dir for a bind (create_host_path
      // defaults true), so an absent /etc/multica must not fail startup; it
      // must not be turned off here.
      const v = (m.services[s].volumes || []).find((x) => x.target === "/etc/multica");
      if (!v || v.type !== "bind" || v.source !== "/etc/multica" || v.read_only !== true || (v.bind && v.bind.create_host_path === false)) {
        console.error(`${s} (${mode}): /etc/multica mount wrong: ${JSON.stringify(v)}`); process.exit(1);
      }
    }
  ' "$1" "${keys[@]}" || fail "$1: overlay does not forward the passkey, review-admission and merge-receipt env, or mount /etc/multica"
}

check set
check unset

echo "test-passkey-overlay-compose: ok"
