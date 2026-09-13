#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

const digestPattern = /^sha256:[a-f0-9]{64}$/;
const shaPattern = /^[a-f0-9]{40}$/;

function fail(message) {
  throw new Error(message);
}

function option(name, args) {
  const index = args.indexOf(name);
  if (index === -1 || !args[index + 1]) fail(`missing ${name}`);
  return args[index + 1];
}

function assertDigest(name, value) {
  if (!digestPattern.test(value)) fail(`${name} must be sha256:<64 lowercase hex characters>`);
}

function assertRedactedDigest(name, value) {
  if (!/^[a-f0-9]{8}$/.test(value?.prefix ?? "") || !/^[a-f0-9]{4}$/.test(value?.suffix ?? "")) {
    fail(`${name} must contain an 8-character prefix and 4-character suffix`);
  }
}

function validate(snapshot) {
  if (snapshot.schema_version !== 1) fail("unsupported tuple snapshot schema_version");
  if (snapshot.role !== "c00-deployment-baseline-read-only-metadata") {
    fail("tuple snapshot is not read-only C00 deployment-baseline metadata");
  }
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/.test(snapshot.captured_at ?? "")) {
    fail("captured_at must be an RFC3339 UTC timestamp");
  }
  if (!shaPattern.test(snapshot.application_sha)) fail("application_sha must be a 40-character lowercase Git SHA");

  for (const name of ["backend", "web"]) {
    const image = snapshot.images?.[name];
    if (typeof image?.reference !== "string" || !image.reference.includes(":")) {
      fail(`images.${name}.reference must be a tagged image reference`);
    }
    assertDigest(`images.${name}.digest`, image.digest);
  }

  if (snapshot.compose?.file !== "docker-compose.selfhost.yml") {
    fail("compose.file must identify docker-compose.selfhost.yml");
  }
  assertDigest("compose.sha256", snapshot.compose?.sha256);
  assertRedactedDigest("compose.service_config_sha256.backend", snapshot.compose?.service_config_sha256?.backend);
  assertRedactedDigest("compose.service_config_sha256.web", snapshot.compose?.service_config_sha256?.web);

  if (!Number.isInteger(snapshot.migration_ledger?.row_count) || snapshot.migration_ledger.row_count < 1) {
    fail("migration_ledger.row_count must be a positive integer");
  }
  if (typeof snapshot.migration_ledger?.latest?.version !== "string" || !snapshot.migration_ledger.latest.version) {
    fail("migration_ledger.latest.version is required");
  }
  if (!/^\d{4}-\d{2}-\d{2}T/.test(snapshot.migration_ledger?.latest?.applied_at ?? "")) {
    fail("migration_ledger.latest.applied_at must be an RFC3339 timestamp");
  }
  assertDigest("migration_ledger.ordered_sha256", snapshot.migration_ledger?.ordered_sha256);
}

function load(path) {
  const bytes = readFileSync(path);
  const snapshot = JSON.parse(bytes);
  validate(snapshot);
  return { snapshot, digest: `sha256:${createHash("sha256").update(bytes).digest("hex")}` };
}

try {
  const [command, ...args] = process.argv.slice(2);
  const result = load(option("--snapshot", args));
  if (command === "verify") process.stdout.write(`${JSON.stringify(result.snapshot)}\n`);
  else if (command === "digest") process.stdout.write(`${result.digest}\n`);
  else fail("usage: tuple-snapshot.mjs <verify|digest> --snapshot PATH");
} catch (error) {
  process.stderr.write(`tuple snapshot: ${error.message}\n`);
  process.exitCode = 1;
}
