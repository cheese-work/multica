import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const load = () => import("./merge-queue-stale-cancel.mjs");
const REPO = "cheese-work/multica";
const NOW = Date.parse("2026-11-18T00:10:00Z");
const BRANCH = (n) => `gh-readonly-queue/main/pr-${n}-${"a".repeat(40)}`;
const run = (id, over = {}) => ({
  id,
  event: "merge_group",
  status: "in_progress",
  head_branch: BRANCH(id),
  head_sha: String(id).padStart(40, "0"),
  created_at: "2026-11-18T00:00:00Z", // 10 min before NOW
  html_url: `https://github.com/${REPO}/actions/runs/${id}`,
  ...over,
});

// Fake GitHub: `runs` is what the two status listings return, `live` the queue branches that still exist,
// `onMain` the SHAs whose compare against main is identical/behind. `fail` makes one call throw.
// `conflict` makes cancel answer HTTP 409 for those run ids; `conflictForce` does the same for force-cancel.
function fake({ runs = [], live = [], onMain = [], fail, conflict = [], conflictForce = [] } = {}) {
  const calls = [];
  const api = {
    get: async (path) => {
      calls.push(["GET", path]);
      if (fail && path.includes(fail)) throw new Error(`boom ${fail}`);
      if (path.includes("/actions/runs?")) {
        const status = new URL(`https://x/${path}`).searchParams.get("status");
        return { workflow_runs: runs.filter((r) => r.status === status) };
      }
      if (path.includes("/git/matching-refs/heads/gh-readonly-queue/")) return live.map((b) => ({ ref: `refs/heads/${b}` }));
      const m = path.match(/\/compare\/main\.\.\.([0-9a-f]{40})/);
      if (m) return { status: onMain.includes(m[1]) ? "behind" : "diverged" };
      throw new Error(`unexpected GET ${path}`);
    },
    post: async (path) => {
      calls.push(["POST", path]);
      if (fail && path.includes(fail)) throw new Error(`boom ${fail}`);
      const id = Number(path.match(/runs\/(\d+)\//)[1]);
      const list = path.endsWith("/force-cancel") ? conflictForce : conflict;
      if (list.includes(id)) throw Object.assign(new Error(`HTTP 409 ${path}`), { status: 409 });
    },
  };
  const posts = (suffix) => calls.filter(([m, p]) => m === "POST" && p.endsWith(suffix)).map(([, p]) => Number(p.match(/runs\/(\d+)\//)[1]));
  const forced = () => posts("/force-cancel");
  const cancelled = () => posts("/cancel");
  return { api, calls, cancelled, forced };
}
const sweep = async (f, extra = {}) => (await load()).sweep({ api: f.api, repo: REPO, now: () => NOW, print: () => {}, ...extra });

test("a run on a deleted queue branch is cancelled", async () => {
  const f = fake({ runs: [run(1)], live: [] });
  await sweep(f);
  assert.deepEqual(f.cancelled(), [1]);
});

test("a run on a live queue branch is not cancelled", async () => {
  const f = fake({ runs: [run(2)], live: [BRANCH(2)] });
  await sweep(f);
  assert.deepEqual(f.cancelled(), []);
});

test("a run created less than 60 s before the ref read is not cancelled", async () => {
  const young = run(3, { created_at: new Date(NOW - 59_000).toISOString() });
  const old = run(4, { created_at: new Date(NOW - 61_000).toISOString() });
  const f = fake({ runs: [young, old], live: [] });
  await sweep(f);
  assert.deepEqual(f.cancelled(), [4]);
});

test("a run whose head_sha is on main (a merged entry) is not cancelled", async () => {
  const merged = run(5);
  const f = fake({ runs: [merged, run(6)], live: [], onMain: [merged.head_sha] });
  await sweep(f);
  assert.deepEqual(f.cancelled(), [6]);
});

test("a head_sha identical to main also counts as merged", async () => {
  const { isOnMain } = await load();
  assert.equal(await isOnMain({ get: async () => ({ status: "identical" }) }, REPO, "a".repeat(40)), true);
  assert.equal(await isOnMain({ get: async () => ({ status: "ahead" }) }, REPO, "a".repeat(40)), false);
});

test("a pull_request or schedule run, or a non-queue branch, is never cancelled", async () => {
  const f = fake({
    runs: [
      run(7, { event: "pull_request", head_branch: "feature/x" }),
      run(8, { event: "schedule", head_branch: "main" }),
      run(9, { event: "pull_request" }), // queue-looking branch but wrong event
      run(10, { head_branch: "gh-readonly-queue/other/pr-10-x" }), // not main's queue
      run(11, { head_branch: "agent/x/gh-readonly-queue/main/pr-11" }), // prefix must anchor
    ],
    live: [],
  });
  await sweep(f);
  assert.deepEqual(f.cancelled(), []);
});

test("queued runs are swept as well as in_progress runs", async () => {
  const f = fake({ runs: [run(12), run(13, { status: "queued" })], live: [] });
  await sweep(f);
  assert.deepEqual(f.cancelled().sort(), [12, 13]);
});

for (const fail of ["/actions/runs?", "matching-refs", "/compare/", "/cancel"]) {
  test(`an API error on ${fail} cancels nothing more and rejects`, async () => {
    const f = fake({ runs: [run(14), run(15)], live: [], fail });
    await assert.rejects(sweep(f), /boom/);
    // A cancel that itself fails is the only POST allowed, and the sweep stops right there.
    assert.ok(f.cancelled().length <= (fail === "/cancel" ? 1 : 0));
  });
}

test("an error after the first cancel stops the sweep", async () => {
  const merged = run(17);
  const f = fake({ runs: [run(16), merged, run(18)], live: [] });
  const get = f.api.get;
  f.api.get = async (p) => {
    if (p.includes(`/compare/main...${merged.head_sha}`)) throw new Error("boom compare");
    return get(p);
  };
  await assert.rejects(sweep(f), /boom compare/);
  assert.deepEqual(f.cancelled(), [16]);
});

test("a 409 on cancel falls back to force-cancel once and the sweep goes on", async () => {
  const f = fake({ runs: [run(20), run(21)], live: [], conflict: [20] });
  await sweep(f);
  assert.deepEqual(f.cancelled(), [20, 21]);
  assert.deepEqual(f.forced(), [20]);
});

test("a 409 on force-cancel stops the sweep", async () => {
  const f = fake({ runs: [run(22), run(23)], live: [], conflict: [22], conflictForce: [22] });
  await assert.rejects(sweep(f), /409/);
  assert.deepEqual(f.cancelled(), [22]);
  assert.deepEqual(f.forced(), [22]);
});

test("a non-409 cancel error does not force-cancel", async () => {
  const f = fake({ runs: [run(24)], live: [], fail: "/cancel" });
  await assert.rejects(sweep(f), /boom/);
  assert.deepEqual(f.forced(), []);
});

test("gh api failures carry the HTTP status", async () => {
  const { httpStatus } = await load();
  assert.equal(httpStatus("gh: Cannot cancel a workflow run that has not been queued yet. (HTTP 409)"), 409);
  assert.equal(httpStatus("something else"), undefined);
});

test("dry run lists the runs it would cancel and cancels none", async () => {
  const lines = [];
  const f = fake({ runs: [run(19)], live: [] });
  await sweep(f, { dryRun: true, print: (l) => lines.push(l) });
  assert.deepEqual(f.cancelled(), []);
  assert.ok(lines.some((l) => l.includes("would cancel") && l.includes("/runs/19")));
});

test("the workflow keeps the trust boundary: default branch only, least permissions, one sweep at a time", () => {
  const text = readFileSync(new URL("../.github/workflows/merge-queue-stale-cancel.yml", import.meta.url), "utf8");
  assert.match(text, /pull_request_target:\n\s+types: \[dequeued\]/);
  assert.match(text, /workflow_dispatch:/);
  assert.match(text, /dry_run:[\s\S]*default: true/);
  assert.match(text, /ref: \$\{\{ github\.event\.repository\.default_branch \}\}/);
  assert.match(text, /persist-credentials: false/);
  assert.match(text, /permissions:\n\s+actions: write\n\s+contents: read\n/);
  assert.match(text, /concurrency:\n\s+group: [^\n]+\n\s+cancel-in-progress: false/);
  assert.doesNotMatch(text, /github\.event\.pull_request\.head/);
  assert.doesNotMatch(text, /\bdelete:/);
});

test("ci.yml runs this test in script-checks", () => {
  assert.match(readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8"), /scripts\/merge-queue-stale-cancel\.test\.mjs/);
});
