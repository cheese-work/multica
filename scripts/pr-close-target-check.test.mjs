import assert from "node:assert/strict";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const load = () => import("./pr-close-target-check.mjs");
const script = new URL("./pr-close-target-check.mjs", import.meta.url).pathname;
const fixtures = new URL("./fixtures/pr-close-target/", import.meta.url).pathname;
const workflow = new URL("../.github/workflows/pr-close-target-check.yml", import.meta.url).pathname;

const pullRequest = (overrides = {}) => ({
  title: "fix(x): change (CHE-1926) [squash]",
  body: "Closes CHE-1926",
  headRef: "fix/che-1926-change",
  ...overrides,
});

const strayKeys = async (input) =>
  (await load()).checkCloseTarget(input).strays.map(({ key, where }) => `${key}@${where}`);

test("the child named by the close directive passes", async () => {
  assert.deepEqual(await strayKeys(pullRequest()), []);
});

test("a branch that names the operational parent fails", async () => {
  assert.deepEqual(
    await strayKeys(pullRequest({ headRef: "fix/che-1911-schedule-cooldown-lookup" })),
    ["CHE-1911@branch"],
  );
});

test("a title that names the parent fails, and Refs does not shield it", async () => {
  const result = await strayKeys(pullRequest({
    title: "fix(x): change (CHE-1911) [squash]",
    body: "Closes CHE-1926\nRefs CHE-1911",
  }));
  assert.ok(result.includes("CHE-1911@title"), result.join());
});

test("a parent mentioned with Refs is allowed when it is not linked", async () => {
  assert.deepEqual(
    await strayKeys(pullRequest({ body: "Closes CHE-1926\n\nRefs CHE-1911" })),
    [],
  );
});

test("a single title and branch key is the target without a close directive", async () => {
  const result = (await load()).checkCloseTarget(pullRequest({ body: "Refs CHE-1926" }));
  assert.deepEqual(result.closeTargets, []);
  assert.deepEqual(result.strays, []);
});

test("a partial child PR can link the child and reference the parent", async () => {
  assert.deepEqual(
    await strayKeys(pullRequest({ body: "Refs CHE-1930" })),
    [],
  );
});

test("multiple title and branch keys need an explicit close target", async () => {
  assert.deepEqual(
    await strayKeys(pullRequest({
      body: "Refs CHE-1911",
      headRef: "fix/che-1911-schedule-cooldown-lookup",
    })),
    ["CHE-1926@title", "CHE-1911@branch"],
  );
});

test("a close directive in the title declares the target", async () => {
  const result = (await load()).checkCloseTarget(pullRequest({
    title: "fix(x): Closes CHE-1926 [squash]",
    body: "",
    headRef: "fix/che-1926-change",
  }));
  assert.deepEqual(result.closeTargets, ["CHE-1926"]);
  assert.deepEqual(result.strays, []);
});

test("a PR without issue keys or close intent is missing an issue link", async () => {
  const result = (await load()).checkCloseTarget(pullRequest({
    title: "chore: bump [squash]",
    body: "No issue link",
    headRef: "agent/x99/abc123",
  }));
  assert.equal(result.missingIssueLink, true);
});

test("a body-only close directive passes", async () => {
  assert.deepEqual(
    await strayKeys(pullRequest({
      title: "chore: bump [squash]",
      headRef: "agent/x99/abc123",
    })),
    [],
  );
});

test("code spans and fenced blocks do not declare close intent", async () => {
  const body = [
    "The old text said `Closes CHE-1911`.",
    "",
    "```text",
    "Closes CHE-1912",
    "```",
  ].join("\n");
  const input = pullRequest({
    title: "chore: update [squash]",
    body,
    headRef: "agent/x99/abc123",
  });
  const result = (await load()).checkCloseTarget(input);
  assert.deepEqual(result.closeTargets, []);
  assert.deepEqual(result.strays, []);
  assert.deepEqual(
    await strayKeys(pullRequest({ body })),
    [],
  );
});

