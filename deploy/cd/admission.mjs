#!/usr/bin/env node

import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";

const requiredChecks = ["backend", "frontend", "mobile", "cd-qualification"];

// Translate a GitHub Actions path filter into a matcher. Only the subset the
// workflows actually use is supported — `**`, `*` and literals — and anything
// unrecognised is rejected by the caller rather than silently mismatched.
function pathFilterToRegExp(pattern) {
  if (typeof pattern !== "string" || pattern.length === 0) return null;
  if (/[?![\]{}]/.test(pattern)) return null;
  let out = "";
  for (let i = 0; i < pattern.length; i += 1) {
    const char = pattern[i];
    if (char === "*") {
      if (pattern[i + 1] === "*") {
        out += ".*";
        i += 1;
        if (pattern[i + 1] === "/") i += 1;
      } else {
        out += "[^/]*";
      }
    } else if (char === ".") {
      out += "\\.";
    } else if ("+^$()|\\".includes(char)) {
      out += `\\${char}`;
    } else {
      out += char;
    }
  }
  return new RegExp(`^${out}$`);
}

// A required check that never ran because its workflow's path filter excluded
// this commit is not a failure — there is nothing new for it to verify. But
// "it did not run" must never be taken on trust, so admission re-derives that
// conclusion from the same raw evidence a human would use: the commit's
// changed-file list and the filter declared by the workflow itself. Both are
// recorded in the checks file; neither is a boolean the recorder asserts.
function isExcludedByPathFilter(name, checks) {
  const filters = checks.path_filters?.[name];
  const changed = checks.changed_files;
  if (!Array.isArray(filters) || filters.length === 0) return false;
  if (!Array.isArray(changed)) return false;
  // An incomplete file list cannot prove absence of a match. The commits API
  // truncates above 300 files, and the recorder flags that.
  if (checks.changed_files_truncated) return false;
  const matchers = filters.map(pathFilterToRegExp);
  if (matchers.some((matcher) => matcher === null)) return false;
  return !changed.some((file) => matchers.some((matcher) => matcher.test(file)));
}

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
    const conclusion = checks.contexts?.[name];
    if (conclusion === "success") continue;
    // Absent — and only absent — may be excused, and only when the commit
    // provably touches nothing the check's own path filter covers. A check
    // that ran and failed, or is still pending, is always a refusal.
    if (conclusion === undefined && isExcludedByPathFilter(name, checks)) continue;
    fail(`required check ${name} is not success`);
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
