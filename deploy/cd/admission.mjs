#!/usr/bin/env node

import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";

const requiredChecks = ["backend", "frontend", "mobile", "cd-qualification"];

function fail(message) {
  throw new Error(message);
}

function option(name, args) {
  const index = args.indexOf(name);
  if (index === -1 || !args[index + 1]) fail(`missing ${name}`);
  return args[index + 1];
}

function readJSON(path) {
  return JSON.parse(readFileSync(path, "utf8"));
}

function manifestFor(args) {
  const result = spawnSync(process.execPath, [
    "deploy/cd/release-manifest.mjs",
    "verify",
    "--manifest",
    option("--manifest", args),
    "--expected-repository",
    option("--repository", args),
    "--expected-source-sha",
    option("--source-sha", args),
    "--expected-configuration-sha256",
    option("--configuration-sha256", args),
    "--expected-migration-inventory-sha256",
    option("--migration-inventory-sha256", args),
    "--expected-baseline-tuple-sha256",
    tupleDigest(option("--baseline-tuple", args)),
  ], { encoding: "utf8" });
  if (result.status !== 0) fail(result.stderr.trim() || "manifest verification failed");
  return JSON.parse(result.stdout);
}

function tupleDigest(path) {
  const result = spawnSync(process.execPath, [
    "deploy/cd/tuple-snapshot.mjs",
    "digest",
    "--snapshot",
    path,
  ], { encoding: "utf8" });
  if (result.status !== 0) fail(result.stderr.trim() || "tuple snapshot verification failed");
  return result.stdout.trim();
}

function verify(args) {
  const manifest = manifestFor(args);
  if (manifest.kind !== "release-candidate") {
    fail("build-evidence manifests are not deployable; issue a current-tuple release-candidate manifest");
  }

  const event = readJSON(option("--event", args));
  if (event.event_name !== "push") fail("event_name must be push");
  if (event.ref !== "refs/heads/main") fail("event ref must be refs/heads/main");
  if (event.repository !== manifest.repository) fail("event repository does not match manifest repository");
  if (event.sha !== manifest.source_sha) fail("event SHA does not match manifest source SHA");

  const provenance = readJSON(option("--provenance", args));
  for (const name of ["backend", "web"]) {
    const image = provenance.images?.[name];
    if (image?.digest !== manifest.image_digests[name]) fail(`${name} provenance digest does not match manifest`);
    if (image?.architecture !== manifest.architecture) fail(`${name} provenance architecture does not match manifest`);
    if (image?.repository !== manifest.repository) fail(`${name} provenance repository does not match manifest`);
    if (image?.source_sha !== manifest.source_sha) fail(`${name} provenance source SHA does not match manifest`);
  }

  const checks = readJSON(option("--checks", args));
  if (checks.sha !== manifest.source_sha) fail("checks were recorded for a different source SHA");
  for (const name of requiredChecks) {
    if (checks.contexts?.[name] !== "success") fail(`required check ${name} is not success`);
  }

  process.stdout.write(`${JSON.stringify({ admitted: true, source_sha: manifest.source_sha })}\n`);
}

try {
  const [command, ...args] = process.argv.slice(2);
  if (command !== "verify") fail("usage: admission.mjs verify [options]");
  verify(args);
} catch (error) {
  process.stderr.write(`admission: ${error.message}\n`);
  process.exitCode = 1;
}
