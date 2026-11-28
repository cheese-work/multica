#!/usr/bin/env bash
# Contract guard library (CHE-7486 N5b-1). Sourced by cutover.sh, never run.
#
# While a contract migration is applied, cutover and its incumbent recovery
# never start an image whose tree predates it, whatever cutover-state.json
# says. Two reads make that decision, and an uncertain read refuses:
#
#   contract_applied_read   the applied set: the schema_contracts rows, with
#                           their attempt ids and restore paths
#   contract_image_proof    the proof, made on an image id (never a tag):
#                           the image holds /app/migrations/<version>.up.sql
#                           for every applied version
#
# The caller provides:
#   state_dir        the controller state dir (holds contract-attempts/)
#   contract_psql    function: runs one SQL statement through psql with
#                    ON_ERROR_STOP=1 and prints the rows (no table headers)
#
# No function here reads cutover-state.json.

# contract_applied_read sets:
#   contract_applied      one "version|attempt_id|restore" line per applied
#                         contract migration, newest version first; empty
#                         when none is applied. attempt_id is empty when the
#                         row has none.
#   contract_read_error   why the read is uncertain (empty on success)
# Returns 0 for a certain read, 1 for an uncertain one. The read is
# uncertain when psql fails, the ledger cannot be read, an attempt record
# cannot be read, or an attempt record lists a version that has a ledger row
# but no schema_contracts row. A missing schema_contracts table (to_regclass
# NULL) is an empty set only when no attempt record lists a version with a
# ledger row.
contract_applied_read() {
  local present rows line version attempt restore record versions count records_dir names name
  contract_applied=""
  contract_read_error=""

  present="$(contract_psql "SELECT to_regclass('schema_contracts') IS NOT NULL")" || {
    contract_read_error="cannot look up schema_contracts"
    return 1
  }
  case "$present" in
    t)
      rows="$(contract_psql "SELECT version || '|' || coalesce(attempt_id, '') || '|' || restore FROM schema_contracts")" || {
        contract_read_error="cannot read schema_contracts"
        return 1
      }
      while IFS= read -r line; do
        [ -n "$line" ] || continue
        IFS='|' read -r version attempt restore <<<"$line"
        if ! [[ "$version" =~ ^[A-Za-z0-9_]+$ ]] || ! [[ "$attempt" =~ ^([0-9a-f]{32})?$ ]] ||
          ! [[ "$restore" =~ ^[A-Za-z0-9._/-]+$ ]] || [[ "$line" == *"|"*"|"*"|"* ]]; then
          contract_read_error="unreadable schema_contracts row '$line'"
          return 1
        fi
      done <<<"$rows"
      contract_applied="$(printf '%s\n' "$rows" | sed '/^$/d' | LC_ALL=C sort -t '|' -k1,1 -r)"
      ;;
    f) ;;
    *)
      contract_read_error="unreadable schema_contracts lookup '$present'"
      return 1
      ;;
  esac

  # The ledger must be readable even when no attempt record exists: an
  # unreadable ledger is never an empty set.
  count="$(contract_psql "SELECT count(*) FROM schema_migrations")" || {
    contract_read_error="cannot read the ledger"
    return 1
  }
  [[ "$count" =~ ^[0-9]+$ ]] || {
    contract_read_error="unreadable ledger answer '$count'"
    return 1
  }

  # Every attempt record (the final .json files, not a .json.tmp that was
  # never renamed) must be readable, and a version it lists that has a ledger
  # row must also have a schema_contracts row. A directory that exists but
  # cannot be listed is uncertain, not empty.
  records_dir="$state_dir/contract-attempts"
  names=""
  if [ -e "$records_dir" ]; then
    if [ ! -d "$records_dir" ] || ! names="$(ls -A "$records_dir" 2>/dev/null)"; then
      contract_read_error="attempt records under $records_dir cannot be listed"
      return 1
    fi
  fi
  while IFS= read -r name; do
    case "$name" in *.json) ;; *) continue ;; esac
    record="$records_dir/$name"
    versions="$(node -e '
      const record = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"));
      const versions = record.contract_versions;
      if (!Array.isArray(versions) || versions.length === 0 || !versions.every((v) => typeof v === "string" && /^[A-Za-z0-9_]+$/.test(v))) process.exit(1);
      process.stdout.write(versions.join("\n"));
    ' "$record" 2>/dev/null)" || {
      contract_read_error="attempt record $record cannot be read"
      return 1
    }
    while IFS= read -r version; do
      count="$(contract_psql "SELECT count(*) FROM schema_migrations WHERE version = '$version'")" || {
        contract_read_error="cannot read the ledger for $version"
        return 1
      }
      [[ "$count" =~ ^[0-9]+$ ]] || {
        contract_read_error="unreadable ledger answer for $version"
        return 1
      }
      if [ "$count" -gt 0 ] && ! grep -q "^${version}|" <<<"$contract_applied"; then
        contract_read_error="attempt record $record lists $version, which has a ledger row but no schema_contracts row"
        return 1
      fi
    done <<<"$versions"
  done <<<"$names"
  return 0
}

# contract_image_proof IMAGE_ID: the image holds the migration file of every
# applied version (reads contract_applied). 0 proven, 3 refused (a file is
# missing), 1 uncertain (the proof could not run, or no usable image id).
# With an empty applied set the proof passes without a container.
contract_image_proof() {
  local image=$1 line version rc=0
  local -a versions=()
  [ -n "$contract_applied" ] || return 0
  [ -n "$image" ] && [[ "$image" != -* ]] || return 1
  while IFS= read -r line; do
    IFS='|' read -r version _ <<<"$line"
    versions+=("$version")
  done <<<"$contract_applied"
  docker run --rm --network none --entrypoint /bin/sh "$image" \
    -c 'for v; do test -f "/app/migrations/$v.up.sql" || exit 10; done' sh "${versions[@]}" >/dev/null 2>&1 || rc=$?
  case "$rc" in
    0) return 0 ;;
    10) return 3 ;;
    *) return 1 ;;
  esac
}

# contract_applied_versions / contract_applied_attempts print the applied set
# as comma lists, newest version first (the attempt is "none" when the row has
# no attempt id).
contract_applied_versions() {
  local line version out=""
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    IFS='|' read -r version _ <<<"$line"
    out="${out:+$out,}$version"
  done <<<"$contract_applied"
  printf '%s' "$out"
}

contract_applied_attempts() {
  local line attempt out=""
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    IFS='|' read -r _ attempt _ <<<"$line"
    out="${out:+$out,}${attempt:-none}"
  done <<<"$contract_applied"
  printf '%s' "$out"
}

# contract_recovery_text names the two recovery routes for the applied set:
# the restore path and the snapshot directory of each applied version.
contract_recovery_text() {
  local line attempt restore restores="" snapshots=""
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    IFS='|' read -r _ attempt restore <<<"$line"
    restores="${restores:+$restores,}$restore"
    snapshots="${snapshots:+$snapshots,}contract-snapshots/${attempt:-<no attempt id>}"
  done <<<"$contract_applied"
  printf '%s' "recovery: route 1, dispatch cd-deploy with cutover_mode recover-forward and a candidate that holds it, or route 2, restore with ${restores} and snapshot \`${snapshots}\` (README 'Contract migration recovery', authorization per Q1)"
}
