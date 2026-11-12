// Keeps open PR branches current with `main`: when a PR is behind, merge `main` into it with
// GitHub's update-branch (a merge commit, no rebase, no force-push).
// Usage: node scripts/pr-main-sync.mjs --repo owner/name [--base main] [--dry-run] [--pr 12,34]  (--pr limits the run to those PRs)
// Safety: an issue with an active author run is skipped; every push is pinned to the PR head
// we read (`expected_head_sha`), so an author commit made in between is never overwritten and
// the call is rejected instead. A conflict pushes nothing and opens one task for the PR author.
// Needs a token that can push to PR branches (GH_TOKEN = the ci-app-token App token, not the Actions token,
// or the new head would not start CI).
import { spawnSync } from "node:child_process";
import { pathToFileURL } from "node:url";

// Branch first (agent branches carry it), then title, then body.
export const issueIdOf = (pr) => {
  const m = [pr.head.ref, pr.title, pr.body].map((s) => /\bche-(\d+)/i.exec(s ?? "")).find(Boolean);
  return m && `CHE-${m[1]}`;
};

// The PR author is the agent named in `agent/<name>/...`; the issue assignee may be a reviewer or a human.
export const authorOf = (pr) => /^agent\/([^/]+)\//.exec(pr.head.ref)?.[1];

// The branch's agent; else the issue assignee as the responsible owner; else fail so the job goes red
// instead of leaving a task nobody owns.
export const taskOwner = (author, agents, issue) => {
  const owner = agents.find((a) => a.name === author)?.id ?? issue.assignee_id;
  if (!owner) throw new Error(`no owner for the conflict task: author ${author ?? "(not an agent branch)"}, issue has no assignee`);
  return owner;
};

export async function syncPrs({ gh, multica, repo, base, only, print = console.log }) {
  const failures = [];
  for (const pr of (await gh.openPrs()).filter((p) => !only || only.includes(p.number))) {
    try {
      // Fork branches are not ours to push to; a PR with no linked issue cannot be checked for an active run.
      // A draft is synced on the first main-build run after it is marked ready (CHE-4963): keeps PR CI load down.
      if (pr.draft || pr.base.ref !== base || pr.head.repo?.full_name !== repo) continue;
      if (!(await gh.behindBy(pr.head.sha))) continue;
      const id = issueIdOf(pr);
      if (!id) {
        print(`#${pr.number}: skip, no linked issue`);
        continue;
      }
      if ((await multica.activeRuns(id)).length) {
        print(`#${pr.number}: skip, active run on ${id}`);
        continue;
      }
      const result = await gh.updateBranch(pr.number, pr.head.sha);
      print(`#${pr.number}: ${result}`);
      // A dry run reports the same split without writing: "would-conflict" opens (prints) the task it would create.
      if ((result === "conflict" || result === "would-conflict") && !(await multica.findConflictTask(pr.number))) await multica.createConflictTask(pr, id);
    } catch (e) {
      print(`#${pr.number}: error ${e.message}`);
      failures.push(e);
    }
  }
  if (failures.length) throw failures[0];
}

const run = (cmd, args, input) => {
  const r = spawnSync(cmd, args, { input, encoding: "utf8", maxBuffer: 1 << 28 });
  return { ok: r.status === 0, out: r.stdout, err: `${r.stderr}${r.stdout}` };
};
const must = (cmd, args, input) => {
  const r = run(cmd, args, input);
  if (!r.ok) throw new Error(`${cmd} ${args.slice(0, 3).join(" ")} failed: ${r.err}`);
  return r.out;
};

// GitHub computes `mergeable` (the PR merges into base without conflict, the same condition update-branch needs) lazily.
export const mergeOutcome = (mergeable) => {
  if (mergeable === null || mergeable === undefined) throw new Error("mergeable not computed yet");
  return mergeable ? "would-merge" : "would-conflict";
};

export const taskTitle = (n) => `main-sync conflict: PR #${n}`;

export const ghFor = (repo, base, dryRun) => ({
  openPrs: async () => must("gh", ["api", "--paginate", `repos/${repo}/pulls?state=open&per_page=100`, "--jq", ".[] | @json"]).split("\n").filter(Boolean).map((l) => JSON.parse(l)),
  behindBy: async (sha) => Number(must("gh", ["api", `repos/${repo}/compare/${base}...${sha}`, "--jq", ".behind_by"])),
  updateBranch: async (number, expectedHeadSha) => {
    if (dryRun) {
      // First read starts the computation and returns null: retry a few times, 2 s apart.
      for (let i = 0; ; i++) {
        const v = must("gh", ["api", `repos/${repo}/pulls/${number}`, "--jq", ".mergeable"]).trim();
        try {
          return mergeOutcome(v === "null" ? null : v === "true");
        } catch (e) {
          if (i >= 3) throw e;
          Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 2000);
        }
      }
    }
    const r = run("gh", ["api", "-X", "PUT", `repos/${repo}/pulls/${number}/update-branch`, "-f", `expected_head_sha=${expectedHeadSha}`]);
    if (r.ok) return "merged";
    if (/merge conflict/i.test(r.err)) return "conflict";
    if (/expected head sha|head ref/i.test(r.err)) return "moved";
    throw new Error(`update-branch PR #${number} failed: ${r.err}`);
  },
});

export const multicaCli = (repo, dryRun) => ({
  activeRuns: async (id) => JSON.parse(must("multica", ["issue", "runs", id, "--active", "--output", "json"])),
  // Closed tasks do not count: search hides done and cancelled issues by default.
  findConflictTask: async (n) =>
    JSON.parse(must("multica", ["issue", "search", taskTitle(n), "--output", "json"])).issues.find((i) => i.title === taskTitle(n)),
  createConflictTask: async (pr, id) => {
    if (dryRun) return console.log(`#${pr.number}: would open "${taskTitle(pr.number)}" under ${id}`);
    const issue = JSON.parse(must("multica", ["issue", "get", id, "--output", "json"]));
    const agents = JSON.parse(must("multica", ["agent", "list", "--output", "json"]));
    const owner = taskOwner(authorOf(pr), Array.isArray(agents) ? agents : agents.agents, issue);
    const body = `PR https://github.com/${repo}/pull/${pr.number} (${id}) conflicts with the base branch. Merge the base branch into \`${pr.head.ref}\`, resolve the conflict, push. Nothing was pushed by the sync job.`;
    must("multica", ["issue", "create", "--title", taskTitle(pr.number), "--parent", issue.id, "--assignee-id", owner,
      ...(issue.project_id ? ["--project", issue.project_id] : []), "--status", "todo", "--description-stdin"], body);
  },
});

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const arg = (k, d) => (process.argv.includes(k) ? process.argv[process.argv.indexOf(k) + 1] : d);
  const repo = arg("--repo");
  const base = arg("--base", "main");
  const dryRun = process.argv.includes("--dry-run");
  const only = arg("--pr")?.split(",").map(Number);
  await syncPrs({ gh: ghFor(repo, base, dryRun), multica: multicaCli(repo, dryRun), repo, base, only });
}
