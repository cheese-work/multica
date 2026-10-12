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
usage: capture-tuple.sh --compose-dir PATH --output PATH [--application-sha SHA] [--git-dir PATH] [--compose-service-suffix SUFFIX]

  --compose-dir PATH      Directory holding docker-compose.selfhost.yml (the
                          live self-host stack root on C00).
  --output PATH           Where to write the tuple snapshot JSON.
  --application-sha SHA   Override the recorded application_sha. Defaults to
                          the running backend container's
                          org.opencontainers.image.revision label, then to the
                          image tag resolved against --git-dir.
  --git-dir PATH          A checkout of this repository used to verify, and to
                          expand, a commit taken from the image tag. Defaults
                          to this script's own repository when it is run from
                          one. Ignored when --application-sha is passed.
  --compose-service-suffix SUFFIX
                          Resolve backend-SUFFIX/frontend-SUFFIX from the A/B
                          Compose overlay instead of backend/frontend.

Resolution order for the recorded application_sha, highest priority first:
  1. --application-sha, when given
  2. the image's org.opencontainers.image.revision label
  3. the image tag (`sha-<40>` or a short SHA), verified against --git-dir

Note for readers tracing production behaviour: cd-deploy.yml and deploy.sh
both determine the commit themselves and pass --application-sha, so step 3
normally runs only when this script is invoked directly. cd-deploy.yml
deliberately passes nothing when the image carries a revision label, so that
step 2 — the image's own statement about itself — stays authoritative.
EOF
}

compose_dir=""
output=""
application_sha=""
git_dir=""
git_dir_set=0
compose_service_suffix=""

while (($#)); do
  case "$1" in
    --compose-dir) compose_dir=${2:?}; shift 2 ;;
    --output) output=${2:?}; shift 2 ;;
    --application-sha) application_sha=${2:?}; shift 2 ;;
    --git-dir) git_dir=${2-}; git_dir_set=1; shift 2 ;;
    --compose-service-suffix) compose_service_suffix=${2:?}; shift 2 ;;
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

backend_service="backend"
web_service="frontend"
if [ -n "$compose_service_suffix" ]; then
  if ! [[ "$compose_service_suffix" =~ ^[a-z0-9][a-z0-9-]*$ ]]; then
    echo "invalid --compose-service-suffix: $compose_service_suffix" >&2
    exit 2
  fi
  backend_service="backend-${compose_service_suffix}"
  web_service="frontend-${compose_service_suffix}"
fi

compose() {
  local args=( -f "docker-compose.selfhost.yml" )
  if [ -n "$compose_service_suffix" ]; then
    args+=( -f "deploy/cd/docker-compose.ab.yml" )
  fi
  (cd "$compose_dir" && docker compose "${args[@]}" "$@")
}

# Default --git-dir to this script's own checkout when it is running from one.
# On C00 the script is scp'd to a bare temp directory and this stays empty,
# which is correct — the caller passes --git-dir (or --application-sha) when
# tag resolution is needed. An explicit `--git-dir ""` opts out of the default
# and models that bare-directory invocation.
if [ -z "$git_dir" ] && [ "$git_dir_set" -eq 0 ]; then
  self_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  if git -C "$self_dir" rev-parse --git-dir >/dev/null 2>&1; then
    git_dir="$self_dir"
  fi
fi

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

backend_ref="$(service_image "$backend_service")"
web_ref="$(service_image "$web_service")"
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

# application_sha_from_tag recovers the deployed commit from the image TAG when
# the image carries no org.opencontainers.image.revision label. D1 tags every
# image it publishes `sha-<40-char-commit>`, and C00's current locally-built
# images are tagged with the 8-character short SHA (`…:ff152610`) — neither
# form is a full SHA on its own, so resolve the tag against the repository's
# git history to get the 40-character commit the tuple schema requires.
#
# This exists because the label is not guaranteed: an image built outside D1
# (a `make selfhost` build, or any path that does not pass the
# --label org.opencontainers.image.revision docker/buildx flag) has an empty
# label, and the first real D2 run failed exactly there — "could not determine
# a 40-character application_sha (got '<empty>')" — even though the tag named
# the commit unambiguously. Prefer the label when present; it is the image's
# own statement about itself. Fall back to the tag only when it resolves to a
# real commit, never to a guess.
application_sha_from_tag() {
  local ref=$1 tag candidate
  tag="${ref##*:}"
  case "$tag" in
    sha-*) candidate="${tag#sha-}" ;;
    *) candidate="$tag" ;;
  esac
  # Only hex-looking tags can be a commit; anything else (`latest`, a version
  # tag) must not be fed to rev-parse, which would happily resolve a branch or
  # tag name of the same spelling and record something that is not the
  # deployed commit.
  if ! [[ "$candidate" =~ ^[a-f0-9]{7,40}$ ]]; then
    printf ''
    return 0
  fi
  # Verify against the repository whenever one is available, for BOTH the
  # abbreviated and the full-length case. A syntactically valid `sha-<40 hex>`
  # tag is not proof the commit exists — a mis-tag, a corrupted publish or
  # manual drift can produce one — and recording it unchecked would contradict
  # this function's own rule of never resolving to a guess.
  if [ -n "$git_dir" ]; then
    # ^{commit} forces a commit (not a tag object), and the trailing check
    # keeps an ambiguous or unknown abbreviation from silently becoming a
    # different commit.
    verified="$(git -C "$git_dir" rev-parse --verify --quiet "${candidate}^{commit}" 2>/dev/null || true)"
    if [[ "$verified" =~ ^[a-f0-9]{40}$ ]]; then
      printf '%s' "$verified"
      return 0
    fi
    # A repository was available and disagreed: refuse rather than fall back to
    # the unverified spelling.
    printf ''
    return 0
  fi
  # No repository to check against. Only a full-length SHA is self-describing
  # enough to use unverified; an abbreviation cannot be expanded at all.
  if [ ${#candidate} -eq 40 ]; then
    printf '%s' "$candidate"
    return 0
  fi
  printf ''
}

if [ -z "$application_sha" ]; then
  application_sha="$(docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$backend_ref" 2>/dev/null)"
fi
if ! [[ "$application_sha" =~ ^[a-f0-9]{40}$ ]]; then
  application_sha="$(application_sha_from_tag "$backend_ref")"
fi
if ! [[ "$application_sha" =~ ^[a-f0-9]{40}$ ]]; then
  echo "could not determine a 40-character application_sha for $backend_ref" >&2
  echo "  the image carries no org.opencontainers.image.revision label and its tag did not resolve to a commit" >&2
  echo "  pass --application-sha explicitly" >&2
  exit 1
fi

compose_sha="sha256:$(sha256sum "$compose_file" | awk '{print $1}')"
backend_config_hash="$(service_config_hash "$backend_service")"
web_config_hash="$(service_config_hash "$web_service")"

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