test("CommonMark code delimiters follow the existing close-intent scanner", async () => {
  const body = [
    "The old line was ``Closes CHE-1911` with a backtick``.",
    "",
    "~~~ lang`example",
    "Closes CHE-1912",
    "~~~",
    "",
    "Stray `` marker then Closes CHE-1913",
    "",
    "Closes CHE-1926",
  ].join("\n");
  const result = (await load()).checkCloseTarget(pullRequest({ body }));
  assert.deepEqual(result.closeTargets, ["CHE-1913", "CHE-1926"]);
  assert.deepEqual(result.strays, [
    { key: "CHE-1913", where: "close target" },
  ]);
});

test("all existing close-keyword forms and separators are recognized", async () => {
  const directives = [
    "close CHE-1926",
    "closed: CHE-1926",
    "closes CHE-1926",
    "fix: CHE-1926",
    "fixes: CHE-1926",
    "fixed CHE-1926",
    "resolve: CHE-1926",
    "resolves CHE-1926",
    "resolved CHE-1926",
  ];
  for (const body of directives) {
    const result = (await load()).checkCloseTarget(pullRequest({ body }));
    assert.deepEqual(result.closeTargets, ["CHE-1926"], body);
    assert.deepEqual(result.strays, [], body);
  }
});

test("a close keyword followed by unrelated words does not override the linked target", async () => {
  const result = (await load()).checkCloseTarget(pullRequest({
    body: "Closes the task CHE-1911",
  }));
  assert.deepEqual(result.closeTargets, []);
  assert.deepEqual(result.strays, []);
});

test("close targets that disagree with title and branch links fail", async () => {
  const result = (await load()).checkCloseTarget(pullRequest({
    body: "Closes CHE-1926\nCloses CHE-1911",
  }));
  assert.deepEqual(result.strays, [
    { key: "CHE-1911", where: "close target" },
  ]);
});

test("issue-key matching follows the server grammar across prefixes", async () => {
  const result = (await load()).checkCloseTarget(pullRequest({
    title: "fix(x): change (mul-5) [squash]",
    body: "closes MUL-5",
    headRef: "fix/mul-5",
  }));
  assert.deepEqual(result.closeTargets, ["MUL-5"]);
  assert.deepEqual(result.strays, []);
});

test("the CLI rejects the operational-parent fixture and accepts the child fixture", () => {
  const run = (name) => spawnSync(
    "node",
    [script, "--event", `${fixtures}${name}.json`],
    { encoding: "utf8" },
  );
  const wrongTarget = run("operational-parent");
  assert.equal(wrongTarget.status, 1);
  assert.match(wrongTarget.stdout, /CHE-1911/);
  assert.match(wrongTarget.stdout, /::error::/);

  const correctTarget = run("child");
  assert.equal(correctTarget.status, 0, correctTarget.stdout + correctTarget.stderr);
  assert.match(correctTarget.stdout, /CHE-1926/);
});

test("the CLI skips upstream-sync heads", () => {
  const result = spawnSync(
    "node",
    [script, "--event", `${fixtures}upstream-sync.json`],
    { encoding: "utf8" },
  );
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /skipped.*upstream-sync/i);
});

