import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fetchChangelog, FETCH_DEADLINE_MS } from "./fork-changelog-fetch.mjs";

const sha = "a".repeat(40);
const pr = (number, extra = {}) => ({
  number, title: "feat: Notes", body: "Changelog: Notes now show shipped changes.",
  head: { ref: "feature/notes" }, merge_commit_sha: sha,
  merged_at: "2026-10-12T18:00:00Z", updated_at: "2026-10-13T00:00:00Z", ...extra,
});
const page = (prs, next = false) => ({
  ok: true, status: 200, json: async () => prs,
  headers: new Headers(next ? { link: '<https://api.github.com/next>; rel="next"' } : {}),
});

function fixture(context, extra = {}) {
  const directory = mkdtempSync(join(tmpdir(), "fork-fetch-"));
  context.after(() => rmSync(directory, { recursive: true, force: true }));
  const out = join(directory, "entries.json");
  const summaryPath = join(directory, "summary.md");
  writeFileSync(out, "stale entries");
  const warnings = [];
  return { directory, warnings, out, summaryPath, options: {
    token: "test-token", out, summaryPath, firstParent: [sha],
    warning: (message) => warnings.push(message), fetchImpl: async () => page([pr(1)]), ...extra,
  } };
}

test("fetches closed main PRs ordered by update, writes atomically and summarizes counts", async (context) => {
  const fixtureData = fixture(context, { fetchImpl: async (url, options) => {
    assert.equal(existsSync(fixtureData.out), false);
    const query = new URL(url).searchParams;
    for (const [key, value] of Object.entries({ state: "closed", base: "main", sort: "updated", direction: "desc", per_page: "100" })) {
      assert.equal(query.get(key), value);
    }
    assert.equal(options.headers.Authorization, "Bearer test-token");
    assert.ok(options.signal instanceof AbortSignal);
    return page([pr(1), pr(2, { body: "Changelog: none" }), pr(3, { merged_at: null })]);
  } });
  const result = await fetchChangelog(fixtureData.options);
  assert.equal(result.entries.length, 1);
  assert.deepEqual(JSON.parse(readFileSync(fixtureData.out, "utf8")), { entries: result.entries });
  assert.deepEqual(readdirSync(fixtureData.directory).sort(), ["entries.json", "summary.md"]);
  assert.match(readFileSync(fixtureData.summaryPath, "utf8"), /Shown: 1[\s\S]*Hidden: 1[\s\S]*none: 1/);
  assert.deepEqual(fixtureData.warnings, []);
  assert.equal(FETCH_DEADLINE_MS, 180_000);
});

test("paginates until updated_at is strictly before the ICT start", async (context) => {
  const pages = [page([pr(1)], true), page([
    pr(2, { updated_at: "2026-10-10T17:00:00Z" }),
    pr(3, { updated_at: "2026-10-10T16:59:59Z" }),
  ], true)];
  const urls = [];
  const data = fixture(context, { fetchImpl: async (url) => { urls.push(url); return pages.shift(); } });
  const result = await fetchChangelog(data.options);
  assert.equal(urls.length, 2);
  assert.equal(new URL(urls[1]).searchParams.get("page"), "2");
  assert.deepEqual(result.entries.map((entry) => entry.number), [2, 1]);
});

test("excludes unmerged PRs and PRs merged after the qualified SHA", async (context) => {
  const data = fixture(context, { fetchImpl: async () => page([
    pr(1), pr(2, { merge_commit_sha: "b".repeat(40) }), pr(3, { merged_at: null }),
  ]) });
  assert.deepEqual((await fetchChangelog(data.options)).entries.map((entry) => entry.number), [1]);
});

for (const [name, extra] of [
  ["no token", { token: "", fetchImpl: async () => { throw new Error("must not fetch"); } }],
  ["403", { fetchImpl: async () => ({ ok: false, status: 403 }) }],
  ["5xx", { fetchImpl: async () => ({ ok: false, status: 503 }) }],
  ["invalid response", { fetchImpl: async () => page({ error: "not a list" }) }],
  ["failure reading a page", { fetchImpl: async () => ({ ok: true, json: async () => { throw new Error("broken page"); } }) }],
  ["generator throws", { generator: () => { throw new Error("generator failed"); } }],
  ["timeout", { deadlineMs: 10, fetchImpl: async () => new Promise(() => {}) }],
]) {
  test(`${name} leaves no file and adds a warning`, async (context) => {
    const data = fixture(context, extra);
    assert.equal(await fetchChangelog(data.options), null);
    assert.equal(existsSync(data.out), false);
    assert.equal(data.warnings.length, 1);
    assert.match(readFileSync(data.summaryPath, "utf8"), /unavailable/);
    assert.deepEqual(readdirSync(data.directory), ["summary.md"]);
  });
}

