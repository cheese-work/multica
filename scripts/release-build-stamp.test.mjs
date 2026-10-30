import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const root = new URL("../", import.meta.url);
const rootPath = fileURLToPath(root);
const helperPath = fileURLToPath(new URL("scripts/release-stamp.sh", root));
const workflow = readFileSync(new URL(".github/workflows/release.yml", root), "utf8");
const cdWorkflow = readFileSync(new URL(".github/workflows/cd-qualification.yml", root), "utf8");
const dockerfile = readFileSync(new URL("Dockerfile", root), "utf8");
const dockerfileWeb = readFileSync(new URL("Dockerfile.web", root), "utf8");
const goreleaser = readFileSync(new URL(".goreleaser.yml", root), "utf8");

test("UPSTREAM_VERSION tracks the single plain upstream semver at the repo root", () => {
  const upstreamVersion = readFileSync(new URL("UPSTREAM_VERSION", root), "utf8").trim();
  assert.match(upstreamVersion, /^\d+\.\d+\.\d+$/);
  assert.equal(upstreamVersion, "0.7.1");
});

// The workflow tests below pick tags relative to the file, so a sync that
// moves UPSTREAM_VERSION does not break them.
const currentUpstream = readFileSync(new URL("UPSTREAM_VERSION", root), "utf8").trim();
const [upMajor, upMinor, upPatch] = currentUpstream.split(".").map(Number);
const otherTag = `v${upMajor}.${upMinor}.${upPatch + 1}`;

test("release-stamp.sh formats an input commit timestamp in ICT (UTC+7) across day, month, and year rollovers", () => {
  for (const [isoTime, expected] of [
    ["2026-10-21T01:56:00Z", "20261021-0856"],
    ["2026-10-31T16:59:00Z", "20261031-2359"],
    ["2026-10-31T17:30:00Z", "20261101-0030"],
    ["2026-12-31T17:05:00Z", "20270101-0005"],
    ["2026-10-21T08:56:00+07:00", "20261021-0856"],
  ]) {
    const plain = spawnSync("sh", [helperPath, isoTime], { encoding: "utf8" });
    assert.equal(plain.status, 0, plain.stderr);
    assert.equal(plain.stdout.trim(), expected);

    const prefixed = spawnSync("sh", [helperPath, "0.6.3", isoTime], { encoding: "utf8" });
    assert.equal(prefixed.status, 0, prefixed.stderr);
    assert.equal(prefixed.stdout.trim(), `0.6.3-${expected}`);
  }
});

test("release-stamp.sh defaults to current ICT time and supports an optional version prefix", () => {
  const plain = spawnSync("sh", [helperPath], { encoding: "utf8" });
  assert.equal(plain.status, 0, plain.stderr);
  assert.match(plain.stdout.trim(), /^\d{8}-\d{4}$/);

  const prefixed = spawnSync("sh", [helperPath, "0.6.3"], { encoding: "utf8" });
  assert.equal(prefixed.status, 0, prefixed.stderr);
  assert.match(prefixed.stdout.trim(), /^0\.6\.3-\d{8}-\d{4}$/);
});

