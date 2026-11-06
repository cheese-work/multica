import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

const load = () => import("./merge-queue-dequeue-report.mjs");
const log = readFileSync(
  new URL("./fixtures/merge-queue-dequeue/backend-tests-job-125788999656.txt", import.meta.url),
  "utf8",
);

test("extractFailures keeps the audit line, FAIL lines and the runner error, not passing-test noise", async () => {
  const { extractFailures } = await load();
  const lines = extractFailures(log);
  assert.ok(lines.some((l) => l.includes("--- FAIL: TestRouterWriteRoutesAuditedAgainstTaskWriteScope")));
  assert.ok(lines.some((l) => l.includes("route_write_scope_audit_test.go:192: write family api/agent-decisions")));
  assert.ok(lines.some((l) => l.startsWith("FAIL\tgithub.com/multica-ai/multica/server/cmd/server")));
  assert.ok(lines.some((l) => l.startsWith("##[error]Process completed")));
  assert.ok(!lines.some((l) => l.includes("simulated handler boom")), "mid-line 'panic:' is a passing test's WARN");
  assert.ok(lines.every((l) => !/^\d{4}-\d\d-\d\dT/.test(l)), "timestamps stripped");
});

test("extractFailures caps the output", async () => {
  const { extractFailures } = await load();
  const many = Array.from({ length: 50 }, (_, i) => `--- FAIL: TestN${i} (0.00s)`).join("\n");
  assert.equal(extractFailures(many, 20).length, 20);
});

test("buildStatus names the base and the first failing test, within 140 chars", async () => {
  const { buildStatus } = await load();
  const s = buildStatus({
    baseSha: "c6e7585ebfb1543dec74ba61d7ba7ef9f47e1add",
    lines: extractLines(),
    targetUrl: "https://example/run",
  });
  assert.equal(s.state, "failure");
  assert.equal(s.context, "merge-queue-result");
  assert.equal(s.description, "failed in merge queue on main@c6e7585: TestRouterWriteRoutesAuditedAgainstTaskWriteScope");
  assert.ok(s.description.length <= 140);
  assert.equal(
    buildStatus({ baseSha: "a".repeat(40), lines: ["--- FAIL: " + "T".repeat(300)], targetUrl: "u" }).description.length,
    140,
  );
});

function extractLines() {
  return ["route_write_scope_audit_test.go:192: x", "--- FAIL: TestRouterWriteRoutesAuditedAgainstTaskWriteScope (0.02s)"];
}

const removal = (reason, extra = {}) => ({ __typename: "RemovedFromMergeQueueEvent", reason, createdAt: "2026-11-03T08:00:00Z", beforeCommit: { oid: "q".repeat(40) }, ...extra });
const push = { __typename: "PullRequestCommit" };
const forcePush = { __typename: "HeadRefForcePushedEvent" };

test("countAttempts counts failed-check removals after the last push, by timeline order", async () => {
  const { countAttempts } = await load();
  assert.equal(countAttempts([push, removal("failed_checks"), removal("manual"), removal("failed_checks")]), 2);
  assert.equal(countAttempts([push, removal("failed_checks"), push, removal("failed_checks")]), 1);
  assert.equal(countAttempts([push, removal("failed_checks"), forcePush]), 0);
});

test("buildComment carries marker, SHAs, jobs, failing lines, run URL and attempt count", async () => {
  const { buildComment, MARKER } = await load();
  const body = buildComment({
    headSha: "98aa73dd13ba591fb27219eda3b94ecb4bbeedd3",
    baseSha: "c6e7585ebfb1543dec74ba61d7ba7ef9f47e1add",
    attempts: 4,
    failures: [{ job: "backend-tests", jobUrl: "https://example/job/1", lines: extractLines() }],
    runUrls: ["https://example/run/1"],
  });
  assert.ok(body.startsWith(MARKER));
  for (const s of ["98aa73dd", "c6e7585", "backend-tests", "https://example/job/1", "--- FAIL: TestRouter", "https://example/run/1", "4"]) {
    assert.ok(body.includes(s), s);
  }
});

const fakeApi = (reason, { timeline, existingComment = false } = {}) => {
  const writes = [];
  const run = { id: 7, html_url: "https://example/run/7", conclusion: "failure", head_branch: "gh-readonly-queue/main/pr-842-" + "c".repeat(40) };
  return {
    writes,
    graphql: async () => ({
      repository: { pullRequest: { headRefOid: "h".repeat(40), timelineItems: { nodes: timeline ?? [push, removal(reason)] } } },
    }),
    get: async (path) => {
      if (path.includes("/actions/runs?")) return { workflow_runs: [run] };
      if (path.includes("/jobs?")) return { jobs: [{ id: 9, name: "backend-tests", conclusion: "failure", html_url: "https://example/job/9" }, { id: 10, name: "lint", conclusion: "success", html_url: "u" }] };
      if (path.includes("/issues/842/comments")) return existingComment ? [{ id: 55, body: "<!-- merge-queue-dequeue-report -->\nold" }] : [];
      throw new Error("unexpected GET " + path);
    },
    text: async () => log,
    send: async (method, path, body) => writes.push({ method, path, body }),
  };
};