test("failure after a complete first page never publishes partial entries", async (context) => {
  let requests = 0;
  const data = fixture(context, { fetchImpl: async () => {
    requests += 1;
    if (requests === 1) return page([pr(1)], true);
    throw new Error("network failed on page 2");
  } });
  assert.equal(await fetchChangelog(data.options), null);
  assert.equal(requests, 2);
  assert.equal(existsSync(data.out), false);
  assert.equal(data.warnings.length, 1);
});

test("the deadline covers all pages rather than restarting per request", async (context) => {
  let requests = 0;
  const data = fixture(context, { deadlineMs: 60, fetchImpl: async () => {
    requests += 1;
    if (requests === 1) {
      await new Promise((resolve) => setTimeout(resolve, 35));
      return page([pr(1)], true);
    }
    await new Promise((resolve) => setTimeout(resolve, 35));
    return page([pr(2)]);
  } });
  assert.equal(await fetchChangelog(data.options), null);
  assert.equal(existsSync(data.out), false);
  assert.match(data.warnings[0], /deadline/);
  await new Promise((resolve) => setTimeout(resolve, 40));
  assert.equal(existsSync(data.out), false);
});

test("a slow JSON response cannot write after the deadline", async (context) => {
  let resolvePage;
  const data = fixture(context, { deadlineMs: 10, fetchImpl: async () => ({
    ok: true, json: () => new Promise((resolve) => { resolvePage = resolve; }),
  }) });
  assert.equal(await fetchChangelog(data.options), null);
  resolvePage([pr(1)]);
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(existsSync(data.out), false);
});

test("write failure removes the temp file and reports unavailability", async (context) => {
  const data = fixture(context);
  writeFileSync(join(data.directory, "parent-is-a-file"), "not a directory");
  data.options.out = join(data.directory, "parent-is-a-file", "entries.json");
  assert.equal(await fetchChangelog(data.options), null);
  assert.equal(data.warnings.length, 1);
  assert.equal(existsSync(data.options.out), false);
});

test("per-PR content warnings are logged as notes, not annotations", async (context) => {
  const notes = [];
  const data = fixture(context, {
    note: (message) => notes.push(message),
    fetchImpl: async () => page([pr(1, { body: "Changelog: Notes now show shipped changes.\nChangelog: Second line." })]),
  });
  const result = await fetchChangelog(data.options);
  assert.equal(result.entries.length, 1);
  assert.deepEqual(data.warnings, []);
  assert.equal(notes.length, 1);
  assert.match(notes[0], /#1: More than one Changelog line/);
});

// CHE-4015
import { execFileSync } from "node:child_process";
import { deploymentsFromGraphql, fetchDeployments, versionLabel } from "./fork-changelog-fetch.mjs";

test("versionLabel is UPSTREAM_VERSION plus the release stamp of the commit time (ICT)", (context) => {
  const dir = mkdtempSync(join(tmpdir(), "fork-label-"));
  context.after(() => rmSync(dir, { recursive: true, force: true }));
  const git = (...args) => execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: { ...process.env, GIT_COMMITTER_DATE: "2026-11-05T07:26:00Z", GIT_AUTHOR_DATE: "2026-11-05T07:26:00Z" } }).trim();
  git("init", "-q");
  writeFileSync(join(dir, "UPSTREAM_VERSION"), "0.7.2\n");
  git("add", ".");
  git("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x");
  assert.equal(versionLabel(git("rev-parse", "HEAD"), { cwd: dir }), "0.7.2-20261105-1426");
});

test("a deployment-read failure writes entries without deploys, warns and still succeeds", async (context) => {
  const data = fixture(context, { deployments: async () => { throw new Error("deployments: HTTP 403"); } });
  const result = await fetchChangelog(data.options);
  assert.equal(result.entries.length, 1);
  assert.equal("deploys" in JSON.parse(readFileSync(data.out, "utf8")), false);
  assert.equal(data.warnings.length, 1);
  assert.match(data.warnings[0], /Deploy dividers unavailable.*HTTP 403/);
  assert.match(readFileSync(data.summaryPath, "utf8"), /Deploy dividers unavailable/);
});

