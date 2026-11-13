import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

// Terra's FAIL on 8dfebb045: the X99 consumer must not run scripts straight from a checkout, because it holds the secret key.
const dir = new URL(".", import.meta.url).pathname;
const sh = (cwd, ...a) => execFileSync("git", a, { cwd, encoding: "utf8" }).trim();

test("weekly.sh refuses to run outside the trusted install", () => {
  const state = mkdtempSync(join(tmpdir(), "sw-state-"));
  const r = spawnSync("bash", [join(dir, "weekly.sh"), "run1", "a", "p", "--no-apply"], { env: { ...process.env, SECURITY_WEEKLY_STATE: state }, encoding: "utf8" });
  assert.notEqual(r.status, 0);
  assert.match(r.stderr, /not the trusted install/);
});

test("install-trusted.sh installs a main-reachable revision and refuses any other", () => {
  const root = mkdtempSync(join(tmpdir(), "sw-repo-"));
  const origin = join(root, "origin.git");
  const work = join(root, "work");
  sh(root, "init", "-q", "--bare", "-b", "main", origin);
  mkdirSync(work);
  sh(work, "init", "-q", "-b", "main");
  sh(work, "remote", "add", "origin", origin);
  sh(work, "config", "user.email", "t@t");
  sh(work, "config", "user.name", "t");
  mkdirSync(join(work, "scripts/security-weekly"), { recursive: true });
  writeFileSync(join(work, "scripts/security-weekly/weekly.sh"), "#!/bin/sh\n");
  sh(work, "add", ".");
  sh(work, "commit", "-q", "-m", "main");
  sh(work, "push", "-q", "origin", "HEAD:main");
  const good = sh(work, "rev-parse", "HEAD");
  sh(work, "checkout", "-q", "-b", "side");
  writeFileSync(join(work, "scripts/security-weekly/weekly.sh"), "#!/bin/sh\necho evil\n");
  sh(work, "commit", "-q", "-am", "side");
  sh(work, "push", "-q", "origin", "side");
  const bad = sh(work, "rev-parse", "HEAD");
  const state = mkdtempSync(join(tmpdir(), "sw-state-"));
  const env = { ...process.env, SECURITY_WEEKLY_STATE: state };
  const run = (sha) => spawnSync("bash", [join(dir, "install-trusted.sh"), sha], { cwd: work, env, encoding: "utf8" });
  const refused = run(bad);
  assert.notEqual(refused.status, 0);
  assert.match(refused.stderr, /not on origin\/main/);
  assert.ok(!existsSync(join(state, "trusted", bad)));
  const ok = run(good);
  assert.equal(ok.status, 0, ok.stderr);
  assert.ok(existsSync(join(state, "trusted", good, "weekly.sh")));
  assert.equal(execFileSync("readlink", [join(state, "trusted/current")], { encoding: "utf8" }).trim(), join(state, "trusted", good));
});