test("the workflow runs the trusted workflow checker on a hosted runner without PR code", () => {
  const content = readFileSync(workflow, "utf8");
  assert.match(content, /pull_request_target:/);
  assert.match(content, /runs-on: ubuntu-24\.04/);
  assert.match(content, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.doesNotMatch(content, /github\.event\.pull_request\.base\.sha/);
  assert.match(content, /persist-credentials: false/);
  assert.match(content, /node scripts\/pr-close-target-check\.mjs --event "\$GITHUB_EVENT_PATH"/);
  assert.doesNotMatch(content, /pull_request:\s*$/m);
});

test("the workflow's checkout leaves no sparse state for a later full checkout on a reused workspace", () => {
  const content = readFileSync(workflow, "utf8");
  const sparse = content.match(/^\s*sparse-checkout: (\S+)$/m)?.[1];
  const dir = mkdtempSync(join(tmpdir(), "sparse-"));
  try {
    const git = (...args) => {
      const r = spawnSync("git", args, { cwd: dir, encoding: "utf8" });
      assert.equal(r.status, 0, `git ${args.join(" ")}: ${r.stderr}`);
      return r;
    };
    git("init", "-q");
    for (const f of ["scripts/pr-close-target-check.mjs", "server/main.go"]) {
      mkdirSync(join(dir, f, ".."), { recursive: true });
      writeFileSync(join(dir, f), "x\n");
    }
    git("add", "-A");
    git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x");
    // Replay actions/checkout@v6 non-cone sparse input: core.sparseCheckout=true in local config.
    if (sparse) {
      git("config", "core.sparseCheckout", "true");
      writeFileSync(join(dir, ".git/info/sparse-checkout"), `\n${sparse}\n`);
    }
    git("checkout", "-qf", "HEAD");
    // Replay a later full checkout on the same path: it disables sparse via worktree
    // config, then unsets extensions.worktreeConfig, exposing any leftover local true.
    git("sparse-checkout", "disable");
    git("config", "--unset", "extensions.worktreeConfig");
    git("checkout", "-qf", "HEAD");
    assert.ok(existsSync(join(dir, "server/main.go")), "server/ missing after reused full checkout");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

const healName = "Reset leaked sparse-checkout state";
const healRun = (content) =>
  content.match(new RegExp(`- name: ${healName}\\n\\s+run: \\|\\n((?:\\s{10,}.*\\n)+)`))?.[1];

const workflowsDir = new URL("../.github/workflows/", import.meta.url).pathname;

// Every job whose runs-on (or matrix runs-on) names cheese-x99 and that uses actions/checkout.
const x99CheckoutJobs = () =>
  readdirSync(workflowsDir)
    .filter((file) => file.endsWith(".yml"))
    .flatMap((file) => {
      const [, jobs = ""] = readFileSync(join(workflowsDir, file), "utf8").split(/^jobs:\n/m);
      return jobs
        .split(/^  (?=[\w-]+:\s*$)/m)
        .slice(1)
        .filter((body) => /^\s*runs-on:.*cheese-x99/m.test(body) && /uses: actions\/checkout@/.test(body))
        .map((body) => ({ id: `${file}:${body.split(":")[0]}`, body }));
    });

test("every X99 job that checks out heals leaked sparse state before its first checkout", () => {
  const jobs = x99CheckoutJobs();
  assert.ok(jobs.length >= 3, `discovery found only ${jobs.map((j) => j.id)}`);
  const missing = jobs
    .filter(({ body }) => {
      const heal = body.indexOf(`name: ${healName}`);
      return heal < 0 || heal > body.search(/uses: actions\/checkout@/);
    })
    .map((j) => j.id);
  assert.deepEqual(missing, [], "jobs without the heal step before checkout");
  const runs = new Set(jobs.map(({ body }) => healRun(body)));
  assert.equal(runs.size, 1, "heal step text must be identical in every job");
});

test("a workspace already contaminated by sparse state gets a full tree on the next checkout", () => {
  const run = healRun(readFileSync(workflow, "utf8"));
  const dir = mkdtempSync(join(tmpdir(), "sparse-"));
  try {
    const git = (...args) => {
      const r = spawnSync("git", args, { cwd: dir, encoding: "utf8" });
      assert.equal(r.status, 0, `git ${args.join(" ")}: ${r.stderr}`);
    };
    git("init", "-q");
    for (const f of ["scripts/pr-close-target-check.mjs", "server/main.go"]) {
      mkdirSync(join(dir, f, ".."), { recursive: true });
      writeFileSync(join(dir, f), "x\n");
    }
    git("add", "-A");
    git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "x");
    // State a prior non-cone sparse checkout left on the runner (observed on cheese-x99-repo-1/3/4/5/6/7).
    git("config", "core.sparseCheckout", "true");
    writeFileSync(join(dir, ".git/info/sparse-checkout"), "\nscripts/pr-close-target-check.mjs\n");
    git("read-tree", "-mu", "HEAD");
    assert.ok(!existsSync(join(dir, "server/main.go")), "fixture must start contaminated");
    if (run) {
      const r = spawnSync("bash", ["-e", "-c", run], {
        encoding: "utf8",
        env: { ...process.env, GITHUB_WORKSPACE: dir },
      });
      assert.equal(r.status, 0, r.stderr);
    }
    // actions/checkout@v6 without a sparse input: disable, unset extensions.worktreeConfig, checkout.
    git("sparse-checkout", "disable");
    git("config", "--unset", "extensions.worktreeConfig");
    git("checkout", "-qf", "HEAD");
    assert.ok(existsSync(join(dir, "server/main.go")), "server/ missing after checkout on contaminated workspace");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
