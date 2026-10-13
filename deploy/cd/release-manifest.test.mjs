import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const script = new URL("./release-manifest.mjs", import.meta.url);

function fixture(context, extra = []) {
  const directory = mkdtempSync(join(tmpdir(), "fork-manifest-"));
  context.after(() => rmSync(directory, { recursive: true, force: true }));
  const output = join(directory, "manifest.json");
  const entries = join(directory, "entries.json");
  const bytes = '{"entries":[]}\n';
  writeFileSync(entries, bytes);
  const result = spawnSync(process.execPath, [script.pathname, "create",
    "--kind", "build-evidence", "--repository", "cheese-work/multica",
    "--source-sha", "a".repeat(40), "--architecture", "linux/amd64",
    "--configuration-sha256", `sha256:${"1".repeat(64)}`,
    "--migration-inventory-sha256", `sha256:${"2".repeat(64)}`,
    "--backend-image", `ghcr.io/cheese-work/backend@sha256:${"3".repeat(64)}`,
    "--web-image", `ghcr.io/cheese-work/web@sha256:${"4".repeat(64)}`,
    "--output", output, ...extra.map((value) => value === "ENTRIES" ? entries : value)],
  { encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr);
  return { output, bytes, manifest: JSON.parse(readFileSync(output, "utf8")) };
}

test("manifest records the exact entries bytes as sha256", (context) => {
  const { bytes, manifest } = fixture(context, ["--fork-changelog", "ENTRIES"]);
  assert.equal(manifest.fork_changelog_sha256, `sha256:${createHash("sha256").update(bytes).digest("hex")}`);
});

test("manifest records absent when the entries file is unavailable", (context) => {
  assert.equal(fixture(context).manifest.fork_changelog_sha256, "absent");
});

test("manifest verification rejects an entries hash mismatch", (context) => {
  const { output } = fixture(context, ["--fork-changelog", "ENTRIES"]);
  const result = spawnSync(process.execPath, [script.pathname, "verify", "--manifest", output,
    "--expected-fork-changelog-sha256", `sha256:${"0".repeat(64)}`], { encoding: "utf8" });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /fork_changelog_sha256 does not match/);
});

test("manifest verification rejects malformed entries hashes", (context) => {
  const { output, manifest } = fixture(context);
  manifest.fork_changelog_sha256 = "not-a-digest";
  writeFileSync(output, JSON.stringify(manifest));
  const result = spawnSync(process.execPath, [script.pathname, "verify", "--manifest", output], { encoding: "utf8" });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /fork_changelog_sha256 must be/);
});