test("report: failed_checks writes one sticky comment and one head status", async () => {
  const { report } = await load();
  const api = fakeApi("failed_checks");
  await report({ api, repo: "o/r", pr: 842 });
  assert.deepEqual(api.writes.map((w) => `${w.method} ${w.path}`), ["POST /repos/o/r/issues/842/comments", `POST /repos/o/r/statuses/${"h".repeat(40)}`]);
  assert.equal(api.writes[1].body.state, "failure");
  assert.ok(api.writes[0].body.body.includes("TestRouterWriteRoutesAuditedAgainstTaskWriteScope"));
});

test("report: manual removal writes nothing", async () => {
  const { report } = await load();
  const api = fakeApi("manual");
  await report({ api, repo: "o/r", pr: 842 });
  assert.deepEqual(api.writes, []);
});

test("report: dry run prints payloads and writes nothing", async () => {
  const { report } = await load();
  const api = fakeApi("failed_checks");
  const out = [];
  await report({ api, repo: "o/r", pr: 842, dryRun: true, print: (s) => out.push(s) });
  assert.deepEqual(api.writes, []);
  assert.equal(out.length, 2);
});

test("report: a head pushed after the dequeue fired gets no stale comment and no status", async () => {
  const { report } = await load();
  const api = fakeApi("failed_checks");
  await report({ api, repo: "o/r", pr: 842, eventHeadSha: "old".padEnd(40, "0") });
  assert.deepEqual(api.writes, []);
});

test("report: the head of the dequeue event gets the status", async () => {
  const { report } = await load();
  const api = fakeApi("failed_checks");
  await report({ api, repo: "o/r", pr: 842, eventHeadSha: "h".repeat(40) });
  assert.equal(api.writes[1].path, `/repos/o/r/statuses/${"h".repeat(40)}`);
});

for (const [name, pushed] of [["a commit", push], ["a force push", forcePush]]) {
  for (const eventHeadSha of [undefined, "h".repeat(40)]) {
    test(`report: ${name} after the failed removal writes nothing (${eventHeadSha ? "event" : "dispatch"})`, async () => {
      const { report } = await load();
      const api = fakeApi("failed_checks", { timeline: [push, removal("failed_checks"), pushed] });
      await report({ api, repo: "o/r", pr: 842, eventHeadSha });
      assert.deepEqual(api.writes, []);
    });
  }
}

test("report: only the removal after the last push is reported, with its own attempt count", async () => {
  const { report } = await load();
  const api = fakeApi("failed_checks", { timeline: [push, removal("failed_checks"), push, removal("failed_checks")] });
  await report({ api, repo: "o/r", pr: 842, eventHeadSha: "h".repeat(40) });
  assert.match(api.writes[0].body.body, /Failed attempts for this head: 1/);
});

test("report: a manual removal after a failed one on the same head writes nothing", async () => {
  const { report } = await load();
  const api = fakeApi("failed_checks", { timeline: [push, removal("failed_checks"), removal("manual")] });
  await report({ api, repo: "o/r", pr: 842 });
  assert.deepEqual(api.writes, []);
});

test("report: replaying an older removal is dry-run only", async () => {
  const { report } = await load();
  const api = fakeApi("failed_checks", { timeline: [push, removal("failed_checks"), push, removal("failed_checks")] });
  await assert.rejects(report({ api, repo: "o/r", pr: 842, eventIndex: -2 }), /dry/);
  const out = [];
  await report({ api, repo: "o/r", pr: 842, eventIndex: -2, dryRun: true, print: (s) => out.push(s) });
  assert.equal(out.length, 2);
});

test("report: overlapping dequeue runs for one head update the same sticky comment", async () => {
  const { report } = await load();
  const api = fakeApi("failed_checks", { timeline: [push, removal("failed_checks"), removal("failed_checks")], existingComment: true });
  await Promise.all([1, 2].map(() => report({ api, repo: "o/r", pr: 842, eventHeadSha: "h".repeat(40) })));
  assert.deepEqual(api.writes.map((w) => w.method).sort(), ["PATCH", "PATCH", "POST", "POST"]);
  assert.ok(api.writes.every((w) => w.method !== "POST" || w.path.includes("/statuses/")), "never a second comment");
});

// Trust boundary: the workflow holds a write token, so it must run default-branch code, never PR code.
const workflow = readFileSync(new URL("../.github/workflows/merge-queue-dequeue-report.yml", import.meta.url), "utf8");

test("workflow runs on pull_request_target and checks out the default branch only", () => {
  assert.match(workflow, /^\s{2}pull_request_target:/m);
  assert.doesNotMatch(workflow, /^\s{2}pull_request:/m, "pull_request runs the PR's own workflow/script and has a read-only fork token");
  assert.match(workflow, /ref: \$\{\{ github\.event\.repository\.default_branch \}\}/);
  assert.match(workflow, /persist-credentials: false/);
  assert.doesNotMatch(workflow, /pull_request\.head\.(ref|repo)|refs\/pull|merge_commit_sha/);
});

