#!/usr/bin/env node

// Expand/contract release-packet contract checks for A/B cutover (CHE-397
// unit 2), extending the D2 tuple/manifest contract with the checks the
// accepted architecture requires for a cutover packet specifically:
// ordered migration versions/checksums, hook/backfill completion, a
// compatible previous image pair, and a minimum rollback version.
//
// This is an explicit EXTENSION, not a replacement of D2's own contract —
// release-manifest.mjs and tuple-snapshot.mjs are unchanged and still the
// authority for manifest/tuple shape and admission. This module additionally
// verifies migration CHECKSUMS against server/migrations/ on disk, which
// deploy/cd/README.md's own "What D2 does not yet cover" section notes D2
// does not do — per the issue's instruction to add that only as an explicit,
// tested extension, never folded silently into the existing D2 files.

import { createHash } from "node:crypto";
import { readdirSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";

function fail(message) {
  throw new Error(message);
}

function option(name, args) {
  const index = args.indexOf(name);
  if (index === -1 || !args[index + 1]) fail(`missing ${name}`);
  return args[index + 1];
}

function optional(name, args) {
  const index = args.indexOf(name);
  return index === -1 ? undefined : args[index + 1];
}

function readJSON(path) {
  return JSON.parse(readFileSync(path, "utf8"));
}

// migrationChecksums reads every *.up.sql file under migrationsDir in
// sorted (i.e. applied) order and returns { version, sha256 } pairs.
// Mirrors server/internal/migrations.AllVersions()/ExtractVersion exactly:
// only ".up.sql" files define the applied-order version set (schema_migrations
// records the "up" version, and Files("up") is what readiness/AllVersions
// walks) — a ".down.sql" sibling is a different file for the same version,
// not a second version, and including it here would double-count every
// migration and produce a version list AllVersions() never actually visits.
function migrationChecksums(migrationsDir) {
  const dir = resolve(migrationsDir);
  const names = readdirSync(dir)
    .filter((name) => name.endsWith(".up.sql"))
    .sort();
  return names.map((name) => {
    const bytes = readFileSync(join(dir, name));
    return {
      version: name.replace(/\.up\.sql$/, ""),
      sha256: `sha256:${createHash("sha256").update(bytes).digest("hex")}`,
    };
  });
}

// validatePacket applies every reject condition the accepted architecture
// names for a release packet: dirty ledger, checksum drift, invalid
// indexes, unknown writers. Each check fails closed (an ambiguous or
// missing input is a rejection, not a pass).
function validatePacket(packet, { migrationsDir }) {
  if (packet.schema_version !== 1) fail("unsupported release packet schema_version");

  if (!Array.isArray(packet.ordered_migrations) || packet.ordered_migrations.length === 0) {
    fail("ordered_migrations must be a non-empty array");
  }
  const onDisk = migrationChecksums(migrationsDir);
  const onDiskByVersion = new Map(onDisk.map((m) => [m.version, m.sha256]));

  // Ordering: the packet's own listed order must match on-disk sorted order
  // for every version it claims to include — a packet cannot assert an
  // out-of-order application sequence that the migrator itself would never
  // produce (schema_migrations is applied in filename-sorted order).
  const packetVersions = packet.ordered_migrations.map((m) => m.version);
  const onDiskVersionsInPacket = onDisk.map((m) => m.version).filter((v) => packetVersions.includes(v));
  if (JSON.stringify(packetVersions) !== JSON.stringify(onDiskVersionsInPacket)) {
    fail("ordered_migrations is not in on-disk applied order — packet lists a different sequence than server/migrations/ would produce");
  }

  for (const entry of packet.ordered_migrations) {
    if (typeof entry.version !== "string" || !entry.version) fail("every ordered_migrations entry needs a version");
    if (!/^sha256:[a-f0-9]{64}$/.test(entry.sha256 ?? "")) {
      fail(`ordered_migrations[${entry.version}].sha256 must be sha256:<64 lowercase hex>`);
    }
    const expected = onDiskByVersion.get(entry.version);
    if (!expected) fail(`ordered_migrations references version ${entry.version}, which does not exist under ${migrationsDir}`);
    if (expected !== entry.sha256) {
      fail(`checksum drift: ordered_migrations[${entry.version}].sha256 (${entry.sha256}) does not match the on-disk migration file (${expected})`);
    }
  }

  // Dirty ledger / unknown writer: every reported ledger row must be one of
  // the packet's own ordered_migrations, and applied_by must be a role this
  // packet explicitly allowlists — an unrecognized writer (a manual psql
  // session, an out-of-band tool) invalidates the packet rather than being
  // silently accepted as "some migration ran".
  if (!Array.isArray(packet.ledger_rows)) fail("ledger_rows must be an array (even if empty)");
  const allowedWriters = new Set(packet.allowed_writers ?? []);
  if (allowedWriters.size === 0) fail("allowed_writers must name at least one recognized migration-writer role");
  for (const row of packet.ledger_rows) {
    if (!packetVersions.includes(row.version)) {
      fail(`ledger contains version ${row.version} not present in ordered_migrations — dirty ledger state`);
    }
    if (!allowedWriters.has(row.applied_by)) {
      fail(`ledger row for ${row.version} was applied_by unknown writer '${row.applied_by}'`);
    }
  }

  // Invalid indexes: any index the packet reports as invalid (a concurrent
  // build that failed or was interrupted, per AGENTS.md's CONCURRENTLY
  // migration rule) is an outright rejection — an invalid index cannot be
  // trusted for either the forward or rollback direction.
  if (!Array.isArray(packet.indexes)) fail("indexes must be an array (even if empty)");
  const invalidIndexes = packet.indexes.filter((i) => i.valid !== true);
  if (invalidIndexes.length > 0) {
    fail(`invalid indexes present: ${invalidIndexes.map((i) => i.name).join(", ")}`);
  }

  // Hooks/backfills: every hook the packet declares must report completed,
  // never partial/pending/failed — a partially-run backfill leaves rows in
  // a shape neither the old nor the new binary was written to expect.
  if (!Array.isArray(packet.hooks)) fail("hooks must be an array (even if empty)");
  const incompleteHooks = packet.hooks.filter((h) => h.status !== "completed");
  if (incompleteHooks.length > 0) {
    fail(`incomplete hooks/backfills present: ${incompleteHooks.map((h) => `${h.name}=${h.status}`).join(", ")}`);
  }

  // Compatible previous image pair + minimum rollback version: required so
  // a cutover packet is only ever admitted alongside a concrete, checked
  // rollback target, never "roll back to whatever happens to be recorded".
  if (typeof packet.previous_image_pair?.backend !== "string" || typeof packet.previous_image_pair?.web !== "string") {
    fail("previous_image_pair.backend and .web are required");
  }
  if (!/^sha256:[a-f0-9]{64}$/.test(packet.previous_image_pair.backend_digest ?? "")) {
    fail("previous_image_pair.backend_digest must be sha256:<64 lowercase hex>");
  }
  if (!/^sha256:[a-f0-9]{64}$/.test(packet.previous_image_pair.web_digest ?? "")) {
    fail("previous_image_pair.web_digest must be sha256:<64 lowercase hex>");
  }
  if (typeof packet.minimum_rollback_version !== "string" || !packet.minimum_rollback_version) {
    fail("minimum_rollback_version is required");
  }
  if (!packetVersions.includes(packet.minimum_rollback_version) && !onDiskByVersion.has(packet.minimum_rollback_version)) {
    fail(`minimum_rollback_version ${packet.minimum_rollback_version} is not a known migration version`);
  }

  return { ok: true };
}

function verify(args) {
  const packet = readJSON(option("--packet", args));
  const migrationsDir = optional("--migrations-dir", args) ?? "server/migrations";
  const result = validatePacket(packet, { migrationsDir });
  process.stdout.write(`${JSON.stringify(result)}\n`);
}

try {
  const [command, ...args] = process.argv.slice(2);
  if (command !== "verify") fail("usage: release-packet.mjs verify --packet PATH [--migrations-dir PATH]");
  verify(args);
} catch (error) {
  process.stderr.write(`release packet: ${error.message}\n`);
  process.exitCode = 1;
}
