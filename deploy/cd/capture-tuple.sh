#!/usr/bin/env bash
set -euo pipefail

# Capture the LIVE self-host stack's identity as a C00 deployment-baseline
# tuple snapshot (CHE-530). Runs ON C00.
#
# Two callers, one question — "what is this host running right now?":
#
#   1. cd-deploy.yml's prepare-release-candidate job, over SSH, to obtain the
#      baseline tuple a release-candidate manifest must bind (admission.mjs
#      hashes it via tuple-snapshot.mjs, so it MUST pass that validator).
#   2. deploy.sh, after a successful deploy, to record the newly deployed
#      tuple as the next deploy's rollback baseline.
#
# Those were separate implementations until this script existed, and the
# deploy.sh copy emitted placeholder digests that tuple-snapshot.mjs rejects
# (61 hex characters, not 64) for compose.sha256, migration_ledger.ordered_sha256
# and any unresolvable image digest. That made every deployed-tuple.json
# unusable as the NEXT deploy's admission baseline — the automatic chain
# worked once and then refused its own output. Every digest this script emits
# is a real one computed from the live host.
#
# Read-only with respect to the stack: it inspects containers, reads the
# compose file, and runs one SELECT against the migration ledger. It starts,
# stops and mutates nothing.

usage() {
  cat <<'EOF'
usage: capture-tuple.sh --compose-dir PATH --output PATH [--application-sha SHA]

  --compose-dir PATH      Directory holding docker-compose.selfhost.yml (the
                          live self-host stack root on C00).
  --output PATH           Where to write the tuple snapshot JSON.
  --application-sha SHA   Override the recorded application_sha. Defaults to
                          the running backend container's
                          org.opencontainers.image.revision label, which is
                          what the image was actually built from.
EOF
}

compose_dir=""
output=""
application_sha=""

while (($#)); do
  case "$1" in
    --compose-dir) compose_dir=${2:?}; shift 2 ;;
    --output) output=${2:?}; shift 2 ;;
    --application-sha) application_sha=${2:?}; shift 2 ;;
    --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [ -z "$compose_dir" ] || [ -z "$output" ]; then
  echo "missing --compose-dir or --output" >&2
  usage >&2
  exit 2
fi

compose_file="$compose_dir/docker-compose.selfhost.yml"
if [ ! -f "$compose_file" ]; then
  echo "required file does not exist: $compose_file" >&2
  exit 1
fi

compose() {
  (cd "$compose_dir" && docker compose -f docker-compose.selfhost.yml "$@")
}

# service_image reports the "repo:tag" reference a running compose service was
# started from. `compose images --format json` is the documented way to ask
# Compose this; it returns an array (one row per container) on modern versions
# and a bare object on some older ones, so handle both.
service_image() {
  compose images "$1" --format json 2>/dev/null | node -e '
    let d = "";
    process.stdin.on("data", (c) => (d += c));
    process.stdin.on("end", () => {
      const rows = JSON.parse(d || "[]");
      const row = Array.isArray(rows) ? rows[0] : rows;
      process.stdout.write(row && row.Repository ? `${row.Repository}:${row.Tag}` : "");
    });
  '
}

# image_digest resolves a "repo:tag" reference to the sha256 digest the local
# daemon holds for it. RepoDigests is the registry-provided digest and is the
# one a manifest pins; it is empty for a locally-built image that was never
# pushed or pulled, in which case the image ID (.Id, the config digest) is the
# only stable identifier available and is still a real, verifiable digest of
# that exact image — unlike a placeholder, it identifies something.
image_digest() {
  local ref=$1 digest
  digest="$(docker inspect --format '{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}' "$ref" 2>/dev/null | sed -E 's#^.*@##')"
  if [ -z "$digest" ]; then
    digest="$(docker inspect --format '{{.Id}}' "$ref" 2>/dev/null)"
  fi
  printf '%s' "$digest"
}

# service_config_hash returns Compose's own config hash for a service's running
# container — the value Compose itself uses to decide whether a container is
# stale. The tuple records it redacted (8-character prefix + 4-character
# suffix, the shape tuple-snapshot.mjs validates) because the full hash is
# derived from the service's resolved environment, which includes secret
# values.
service_config_hash() {
  local container
  container="$(compose ps -q "$1" 2>/dev/null | head -n 1)"
  if [ -z "$container" ]; then
    printf ''
    return 0
  fi
  docker inspect --format '{{index .Config.Labels "com.docker.compose.config-hash"}}' "$container" 2>/dev/null
}

backend_ref="$(service_image backend)"
web_ref="$(service_image frontend)"
if [ -z "$backend_ref" ] || [ -z "$web_ref" ]; then
  echo "could not resolve the running backend/frontend image references — is the stack up?" >&2
  exit 1
