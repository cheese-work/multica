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

function extractLines() {
  return ["route_write_scope_audit_test.go:192: x", "--- FAIL: TestRouterWriteRoutesAuditedAgainstTaskWriteScope (0.02s)"];
}

test("countAttempts counts failed-check removals since the head was committed", async () => {
  const { countAttempts } = await load();
  const ev = (reason, createdAt) => ({ reason, createdAt });
  assert.equal(
    countAttempts(
      [ev("failed_checks", "2026-11-03T01:00:00Z"), ev("manual", "2026-11-03T08:00:00Z"), ev("failed_checks", "2026-11-03T09:00:00Z")],
      "2026-11-03T07:00:00Z",
    ),
    1,
  );
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

const fakeApi = (reason) => {
  const writes = [];
  const run = { id: 7, html_url: "https://example/run/7", conclusion: "failure", head_branch: "gh-readonly-queue/main/pr-842-" + "c".repeat(40) };
  return {
    writes,
    graphql: async () => ({
      repository: {
        pullRequest: {
          headRefOid: "h".repeat(40),
          commits: { nodes: [{ commit: { committedDate: "2026-11-03T07:00:00Z" } }] },
          timelineItems: { nodes: [{ reason, createdAt: "2026-11-03T08:00:00Z", beforeCommit: { oid: "q".repeat(40) } }] },
        },
      },
    }),
    get: async (path) => {
      if (path.includes("/actions/runs?")) return { workflow_runs: [run] };
      if (path.includes("/jobs?")) return { jobs: [{ id: 9, name: "backend-tests", conclusion: "failure", html_url: "https://example/job/9" }, { id: 10, name: "lint", conclusion: "success", html_url: "u" }] };
      if (path.includes("/issues/842/comments")) return [];
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
