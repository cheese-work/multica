import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
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

test("buildStatus never says 'unknown': falls back to the failed job, then to GitHub's reason", async () => {
  const { buildStatus } = await load();
  const base = { baseSha: "a".repeat(40), targetUrl: "u" };
  assert.equal(buildStatus({ ...base, lines: [], jobs: ["backend-tests"] }).description, "failed in merge queue on main@aaaaaaa: backend-tests");
  assert.equal(buildStatus({ ...base, lines: [] }).description, "failed in merge queue on main@aaaaaaa: failed_checks");
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

const fakeApi = (reason, { timeline, existingComment = false, runStates = [{ status: "completed", conclusion: "failure" }] } = {}) => {
  const writes = [];
  let polls = 0;
  const run = { id: 7, html_url: "https://example/run/7", head_branch: "gh-readonly-queue/main/pr-842-" + "c".repeat(40) };
  return {
    writes,
    graphql: async () => ({
      repository: { pullRequest: { headRefOid: "h".repeat(40), timelineItems: { nodes: timeline ?? [push, removal(reason)] } } },
    }),
    get: async (path) => {
      if (path.includes("/actions/runs?")) return { workflow_runs: [{ ...run, ...runStates[Math.min(polls++, runStates.length - 1)] }] };
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

test("report: queue run still in progress at dequeue time is waited for, not reported as unknown", async () => {
  const { report } = await load();
  const api = fakeApi("failed_checks", { runStates: [{ status: "in_progress", conclusion: null }, { status: "in_progress", conclusion: null }, { status: "completed", conclusion: "failure" }] });
  const sleeps = [];
  await report({ api, repo: "o/r", pr: 842, sleep: async (ms) => sleeps.push(ms) });
  assert.equal(sleeps.length, 2);
  assert.match(api.writes[1].body.description, /TestRouterWriteRoutesAuditedAgainstTaskWriteScope/);
  assert.doesNotMatch(api.writes[1].body.description, /unknown/);
});

test("report: a run that never finishes still reports its already-failed jobs, never 'unknown'", async () => {
  const { report } = await load();
  const api = fakeApi("failed_checks", { runStates: [{ status: "in_progress", conclusion: null }] });
  await report({ api, repo: "o/r", pr: 842, sleep: async () => {} });
  assert.doesNotMatch(api.writes[1].body.description, /unknown/);
  assert.match(api.writes[1].body.description, /TestRouter/);
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
