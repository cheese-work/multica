import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const load = () => import("./pr-main-sync.mjs");

const pr = (n, over = {}) => ({
  number: n,
  title: `feat: thing CHE-${n}`,
  body: "",
  base: { ref: "main" },
  head: { ref: `agent/x/che-${n}-abc`, sha: `sha${n}`, repo: { full_name: "o/r" } },
  ...over,
});

// Fake gh + multica; records every write so a test can assert "no push" / "one task".
const fakes = ({ prs, behind = {}, update = {}, active = {}, tasks = [] }) => {
  const log = { updates: [], created: [], runsChecked: [] };
  const gh = {
    openPrs: async () => prs,
    behindBy: async (sha) => behind[sha] ?? 1,
    updateBranch: async (number, expectedHeadSha) => {
      log.updates.push([number, expectedHeadSha]);
      return update[number] ?? "merged";
    },
  };
  const multica = {
    activeRuns: async (id) => {
      log.runsChecked.push(id);
      return active[id] ?? [];
    },
    findConflictTask: async (number) => tasks.find((t) => t.pr === number),
    createConflictTask: async (p, id) => log.created.push([p.number, id]),
  };
  return { gh, multica, log };
};

const run = async (f) => (await load()).syncPrs({ ...f, repo: "o/r", base: "main", print: () => {} });

test("a PR behind main gets one update-branch call pinned to its head sha", async () => {
  const f = fakes({ prs: [pr(1)] });
  await run(f);
  assert.deepEqual(f.log.updates, [[1, "sha1"]]);
  assert.deepEqual(f.log.created, []);
});

test("a PR already current with main is left alone", async () => {
  const f = fakes({ prs: [pr(1)], behind: { sha1: 0 } });
  await run(f);
  assert.deepEqual(f.log.updates, []);
});

test("a conflicting PR gets one author task and no further push", async () => {
  const f = fakes({ prs: [pr(2)], update: { 2: "conflict" } });
  await run(f);
  assert.deepEqual(f.log.created, [[2, "CHE-2"]]);
});

test("a conflict with an existing task for that PR does not create a second task", async () => {
  const f = fakes({ prs: [pr(2)], update: { 2: "conflict" }, tasks: [{ pr: 2 }] });
  await run(f);
  assert.deepEqual(f.log.created, []);
});

test("a PR whose linked issue has an active author run is skipped before any merge", async () => {
  const f = fakes({ prs: [pr(3)], active: { "CHE-3": [{ id: "run" }] } });
  await run(f);
  assert.deepEqual(f.log.runsChecked, ["CHE-3"]);
  assert.deepEqual(f.log.updates, []);
});

test("if the author pushed in between, the rejected update is skipped quietly", async () => {
  const f = fakes({ prs: [pr(4)], update: { 4: "moved" } });
  await run(f);
  assert.deepEqual(f.log.created, []);
});

test("fork PRs, other bases and PRs with no linked issue are skipped without a merge", async () => {
  const f = fakes({
    prs: [
      pr(5, { head: { ref: "agent/x/che-5", sha: "s5", repo: { full_name: "fork/r" } } }),
      pr(6, { base: { ref: "release" } }),
      pr(7, { title: "no issue", head: { ref: "topic", sha: "s7", repo: { full_name: "o/r" } } }),
    ],
  });
  await run(f);
  assert.deepEqual(f.log.updates, []);
});

test("one failing PR does not stop the rest; the run reports failure at the end", async () => {
  const f = fakes({ prs: [pr(8), pr(9)] });
  f.gh.updateBranch = async (n) => {
    if (n === 8) throw new Error("boom");
    f.log.updates.push([n]);
    return "merged";
  };
  await assert.rejects(run(f), /boom/);
  assert.deepEqual(f.log.updates, [[9]]);
});

test("authorOf is the agent in the branch name, not whoever the issue is assigned to", async () => {
  const { authorOf } = await load();
  assert.equal(authorOf({ head: { ref: "agent/x99-claude-sonnet-200k/che-1-abc" } }), "x99-claude-sonnet-200k");
  assert.equal(authorOf({ head: { ref: "topic/che-1" } }), undefined);
});

test("--pr limits the run to the listed PRs", async () => {
  const f = fakes({ prs: [pr(10), pr(11)] });
  await (await load()).syncPrs({ ...f, repo: "o/r", base: "main", only: [11], print: () => {} });
  assert.deepEqual(f.log.updates, [[11, "sha11"]]);
});

test("issueIdOf reads the identifier from branch, then title, then body", async () => {
  const { issueIdOf } = await load();
  assert.equal(issueIdOf({ head: { ref: "agent/a/che-3609-xyz" }, title: "", body: "" }), "CHE-3609");
  assert.equal(issueIdOf({ head: { ref: "topic" }, title: "fix (CHE-12)", body: "" }), "CHE-12");
  assert.equal(issueIdOf({ head: { ref: "topic" }, title: "t", body: "Closes CHE-77" }), "CHE-77");
  assert.equal(issueIdOf({ head: { ref: "topic" }, title: "t", body: "" }), undefined);
});

test("taskOwner: the branch's agent, else the issue assignee, else a loud failure", async () => {
  const { taskOwner } = await load();
  const agents = [{ name: "a1", id: "id-a1" }];
  assert.equal(taskOwner("a1", agents, { assignee_id: "id-x" }), "id-a1");
  assert.equal(taskOwner("ghost", agents, { assignee_id: "id-x" }), "id-x");
  assert.equal(taskOwner(undefined, agents, { assignee_id: "id-x" }), "id-x");
  assert.throws(() => taskOwner("ghost", agents, {}), /no owner/);
});

test("workflow: every live path (Main Build workflow_run or dispatch with dry_run=false) is behind PR_MAIN_SYNC_ENABLED", () => {
  const yml = readFileSync(new URL("../.github/workflows/pr-main-sync.yml", import.meta.url), "utf8");
  const jobIf = yml.match(/^ {4}if: (.*)$/m)[1];
  const dryRun = yml.match(/^ {10}DRY_RUN: (.*)$/m)[1];
  assert.match(jobIf, /PR_MAIN_SYNC_ENABLED == 'true'/);
  // Forced dry-run whenever the switch is off, regardless of the dispatch input.
  assert.match(dryRun, /PR_MAIN_SYNC_ENABLED != 'true'/);
});

test("a draft PR behind main is skipped; a ready PR is synced (CHE-4963)", async () => {
  const f = fakes({ prs: [pr(7, { draft: true }), pr(8, { draft: false })] });
  await run(f);
  assert.deepEqual(f.log.updates, [[8, "sha8"]]);
});

test("a dry run that finds a conflict still reports the task it would open (CHE-4963)", async () => {
  const f = fakes({ prs: [pr(9), pr(10)], update: { 9: "would-conflict", 10: "would-merge" } });
  await run(f);
  assert.deepEqual(f.log.created, [[9, "CHE-9"]]); // createConflictTask only prints in a dry run
});

test("mergeOutcome reads GitHub's mergeable flag and refuses an unknown one", async () => {
  const { mergeOutcome } = await load();
  assert.equal(mergeOutcome(true), "would-merge");
  assert.equal(mergeOutcome(false), "would-conflict");
  assert.throws(() => mergeOutcome(null), /not computed/);
});