test("deployments are bucketed into the written file", async (context) => {
  const data = fixture(context, {
    firstParent: [{ sha, message: "", time: "2026-10-13T00:00:00Z" }],
    versionOf: () => "0.7.2-20261013-0700",
    deployments: async () => [],
  });
  const result = await fetchChangelog(data.options);
  assert.deepEqual(JSON.parse(readFileSync(data.out, "utf8")).deploys.map((d) => [d.version, d.live]), [["0.7.2-20261013-0700", true]]);
  assert.equal(result.entries[0].deploy, "live");
});

// CHE-4015 review round: GitHub marks a superseded deployment INACTIVE, so only the status history tells a prior success.
test("deploymentsFromGraphql keeps a deployment that was ever SUCCESS, at its earliest success, even when now INACTIVE", () => {
  const nodes = [
    { commitOid: "a", statuses: { nodes: [{ state: "QUEUED", createdAt: "2026-11-05T01:00:00Z" }, { state: "SUCCESS", createdAt: "2026-11-05T01:05:00Z" }, { state: "INACTIVE", createdAt: "2026-11-05T05:00:00Z" }] } },
    { commitOid: "b", statuses: { nodes: [{ state: "QUEUED", createdAt: "2026-11-05T02:00:00Z" }, { state: "FAILURE", createdAt: "2026-11-05T02:05:00Z" }] } },
    { commitOid: "c", statuses: { nodes: [] } },
  ];
  assert.deepEqual(deploymentsFromGraphql({ data: { repository: { deployments: { nodes } } } }), [{ sha: "a", state: "SUCCESS", createdAt: "2026-11-05T01:05:00Z", environment: undefined, jobId: undefined, runId: undefined }]);
});

// CHE-5021: CHE-4495 moved the C00 deploy to c00-runtime, whose runs make two deployments (prepare-release-candidate and deploy).
const runtimeNode = (commitOid, state, runId, jobId, createdAt) => ({
  commitOid, environment: "c00-runtime",
  statuses: { nodes: [{ state, createdAt, logUrl: `https://github.com/o/r/actions/runs/${runId}/job/${jobId}` }] },
});
const fakeGithub = ({ nodes, jobsByRun, queries = [] }) => async (url, init) => {
  if (String(url).includes("/graphql")) {
    queries.push(JSON.parse(init.body).query);
    return { ok: true, json: async () => ({ data: { repository: { deployments: { pageInfo: { hasNextPage: false }, nodes } } } }) };
  }
  const run = /runs\/(\d+)\/jobs/.exec(url)[1];
  return { ok: true, json: async () => ({ jobs: jobsByRun[run] }) };
};

test("fetchDeployments reads c00-runtime and keeps c00-production for history", async () => {
  const queries = [];
  await fetchDeployments({ token: "t", repository: "o/r", fetchImpl: fakeGithub({ nodes: [], jobsByRun: {}, queries }) });
  assert.match(queries[0], /environments:\["c00-runtime","c00-production"\]/);
});

test("fetchDeployments counts the deploy job once per run, not prepare-release-candidate", async () => {
  const nodes = [
    runtimeNode("aaa", "SUCCESS", 1, 11, "2026-11-10T02:56:05Z"),
    runtimeNode("aaa", "SUCCESS", 1, 12, "2026-11-10T02:59:12Z"),
  ];
  const jobsByRun = { 1: [{ id: 11, name: "prepare-release-candidate" }, { id: 12, name: "deploy" }] };
  const result = await fetchDeployments({ token: "t", repository: "o/r", fetchImpl: fakeGithub({ nodes, jobsByRun }) });
  assert.deepEqual(result.map((d) => [d.sha, d.createdAt]), [["aaa", "2026-11-10T02:59:12Z"]]);
});

test("fetchDeployments ignores a run whose deploy job failed after prepare-release-candidate succeeded", async () => {
  const nodes = [
    runtimeNode("bbb", "SUCCESS", 2, 21, "2026-11-10T01:00:00Z"),
    runtimeNode("bbb", "FAILURE", 2, 22, "2026-11-10T01:05:00Z"),
  ];
  const jobsByRun = { 2: [{ id: 21, name: "prepare-release-candidate" }, { id: 22, name: "deploy" }] };
  assert.deepEqual(await fetchDeployments({ token: "t", repository: "o/r", fetchImpl: fakeGithub({ nodes, jobsByRun }) }), []);
});

test("fetchDeployments keeps c00-production deployments without a job lookup", async () => {
  const nodes = [{ commitOid: "ccc", environment: "c00-production", statuses: { nodes: [{ state: "SUCCESS", createdAt: "2026-11-07T00:00:00Z" }] } }];
  const result = await fetchDeployments({ token: "t", repository: "o/r", fetchImpl: fakeGithub({ nodes, jobsByRun: {} }) });
  assert.deepEqual(result.map((d) => d.sha), ["ccc"]);
});