fi

backend_digest="$(image_digest "$backend_ref")"
web_digest="$(image_digest "$web_ref")"
if [ -z "$backend_digest" ] || [ -z "$web_digest" ]; then
  echo "could not resolve a digest for $backend_ref / $web_ref" >&2
  exit 1
fi

if [ -z "$application_sha" ]; then
  application_sha="$(docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$backend_ref" 2>/dev/null)"
fi
if ! [[ "$application_sha" =~ ^[a-f0-9]{40}$ ]]; then
  echo "could not determine a 40-character application_sha (got '${application_sha:-<empty>}'); pass --application-sha" >&2
  exit 1
fi

compose_sha="sha256:$(sha256sum "$compose_file" | awk '{print $1}')"
backend_config_hash="$(service_config_hash backend)"
web_config_hash="$(service_config_hash frontend)"

# One statement, two facts: the ledger's size and its ordered content digest.
# The ordered digest is what makes "the schema this tuple describes" verifiable
# — a row count alone cannot distinguish two ledgers of equal length.
# applied_at is formatted by Postgres into RFC3339 UTC rather than being
# reformatted in JS: psql's default timestamptz rendering ("... +00") is not
# a format JavaScript's Date parser accepts, and converting it by hand is a
# second, driftable date implementation. to_char in the query is the same
# fact, already in the shape tuple-snapshot.mjs validates.
ledger_tsv="$(compose exec -T postgres psql \
  -U "${POSTGRES_USER:-multica}" -d "${POSTGRES_DB:-multica}" -tA -F'|' -c \
  "SELECT count(*), coalesce(max(version), ''), coalesce(to_char(max(applied_at) AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'), '') FROM schema_migrations")"
ledger_ordered="$(compose exec -T postgres psql \
  -U "${POSTGRES_USER:-multica}" -d "${POSTGRES_DB:-multica}" -tA -c \
  "SELECT version FROM schema_migrations ORDER BY version" | sha256sum | awk '{print $1}')"

IFS='|' read -r ledger_count ledger_version ledger_applied_at <<<"$ledger_tsv"

node -e '
  const fs = require("node:fs");
  const [
    outPath, backendRef, backendDigest, webRef, webDigest, applicationSha,
    composeSha, backendConfigHash, webConfigHash,
    ledgerCount, ledgerVersion, ledgerAppliedAt, ledgerOrdered,
  ] = process.argv.slice(1);

  // tuple-snapshot.mjs requires an 8-character prefix and 4-character suffix.
  // A hash Compose did not provide (service not running) has no honest
  // redaction, so fail rather than inventing zeros: a tuple that records a
  // configuration it never read is the kind of fiction this whole chain
  // exists to keep out of admission.
  const redact = (name, hash) => {
    if (!/^[a-f0-9]{12,}$/.test(hash || "")) {
      throw new Error(`could not read compose config-hash for ${name}`);
    }
    return { prefix: hash.slice(0, 8), suffix: hash.slice(-4) };
  };

  const applied = ledgerAppliedAt || new Date().toISOString().replace(/\.\d+Z$/, "Z");

  const snapshot = {
    schema_version: 1,
    role: "c00-deployment-baseline-read-only-metadata",
    captured_at: new Date().toISOString().replace(/\.\d+Z$/, "Z"),
    application_sha: applicationSha,
    images: {
      backend: { reference: backendRef, digest: backendDigest },
      web: { reference: webRef, digest: webDigest },
    },
    compose: {
      file: "docker-compose.selfhost.yml",
      sha256: composeSha,
      service_config_sha256: {
        backend: redact("backend", backendConfigHash),
        web: redact("web", webConfigHash),
      },
    },
    migration_ledger: {
      row_count: Number(ledgerCount) || 0,
      latest: { version: ledgerVersion, applied_at: applied },
      ordered_sha256: `sha256:${ledgerOrdered}`,
    },
  };
  fs.writeFileSync(outPath, `${JSON.stringify(snapshot, null, 2)}\n`);
' "$output" "$backend_ref" "$backend_digest" "$web_ref" "$web_digest" "$application_sha" \
  "$compose_sha" "$backend_config_hash" "$web_config_hash" \
  "$ledger_count" "$ledger_version" "$ledger_applied_at" "$ledger_ordered"

# Emit only a tuple that the validator downstream will actually accept. A
# snapshot that fails here would otherwise surface as an opaque admission
# failure one job later, with the real cause already out of scope.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
node "$script_dir/tuple-snapshot.mjs" verify --snapshot "$output" >/dev/null
