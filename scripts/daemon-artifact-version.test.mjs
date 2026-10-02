// The version stamped into CI daemon artifacts must stay in the shape the
// runtime capability gates accept, with the exact commit stamped separately.
// Gates are exercised unmodified: the real packages/core TypeScript gate is
// imported; the Go gate (server/pkg/agent) has its own test over the same script.
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import test from "node:test";

const root = fileURLToPath(new URL("..", import.meta.url));
const script = join(root, "scripts/daemon-artifact-version.sh");
const gateModule = pathToFileURL(join(root, "packages/core/runtimes/cli-version.ts")).href;
const env = {
  ...process.env,
  GIT_CONFIG_NOSYSTEM: "1", GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_AUTHOR_NAME: "CI test", GIT_AUTHOR_EMAIL: "ci@example.invalid",
  GIT_COMMITTER_NAME: "CI test", GIT_COMMITTER_EMAIL: "ci@example.invalid",
};

function repo(t, { tag, commitsAfterTag = 0 }) {
  const dir = mkdtempSync(join(tmpdir(), "daemon-version-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const git = (...args) => execFileSync("git", args, { cwd: dir, env, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }).trim();
  git("init", "-b", "main");
  const commit = (n) => { writeFileSync(join(dir, "f"), String(n)); git("add", "."); git("commit", "-m", `c${n}`); };
  commit(0);
  if (tag) git("tag", tag);
  for (let i = 1; i <= commitsAfterTag; i++) commit(i);
  return { dir, head: git("rev-parse", "HEAD") };
}

const run = (cwd) => spawnSync("bash", [script], { cwd, env, encoding: "utf8" });

function frontendGates(version) {
  const code = `import * as g from ${JSON.stringify(gateModule)};
    const v = process.argv[1];
    console.log(JSON.stringify({ qc: g.checkQuickCreateCliVersion(v).state, fields: g.checkQuickCreateFieldsCliVersion(v).state, project: g.chatProjectContextSupported(v) }));`;
  const r = spawnSync(process.execPath, ["--experimental-strip-types", "--no-warnings", "--input-type=module", "-e", code, version], { encoding: "utf8" });
  assert.equal(r.status, 0, r.stderr);
  return JSON.parse(r.stdout);
}

test("artifact version past a tag is accepted by the frontend capability gates", (t) => {
  const { dir, head } = repo(t, { tag: "v0.6.0", commitsAfterTag: 3 });
  const r = run(dir);
  assert.equal(r.status, 0, r.stderr);
  const version = r.stdout.trim();
  assert.match(version, /^v0\.6\.0-3-g[0-9a-f]{12}$/);
  assert.ok(head.startsWith(version.split("-g")[1]), "describe suffix must abbreviate the built commit");
  assert.deepEqual(frontendGates(version), { qc: "ok", fields: "ok", project: true });
});

test("artifact version exactly on a tag is bare semver and accepted", (t) => {
  const { dir } = repo(t, { tag: "v0.6.0" });
  const r = run(dir);
  assert.equal(r.stdout.trim(), "v0.6.0");
  assert.deepEqual(frontendGates("v0.6.0"), { qc: "ok", fields: "ok", project: true });
});

// Regression pin for the rejected `main-<sha>` stamp: the gates must not
// accept it (they were not weakened), so the script must never emit it.
test("the rejected main-<sha> form fails the unmodified gates", () => {
  assert.deepEqual(frontendGates("main-ed9f2e94d599"), { qc: "missing", fields: "missing", project: false });
});

test("the script fails closed without a reachable release tag", (t) => {
  const none = repo(t, { commitsAfterTag: 2 });
  assert.notEqual(run(none.dir).status, 0);
  const prerelease = repo(t, { tag: "v0.7.0-rc1", commitsAfterTag: 1 });
  assert.notEqual(run(prerelease.dir).status, 0);
});

test("the CI daemon-artifact job uses the script and stamps the full commit separately", () => {
  const ci = readFileSync(join(root, ".github/workflows/ci.yml"), "utf8");
  const job = ci.slice(ci.indexOf("  daemon-artifact:"), ci.indexOf("  image-budget:"));
  assert.match(job, /scripts\/daemon-artifact-version\.sh/);
  assert.match(job, /-X main\.version=\$\{version\}/);
  assert.match(job, /-X main\.commit=\$\{GITHUB_SHA\}/);
  assert.match(job, /fetch-depth: 0/);
  assert.doesNotMatch(job, /main\.version=main-/);
});