test("a deployment read that outlives its budget falls back to day grouping and keeps the entries", async (context) => {
  const data = fixture(context, { deadlineMs: 400, deployments: () => new Promise(() => {}) });
  const result = await fetchChangelog(data.options);
  assert.equal(result.entries.length, 1);
  assert.equal("deploys" in JSON.parse(readFileSync(data.out, "utf8")), false);
  assert.match(data.warnings[0], /Deploy dividers unavailable/);
});

// CHE-5172: 100+ c00-runtime runs were looked up at once; GitHub answered 403 to some, one failure threw away every deploy, c00-production history included.
const manyRuntimeNodes = (count) => Array.from({ length: count }, (_, i) => runtimeNode(`sha${i}`, "SUCCESS", 1000 + i, 5000 + i, "2026-11-10T02:59:12Z"));
const jobsFor = (count) => Object.fromEntries(Array.from({ length: count }, (_, i) => [1000 + i, [{ id: 5000 + i, name: "deploy" }]]));
const productionNode = { commitOid: "prod", environment: "c00-production", statuses: { nodes: [{ state: "SUCCESS", createdAt: "2026-11-07T00:00:00Z" }] } };

test("fetchDeployments bounds concurrent job lookups so GitHub's concurrency limit is not hit", async () => {
  const github = fakeGithub({ nodes: [productionNode, ...manyRuntimeNodes(120)], jobsByRun: jobsFor(120) });
  let inFlight = 0;
  let peak = 0;
  const fetchImpl = async (url, init) => {
    if (String(url).includes("/graphql")) return github(url, init);
    inFlight += 1;
    peak = Math.max(peak, inFlight);
    try {
      await new Promise((resolve) => setTimeout(resolve, 1));
      if (inFlight > 10) return { ok: false, status: 403, json: async () => ({ message: "secondary rate limit" }) };
      return await github(url, init);
    } finally { inFlight -= 1; }
  };
  const report = [];
  const result = await fetchDeployments({ token: "t", repository: "o/r", fetchImpl, report: (m) => report.push(m), retryDelayMs: 0 });
  assert.ok(peak <= 10, `peak concurrency ${peak}`);
  assert.equal(result.length, 121);
  assert.deepEqual(report, []);
});

test("fetchDeployments keeps every readable run and c00-production when one jobs lookup keeps failing, and reports it", async () => {
  const github = fakeGithub({ nodes: [productionNode, ...manyRuntimeNodes(3)], jobsByRun: jobsFor(3) });
  const fetchImpl = async (url, init) => /runs\/1001\/jobs/.test(url)
    ? { ok: false, status: 403, json: async () => ({ message: "Resource not accessible by integration" }) }
    : github(url, init);
  const report = [];
  const result = await fetchDeployments({ token: "t", repository: "o/r", fetchImpl, report: (m) => report.push(m), retryDelayMs: 0 });
  assert.deepEqual(result.map((d) => d.sha).sort(), ["prod", "sha0", "sha2"]);
  assert.equal(report.length, 1);
  assert.match(report[0], /1 of 3 runs/);
  assert.match(report[0], /HTTP 403.*Resource not accessible by integration/);
});

test("fetchDeployments retries a transient jobs-lookup failure", async () => {
  const github = fakeGithub({ nodes: manyRuntimeNodes(1), jobsByRun: jobsFor(1) });
  let calls = 0;
  const fetchImpl = async (url, init) => (String(url).includes("/jobs") && (calls += 1) === 1
    ? { ok: false, status: 403, json: async () => ({ message: "secondary rate limit" }) }
    : github(url, init));
  const report = [];
  const result = await fetchDeployments({ token: "t", repository: "o/r", fetchImpl, report: (m) => report.push(m), retryDelayMs: 0 });
  assert.equal(result.length, 1);
  assert.deepEqual(report, []);
});

test("a partial deployment read keeps the dividers and prints a warning plus a step-summary line", async (context) => {
  const data = fixture(context, {
    firstParent: [{ sha, message: "", time: "2026-10-13T00:00:00Z" }],
    deployments: async (_signal, report) => { report("Deploy dividers incomplete: 1 of 3 runs unreadable"); return []; },
  });
  await fetchChangelog(data.options);
  assert.equal(Array.isArray(JSON.parse(readFileSync(data.out, "utf8")).deploys), true);
  assert.deepEqual(data.warnings, ["Deploy dividers incomplete: 1 of 3 runs unreadable"]);
  assert.match(readFileSync(data.summaryPath, "utf8"), /Deploy dividers incomplete/);
});
