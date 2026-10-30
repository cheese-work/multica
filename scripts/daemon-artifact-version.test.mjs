// The version stamped into CI daemon artifacts must stay in the shape the
// runtime capability gates accept (<upstream>-YYYYMMDD-hhmm), with the exact
// commit stamped separately.
// Gates are exercised unmodified: the real packages/core TypeScript gate is
// imported; the Go gate (server/pkg/agent) has its own test over the same script.
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import test from "node:test";

const root = fileURLToPath(new URL("..", import.meta.url));
const script = join(root, "scripts/daemon-artifact-version.sh");
const stampHelper = join(root, "scripts/release-stamp.sh");
const gateModule = pathToFileURL(join(root, "packages/core/runtimes/cli-version.ts")).href;
const env = {
  ...process.env,
  GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_AUTHOR_NAME: "CI test", GIT_AUTHOR_EMAIL: "ci@example.invalid",
  GIT_COMMITTER_NAME: "CI test", GIT_COMMITTER_EMAIL: "ci@example.invalid",
};

function repo(t, { upstreamVersion = "0.6.3", commitDate = "2026-10-21T01:56:00Z" } = {}) {
  const dir = mkdtempSync(join(tmpdir(), "daemon-version-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  mkdirSync(join(dir, "scripts"), { recursive: true });
  copyFileSync(script, join(dir, "scripts/daemon-artifact-version.sh"));
  copyFileSync(stampHelper, join(dir, "scripts/release-stamp.sh"));
  if (upstreamVersion !== null) {
    writeFileSync(join(dir, "UPSTREAM_VERSION"), `${upstreamVersion}\n`);
  }
  const git = (...args) => execFileSync("git", args, {
    cwd: dir,
    env: { ...env, GIT_AUTHOR_DATE: commitDate, GIT_COMMITTER_DATE: commitDate },
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  }).trim();
  git("init", "-b", "main");
  writeFileSync(join(dir, "f"), "0");
  git("add", ".");
  git("commit", "-m", "c0");
  return { dir, head: git("rev-parse", "HEAD") };
}

const run = (cwd, args = []) => spawnSync("bash", [join(cwd, "scripts/daemon-artifact-version.sh"), ...args], { cwd, env, encoding: "utf8" });

function frontendGates(version) {
  const code = `import * as g from ${JSON.stringify(gateModule)};
    const v = process.argv[1];
    console.log(JSON.stringify({ qc: g.checkQuickCreateCliVersion(v).state, fields: g.checkQuickCreateFieldsCliVersion(v).state, project: g.chatProjectContextSupported(v) }));`;
  const r = spawnSync(process.execPath, ["--experimental-strip-types", "--no-warnings", "--input-type=module", "-e", code, version], { encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  return JSON.parse(r.stdout);
}

test("artifact version stamps <upstream>-YYYYMMDD-hhmm from UPSTREAM_VERSION and commit time in ICT and passes frontend capability gates", (t) => {
  const { dir } = repo(t, { upstreamVersion: "0.6.3", commitDate: "2026-10-21T01:56:00Z" });
  const r = run(dir);
  assert.equal(r.status, 0, r.stderr);
  const version = r.stdout.trim();
  assert.equal(version, "0.6.3-20261021-0856");
  assert.deepEqual(frontendGates(version), { qc: "ok", fields: "ok", project: true });
});

test("artifact version rolls the ICT day and month over at 17:00 UTC", (t) => {
  const { dir } = repo(t, { upstreamVersion: "0.6.3", commitDate: "2026-10-31T17:30:00Z" });
  const r = run(dir);
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.stdout.trim(), "0.6.3-20261101-0030");
  assert.deepEqual(frontendGates("0.6.3-20261101-0030"), { qc: "ok", fields: "ok", project: true });
});

// Regression pin for the rejected `main-<sha>` stamp: the gates must not
// accept it (they were not weakened), so the script must never emit it.
test("the rejected main-<sha> form fails the unmodified gates", () => {
  assert.deepEqual(frontendGates("main-ed9f2e94d599"), { qc: "missing", fields: "missing", project: false });
});

test("the script fails closed when UPSTREAM_VERSION is missing or malformed", (t) => {
  const missing = repo(t, { upstreamVersion: null });
  assert.notEqual(run(missing.dir).status, 0);
  const prerelease = repo(t, { upstreamVersion: "0.7.0-rc1" });
  assert.notEqual(run(prerelease.dir).status, 0);
  const prefixed = repo(t, { upstreamVersion: "v0.6.3" });
  assert.notEqual(run(prefixed.dir).status, 0);
});

test("the CI daemon-artifact job uses the script and stamps the full commit separately", () => {
  const ci = readFileSync(join(root, ".github/workflows/ci.yml"), "utf8");
  const job = ci.slice(ci.indexOf("  daemon-artifact:"), ci.indexOf("  image-budget:"));
  assert.match(job, /scripts\/daemon-artifact-version\.sh/);
  assert.match(job, /-X main\.version=\$\{version\}/);
  assert.match(job, /-X main\.commit=\$\{GITHUB_SHA\}/);
  assert.doesNotMatch(job, /main\.version=main-/);
});
