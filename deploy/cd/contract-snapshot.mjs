#!/usr/bin/env node

// Contract-migration snapshot helpers for cutover.sh (CHE-7486 N5a). The
// controller does the database work (psql through `compose exec`); this file
// only reads the packet's contract list and writes and checks the evidence
// files, so the formats live in one tested place.
//
//   list --packet P
//       One line per contract migration: version TAB tables(csv) TAB restore.
//   entry --version V --table T --rows N --copy FILE [--absent] [--columns-file F]
//       One manifest table entry as JSON: the copy file's SHA-256 is read
//       here. Columns come one per line from --columns-file.
//   manifest --out FILE --attempt-id ID --source-sha SHA --taken-at TS
//            --ledger-latest V --ledger-rows N        (entries as JSON lines on stdin)
//       Writes manifest.json, mode 0600, never over an existing file.
//   verify --manifest FILE
//       For every table: the copy file's line count equals `rows` and a fresh
//       SHA-256 equals the manifest's. Exit 1 on the first mismatch.
//   record --out FILE --attempt-id ID --source-sha SHA --versions CSV
//          --snapshot-dir DIR --manifest-sha256 H --incumbent-image-id ID
//          --incumbent-digest D --candidate-digest D --written-at TS
//       Writes the attempt record body, mode 0600, never over an existing file.

import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

function fail(message) {
  throw new Error(message);
}

function option(name, args, { required = true } = {}) {
  const index = args.indexOf(name);
  if (index === -1 || args[index + 1] === undefined) {
    if (required) fail(`missing ${name}`);
    return undefined;
  }
  return args[index + 1];
}

const sha256 = (bytes) => `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
const lineCount = (bytes) => bytes.reduce((n, byte) => (byte === 0x0a ? n + 1 : n), 0);

function list(args) {
  const packet = JSON.parse(readFileSync(option("--packet", args), "utf8"));
  const entries = packet.contract_migrations ?? [];
  if (!Array.isArray(entries)) fail("packet contract_migrations is not a list");
  for (const entry of entries) {
    if (typeof entry.version !== "string" || !/^[A-Za-z0-9_]+$/.test(entry.version)) fail(`bad contract version ${JSON.stringify(entry.version)}`);
    if (!Array.isArray(entry.snapshot_tables) || entry.snapshot_tables.length === 0 ||
        !entry.snapshot_tables.every((t) => typeof t === "string" && /^[a-z_][a-z0-9_]*$/.test(t))) {
      fail(`bad snapshot_tables for ${entry.version}`);
    }
    process.stdout.write(`${entry.version}\t${entry.snapshot_tables.join(",")}\t${entry.restore}\n`);
  }
}

function entry(args) {
  const copy = readFileSync(option("--copy", args));
  const columnsFile = option("--columns-file", args, { required: false });
  const columns = columnsFile ? readFileSync(columnsFile, "utf8").split("\n").filter(Boolean) : [];
  const rows = Number(option("--rows", args));
  if (!Number.isInteger(rows) || rows < 0) fail("--rows must be a non-negative integer");
  const out = {
    contract_version: option("--version", args),
    table: option("--table", args),
    columns,
    rows,
    sha256: sha256(copy),
  };
  if (args.includes("--absent")) out.absent = true;
  process.stdout.write(`${JSON.stringify(out)}\n`);
}

function manifest(args) {
  const tables = readFileSync(0, "utf8").split("\n").filter(Boolean).map((line) => JSON.parse(line));
  if (tables.length === 0) fail("manifest needs at least one table entry");
  const body = {
    attempt_id: option("--attempt-id", args),
    source_sha: option("--source-sha", args),
    taken_at: option("--taken-at", args),
    ledger: { latest: option("--ledger-latest", args), rows: Number(option("--ledger-rows", args)) },
    tables,
  };
  writeFileSync(option("--out", args), `${JSON.stringify(body, null, 2)}\n`, { flag: "wx", mode: 0o600 });
}

function verify(args) {
  const path = option("--manifest", args);
  const body = JSON.parse(readFileSync(path, "utf8"));
  for (const t of body.tables ?? []) {
    const bytes = readFileSync(join(dirname(path), `${t.table}.copy`));
    if (lineCount(bytes) !== t.rows) fail(`${t.table}.copy has ${lineCount(bytes)} lines, manifest rows is ${t.rows}`);
    if (sha256(bytes) !== t.sha256) fail(`${t.table}.copy SHA-256 ${sha256(bytes)} differs from the manifest's ${t.sha256}`);
  }
}

function record(args) {
  const body = {
    attempt_id: option("--attempt-id", args),
    source_sha: option("--source-sha", args),
    contract_versions: option("--versions", args).split(","),
    snapshot_dir: option("--snapshot-dir", args),
    manifest_sha256: option("--manifest-sha256", args),
    incumbent_backend_image_id: option("--incumbent-image-id", args),
    incumbent_backend_digest: option("--incumbent-digest", args),
    candidate_backend_digest: option("--candidate-digest", args),
    written_at: option("--written-at", args),
  };
  writeFileSync(option("--out", args), `${JSON.stringify(body, null, 2)}\n`, { flag: "wx", mode: 0o600 });
}

try {
  const [command, ...args] = process.argv.slice(2);
  const commands = { list, entry, manifest, verify, record };
  if (!commands[command]) fail("usage: contract-snapshot.mjs <list|entry|manifest|verify|record> [options]");
  commands[command](args);
} catch (error) {
  process.stderr.write(`contract snapshot: ${error.message}\n`);
  process.exitCode = 1;
}