// End-to-end through the real CLI with a fake `gh` first on PATH (CHE-4229).
// Mode "rl2": the first two non-rate_limit calls get the rate-limit 403; "rl": all do; "500": all get HTTP 500.
const RATE_LIMIT_ERR = "gh: API rate limit exceeded for installation. (HTTP 403)";
const runCli = (mode) => {
  const dir = mkdtempSync(join(tmpdir(), "fake-gh-"));
  const calls = join(dir, "calls");
  writeFileSync(join(dir, "gh"), `#!/usr/bin/env node
const fs = require("node:fs");
const args = process.argv.slice(2);
const calls = ${JSON.stringify(calls)};
const prior = fs.existsSync(calls) ? fs.readFileSync(calls, "utf8").split("\\n").filter(Boolean) : [];
fs.appendFileSync(calls, args.join(" ") + "\\n");
if (args[1] === "rate_limit") { console.log("0"); process.exit(0); }
const n = prior.filter((l) => !l.includes("rate_limit")).length;
const mode = ${JSON.stringify(mode)};
if (mode === "500") { console.error("gh: Server Error (HTTP 500)"); process.exit(1); }
if (mode === "rl" || (mode === "rl2" && n < 2)) { console.error(${JSON.stringify(RATE_LIMIT_ERR)}); process.exit(1); }
const path = args[1];
if (path === "graphql") console.log(JSON.stringify({ data: { repository: { pullRequest: { headRefOid: "h".repeat(40), timelineItems: { nodes: [{ __typename: "RemovedFromMergeQueueEvent", reason: "failed_checks", createdAt: "2026-11-06T00:00:00Z", beforeCommit: { oid: "q".repeat(40) } }] } } } } }));
else if (path.includes("/actions/runs?")) console.log(JSON.stringify({ workflow_runs: [] }));
else if (path.includes("/comments")) console.log("[]");
else console.log("{}");
`);
  chmodSync(join(dir, "gh"), 0o755);
  const r = spawnSync(process.execPath, [new URL("./merge-queue-dequeue-report.mjs", import.meta.url).pathname, "--repo", "o/r", "--pr", "918"], {
    encoding: "utf8",
    env: { ...process.env, PATH: `${dir}:${process.env.PATH}` },
  });
  const posts = readFileSync(calls, "utf8").split("\n").filter((l) => l.includes("-X POST") && l.includes("/comments"));
  return { ...r, posts };
};

test("cli: rate-limit 403 twice, then success, posts the report once and exits 0", () => {
  const r = runCli("rl2");
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.posts.length, 1);
});

test("cli: a rate limit that never clears exits 0 with a warning naming the PR and the call", () => {
  const r = runCli("rl");
  assert.equal(r.status, 0, r.stderr);
  assert.match(r.stdout, /^::warning::.*PR #918.*graphql/m);
  assert.equal(r.posts.length, 0);
});

test("cli: HTTP 500 still fails the job", () => {
  const r = runCli("500");
  assert.notEqual(r.status, 0);
  assert.doesNotMatch(r.stdout, /::warning::/);
});

test("waitMs: waits until the reset, capped at 10 min, 60 s when unreadable", async () => {
  const { waitMs } = await load();
  const now = 1_000_000_000_000;
  const at = (s) => String(now / 1000 + s);
  assert.equal(waitMs(at(-30), now), 0);
  assert.equal(waitMs(at(90), now), 90_000);
  assert.equal(waitMs(at(3600), now), 600_000);
  for (const bad of ["", "\n", "null", "abc"]) assert.equal(waitMs(bad, now), 60_000, JSON.stringify(bad));
});

test("callName names the endpoint of a read, a write and a GraphQL call", async () => {
  const { callName } = await load();
  assert.equal(callName(["api", "graphql", "-f", "query=q"]), "graphql");
  assert.equal(callName(["api", "/repos/o/r/actions/runs?event=merge_group"]), "/repos/o/r/actions/runs?event=merge_group");
  assert.equal(callName(["api", "-X", "POST", "/repos/o/r/issues/1/comments", "--input", "-"]), "/repos/o/r/issues/1/comments");
});

test("workflow timeout outlasts every rate-limit wait plus 5 min of work, so the warning can print", async () => {
  const { ATTEMPTS, MAX_WAIT_MS } = await load();
  const yml = readFileSync(new URL("../.github/workflows/merge-queue-dequeue-report.yml", import.meta.url), "utf8");
  const minutes = Number(yml.match(/timeout-minutes:\s*(\d+)/)?.[1]);
  assert.ok(minutes >= ((ATTEMPTS - 1) * MAX_WAIT_MS) / 60_000 + 5, `timeout-minutes ${minutes}`);
});