test("CD qualification stamps plain upstream version, ICT commit build stamp, and commit SHA separately while keeping sha-<GITHUB_SHA> image tags", () => {
  assert.match(dockerfile, /^ARG VERSION=dev$/m);
  assert.match(dockerfile, /^ARG BUILD=$/m);
  assert.match(dockerfile, /^ARG COMMIT=unknown$/m);
  assert.match(dockerfile, /-X main\.version=\$\{VERSION\} -X main\.build=\$\{BUILD\} -X main\.commit=\$\{COMMIT\}/);

  assert.match(dockerfileWeb, /^ARG NEXT_PUBLIC_APP_VERSION=dev$/m);
  assert.match(dockerfileWeb, /^ARG NEXT_PUBLIC_APP_BUILD$/m);
  assert.match(dockerfileWeb, /^ENV NEXT_PUBLIC_APP_BUILD=\$NEXT_PUBLIC_APP_BUILD$/m);
  assert.match(dockerfileWeb, /^ARG NEXT_PUBLIC_APP_COMMIT$/m);
  assert.match(dockerfileWeb, /^ENV NEXT_PUBLIC_APP_COMMIT=\$NEXT_PUBLIC_APP_COMMIT$/m);

  assert.match(cdWorkflow, /tr -d '\[:space:\]' < UPSTREAM_VERSION/);
  assert.match(cdWorkflow, /if \[\[ ! "\$upstream_version" =~ \^\[0-9\]\+\\\.\[0-9\]\+\\\.\[0-9\]\+\$ \]\]; then/);
  assert.match(cdWorkflow, /git show -s --format=%cI "\$GITHUB_SHA"/);
  assert.match(cdWorkflow, /sh scripts\/release-stamp\.sh "\$build_date"/);
  assert.match(cdWorkflow, /--build-arg "VERSION=\$\{upstream_version\}"/);
  assert.match(cdWorkflow, /--build-arg "BUILD=\$\{build_stamp\}"/);
  assert.match(cdWorkflow, /--build-arg "COMMIT=\$\{GITHUB_SHA\}"/);
  assert.match(cdWorkflow, /--build-arg "NEXT_PUBLIC_APP_VERSION=\$\{upstream_version\}"/);
  assert.match(cdWorkflow, /--build-arg "NEXT_PUBLIC_APP_BUILD=\$\{build_stamp\}"/);
  assert.match(cdWorkflow, /--build-arg "NEXT_PUBLIC_APP_COMMIT=\$\{GITHUB_SHA\}"/);
  assert.match(cdWorkflow, /--build-arg "NEXT_PUBLIC_BUILD_DATE=\$\{build_date\}"/);
  assert.match(cdWorkflow, /--tag "\$\{BACKEND_IMAGE\}:sha-\$\{GITHUB_SHA\}"/);
  assert.match(cdWorkflow, /--tag "\$\{WEB_IMAGE\}:sha-\$\{GITHUB_SHA\}"/);
  assert.doesNotMatch(cdWorkflow, /VERSION=sha-\$\{GITHUB_SHA\}/);
  assert.doesNotMatch(cdWorkflow, /NEXT_PUBLIC_APP_VERSION=sha-\$\{GITHUB_SHA\}/);
});

test("release artifacts share one commit-time ICT stamp and UPSTREAM_VERSION from the canonical helper", () => {
  assert.equal(workflow.match(/sh scripts\/release-stamp\.sh/g)?.length, 1);
  assert.match(workflow, /tr -d '\[:space:\]' < UPSTREAM_VERSION/);
  assert.match(workflow, /git show -s --format=%cI "\$GITHUB_SHA"/);
  assert.match(workflow, /upstream_version: \$\{\{ steps\.build_meta\.outputs\.upstream_version \}\}/);
  assert.match(workflow, /build_stamp: \$\{\{ steps\.build_meta\.outputs\.build_stamp \}\}/);
  assert.match(workflow, /UPSTREAM_VERSION: \$\{\{ needs\.verify\.outputs\.upstream_version \}\}/);
  assert.match(workflow, /RELEASE_BUILD_STAMP: \$\{\{ needs\.verify\.outputs\.build_stamp \}\}/);
  assert.match(
    goreleaser,
    /main\.version=\{\{if index \.Env "UPSTREAM_VERSION"\}\}\{\{index \.Env "UPSTREAM_VERSION"\}\}\{\{else\}\}\{\{\.Version\}\}\{\{end\}\}\{\{if index \.Env "RELEASE_BUILD_STAMP"\}\}-\{\{index \.Env "RELEASE_BUILD_STAMP"\}\}\{\{end\}\}/,
  );
  assert.match(workflow, /VERSION=\$\{\{ needs\.verify\.outputs\.upstream_version \}\}/);
  assert.match(workflow, /BUILD=\$\{\{ needs\.verify\.outputs\.build_stamp \}\}/);
  assert.match(workflow, /NEXT_PUBLIC_APP_VERSION=\$\{\{ needs\.verify\.outputs\.upstream_version \}\}/);
  assert.match(workflow, /NEXT_PUBLIC_APP_BUILD=\$\{\{ needs\.verify\.outputs\.build_stamp \}\}/);
  assert.match(workflow, /NEXT_PUBLIC_APP_COMMIT=\$\{\{ github\.sha \}\}/);
  assert.equal(workflow.match(/type=raw,value=\$\{\{ needs\.verify\.outputs\.display_version \}\}/g)?.length, 2);
});

