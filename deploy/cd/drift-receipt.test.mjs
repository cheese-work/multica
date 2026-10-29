import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { spawnSync } from "node:child_process";
import { COMPOSE_FILES, compare, syncComposeFiles } from "./drift-receipt.mjs";

const digest = `sha256:${"a".repeat(64)}`;
const okService = {
  expected_env: ["MULTICA_APP_URL", "DATABASE_URL"],
  container_env: ["PATH", "MULTICA_APP_URL", "DATABASE_URL"],
  image_env: ["PATH"],
  expected_digest: digest,
  repo_digests: [`ghcr.io/cheese-work/multica-web@${digest}`],
};
const okCompose = [{ name: "docker-compose.selfhost.yml", source_sha256: "1", deployed_sha256: "1" }];

test("matching compose bytes, env key names and digests give a match receipt", () => {
  const receipt = compare({ compose: okCompose, services: { "frontend-green": okService } });
  assert.equal(receipt.status, "match");
  assert.deepEqual(receipt.differences, []);
});

test("a deployed compose file that differs from the released source is drift", () => {
  const compose = [{ name: "docker-compose.selfhost.yml", source_sha256: "1", deployed_sha256: "2" }];
  const receipt = compare({ compose, services: { "frontend-green": okService } });
  assert.equal(receipt.status, "drift");
  assert.deepEqual(receipt.differences, ["compose docker-compose.selfhost.yml: deployed bytes differ from released source"]);
});

test("a missing deployed compose file is drift", () => {
  const compose = [{ name: "deploy/cd/docker-compose.ab.yml", source_sha256: "1", deployed_sha256: null }];
  assert.equal(compare({ compose, services: { "frontend-green": okService } }).status, "drift");
});

test("an env key the released compose renders but the container lacks is drift, by name only", () => {
  const service = { ...okService, container_env: ["PATH", "DATABASE_URL"] };
  const receipt = compare({ compose: okCompose, services: { "frontend-green": service } });
  assert.equal(receipt.status, "drift");
  assert.deepEqual(receipt.differences, ["frontend-green env: missing key MULTICA_APP_URL"]);
});

test("a container env key neither rendered nor baked into the image is drift", () => {
  const service = { ...okService, container_env: [...okService.container_env, "STALE_FLAG"] };
  const receipt = compare({ compose: okCompose, services: { "frontend-green": service } });
  assert.deepEqual(receipt.differences, ["frontend-green env: unexpected key STALE_FLAG"]);
});

test("a running image digest that differs from the manifest is drift", () => {
  const service = { ...okService, repo_digests: [`ghcr.io/cheese-work/multica-web@sha256:${"b".repeat(64)}`] };
  const receipt = compare({ compose: okCompose, services: { "frontend-green": service } });
  assert.equal(receipt.status, "drift");
  assert.match(receipt.differences[0], /^frontend-green image: running digest .* does not include manifest digest sha256:a{64}$/);
});

test("the receipt never carries env values", () => {
  const receipt = compare({ compose: okCompose, services: { "frontend-green": okService } });
  assert.equal(JSON.stringify(receipt).includes("="), false);
});

test("syncComposeFiles copies released files over stale ones and is a no-op when already equal", () => {
  const dir = mkdtempSync(join(tmpdir(), "drift-sync-"));
  try {
    const source = join(dir, "source");
    const deployed = join(dir, "deployed");
    for (const name of COMPOSE_FILES) {
      mkdirSync(join(source, name, ".."), { recursive: true });
      writeFileSync(join(source, name), `released ${name}\n`);
    }
    mkdirSync(deployed);
    writeFileSync(join(deployed, "docker-compose.selfhost.yml"), "stale\n");
    syncComposeFiles(source, deployed);
    for (const name of COMPOSE_FILES) {
      assert.equal(readFileSync(join(deployed, name), "utf8"), `released ${name}\n`);
    }
    syncComposeFiles(source, source); // same directory: must not throw
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("an image env key the container no longer carries is drift", () => {
  const service = { ...okService, container_env: ["MULTICA_APP_URL", "DATABASE_URL"] };
  const receipt = compare({ compose: okCompose, services: { "frontend-green": service } });
  assert.deepEqual(receipt.differences, ["frontend-green env: missing key PATH"]);
});

test("compose files absent from both source and deployed are drift, not equal", () => {
  const compose = [{ name: "docker-compose.selfhost.yml", source_sha256: null, deployed_sha256: null }];
  const receipt = compare({ compose, services: { "frontend-green": okService } });
  assert.deepEqual(receipt.differences, ["compose docker-compose.selfhost.yml: released source file is missing"]);
});

test("a receipt that checked no compose files or no services is drift", () => {
  assert.deepEqual(compare({ compose: [], services: { "frontend-green": okService } }).differences, ["compose: no files were compared"]);
  assert.deepEqual(compare({ compose: okCompose, services: {} }).differences, ["services: none were checked"]);
});

test("the check CLI fails closed on empty directories and no services", (context) => {
  const dir = mkdtempSync(join(tmpdir(), "drift-cli-"));
  context.after(() => rmSync(dir, { recursive: true, force: true }));
  writeFileSync(join(dir, "config.json"), "{}");
  writeFileSync(join(dir, "manifest.json"), '{"images":{}}');
  const run = spawnSync(process.execPath, [new URL("./drift-receipt.mjs", import.meta.url).pathname, "check",
    "--source-root", dir, "--compose-dir", dir, "--config", join(dir, "config.json"), "--manifest", join(dir, "manifest.json")],
  { encoding: "utf8" });
  assert.equal(run.status, 1);
  assert.equal(JSON.parse(run.stdout).status, "drift");
});
