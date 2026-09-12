#!/usr/bin/env node

import { readFileSync, writeFileSync } from "node:fs";

const digestPattern = /^sha256:[a-f0-9]{64}$/;
const shaPattern = /^[a-f0-9]{40}$/;
const imagePattern = /^(ghcr\.io\/[a-z0-9._/-]+)@(sha256:[a-f0-9]{64})$/;
const kinds = new Set(["build-evidence", "release-candidate"]);

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

function assertImage(name, value) {
  if (!imagePattern.test(value)) fail(`${name} must be an immutable ghcr.io image digest`);
}

function validate(manifest) {
  if (manifest.schema_version !== 1) fail("unsupported manifest schema_version");
  if (!kinds.has(manifest.kind)) fail("manifest kind must be build-evidence or release-candidate");
  if (typeof manifest.repository !== "string" || !/^[\w.-]+\/[\w.-]+$/.test(manifest.repository)) {
    fail("repository must be an owner/name pair");
  }
  if (!shaPattern.test(manifest.source_sha)) fail("source_sha must be a 40-character lowercase Git SHA");
  if (manifest.architecture !== "linux/amd64") fail("architecture must be linux/amd64");
  assertDigest("configuration_sha256", manifest.configuration_sha256);
  assertDigest("migration_inventory_sha256", manifest.migration_inventory_sha256);
  assertImage("images.backend", manifest.images?.backend);
  assertImage("images.web", manifest.images?.web);

  for (const [name, ref] of Object.entries(manifest.images)) {
    const [, , digest] = imagePattern.exec(ref);
    if (digest !== manifest.image_digests?.[name]) fail(`${name} digest does not match its immutable image reference`);
  }
}

function create(args) {
  const manifest = {
    architecture: option("--architecture", args),
    configuration_sha256: option("--configuration-sha256", args),
    image_digests: {},
    images: {
      backend: option("--backend-image", args),
      web: option("--web-image", args),
    },
    kind: option("--kind", args),
    migration_inventory_sha256: option("--migration-inventory-sha256", args),
    repository: option("--repository", args),
    schema_version: 1,
    source_sha: option("--source-sha", args),
  };

  for (const [name, ref] of Object.entries(manifest.images)) {
    manifest.image_digests[name] = imagePattern.exec(ref)?.[2];
  }
  validate(manifest);
  writeFileSync(option("--output", args), `${JSON.stringify(manifest, null, 2)}\n`);
}

function verify(args) {
  const manifest = JSON.parse(readFileSync(option("--manifest", args), "utf8"));
  validate(manifest);
  const expected = [
    ["--expected-repository", "repository"],
    ["--expected-source-sha", "source_sha"],
    ["--expected-configuration-sha256", "configuration_sha256"],
    ["--expected-migration-inventory-sha256", "migration_inventory_sha256"],
  ];
  for (const [flag, property] of expected) {
    const index = args.indexOf(flag);
    if (index !== -1 && manifest[property] !== args[index + 1]) {
      fail(`${property} does not match ${flag}`);
    }
  }
  process.stdout.write(`${JSON.stringify(manifest)}\n`);
}

try {
  const [command, ...args] = process.argv.slice(2);
  if (command === "create") create(args);
  else if (command === "verify") verify(args);
  else fail("usage: release-manifest.mjs <create|verify> [options]");
} catch (error) {
  process.stderr.write(`release manifest: ${error.message}\n`);
  process.exitCode = 1;
}