test("release workflow verifies that tag core version matches UPSTREAM_VERSION and rejects mismatches", () => {
  const stepMatch = workflow.match(/- name: Compute release build stamp[\s\S]*?run: \|\n([\s\S]*?)\n\n      - name: Setup Go/);
  assert.ok(stepMatch, "Compute release build stamp step not found in release.yml");
  const scriptBody = stepMatch[1]
    .split("\n")
    .map((line) => line.replace(/^ {10}/, ""))
    .join("\n");

  const tempDir = mkdtempSync(join(tmpdir(), "multica-release-tag-guard-"));
  try {
    const outFile = join(tempDir, "github_output");
    writeFileSync(outFile, "");

    const mismatch = spawnSync("bash", ["-euo", "pipefail", "-c", scriptBody], {
      cwd: rootPath,
      encoding: "utf8",
      env: {
        ...process.env,
        GITHUB_REF_NAME: otherTag,
        GITHUB_SHA: "HEAD",
        GITHUB_OUTPUT: outFile,
      },
    });
    assert.notEqual(mismatch.status, 0, `Expected mismatch between tag ${otherTag} and UPSTREAM_VERSION ${currentUpstream} to fail`);
    assert.match(`${mismatch.stdout}\n${mismatch.stderr}`, /does not match UPSTREAM_VERSION/);

    const prereleaseMatch = spawnSync("bash", ["-euo", "pipefail", "-c", scriptBody], {
      cwd: rootPath,
      encoding: "utf8",
      env: {
        ...process.env,
        GITHUB_REF_NAME: `v${currentUpstream}-rc1`,
        GITHUB_SHA: "HEAD",
        GITHUB_OUTPUT: outFile,
      },
    });
    assert.equal(prereleaseMatch.status, 0, prereleaseMatch.stderr);
    const outContent = readFileSync(outFile, "utf8");
    assert.ok(outContent.split("\n").includes(`upstream_version=${currentUpstream}`));
    assert.match(outContent, new RegExp(`^display_version=${currentUpstream.replaceAll(".", "\\.")}-\\d{8}-\\d{4}$`, "m"));
  } finally {
    rmSync(tempDir, { recursive: true, force: true });
  }
});

test("GoReleaser builds snapshots without a release stamp", { skip: !process.env.GORELEASER_BIN }, () => {
  const env = { ...process.env };
  delete env.RELEASE_BUILD_STAMP;
  delete env.UPSTREAM_VERSION;
  const directory = mkdtempSync(join(tmpdir(), "multica-release-stamp-"));
  try {
    const config = join(directory, ".goreleaser.yml");
    writeFileSync(config, `${goreleaser}\ndist: ${JSON.stringify(join(directory, "dist"))}\n`);
    execFileSync(process.env.GORELEASER_BIN, ["build", "--config", config, "--snapshot", "--single-target"], {
      cwd: rootPath,
      env,
    });
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("Makefile local development builds do not invoke the release stamp helper", () => {
  const makefile = readFileSync(new URL("Makefile", root), "utf8");
  assert.doesNotMatch(makefile, /release-stamp\.sh|RELEASE_BUILD_STAMP/);
});
