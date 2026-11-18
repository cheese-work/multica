// Cancels merge_group CI runs whose queue branch the merge queue has already deleted (replaced entries).
// Usage: node scripts/merge-queue-stale-cancel.mjs --repo owner/name [--dry-run]
// Each run is a full sweep. Fail closed: a wrong cancel can eject a PR from the queue, never merge one.
// Any API error stops the sweep (no further cancels) and exits non-zero; there is no retry loop.
import { spawnSync } from "node:child_process";
import { pathToFileURL } from "node:url";

const QUEUE_PREFIX = "gh-readonly-queue/main/";
const MIN_AGE_MS = 60_000; // a branch listing can lag the run start; younger runs are left alone
const PAGE = 100;
const MAX_PAGES = 10;

async function pages(api, path) {
  const sep = path.includes("?") ? "&" : "?";
  const out = [];
  for (let page = 1; page <= MAX_PAGES; page++) {
    const batch = await api.get(`${path}${sep}per_page=${PAGE}&page=${page}`);
    const items = Array.isArray(batch) ? batch : batch.workflow_runs;
    out.push(...items);
    if (items.length < PAGE) break;
  }
  return out;
}

// main-build.yml waits for the merged SHA's merge_group run to finish green, so a merged entry's run
// (branch deleted, non-required jobs still going) must never be cancelled.
export async function isOnMain(api, repo, sha) {
  const { status } = await api.get(`/repos/${repo}/compare/main...${sha}`);
  return status === "identical" || status === "behind";
}

export async function sweep({ api, repo, dryRun = false, now = Date.now, print = console.log }) {
  const runs = [];
  for (const status of ["in_progress", "queued"]) {
    runs.push(...(await pages(api, `/repos/${repo}/actions/runs?event=merge_group&status=${status}`)));
  }
  const readAt = now();
  const live = new Set((await pages(api, `/repos/${repo}/git/matching-refs/heads/gh-readonly-queue/`)).map((r) => r.ref));
  let cancelled = 0;
  for (const run of runs) {
    if (run.event !== "merge_group" || !run.head_branch?.startsWith(QUEUE_PREFIX)) continue;
    if (live.has(`refs/heads/${run.head_branch}`)) continue;
    if (readAt - Date.parse(run.created_at) <= MIN_AGE_MS) continue;
    if (await isOnMain(api, repo, run.head_sha)) continue;
    if (dryRun) {
      print(`would cancel ${run.html_url} (${run.head_branch}, ${run.status})`);
    } else {
      await api.post(`/repos/${repo}/actions/runs/${run.id}/cancel`);
      print(`cancelled ${run.html_url} (${run.head_branch}, ${run.status})`);
    }
    cancelled++;
  }
  print(`${dryRun ? "would cancel" : "cancelled"} ${cancelled} of ${runs.length} merge_group runs`);
  return cancelled;
}

const gh = (args) => {
  const r = spawnSync("gh", ["api", ...args], { encoding: "utf8", maxBuffer: 1 << 28 });
  if (r.status !== 0) throw new Error(`gh api ${args.join(" ")} failed: ${r.stderr}`);
  return r.stdout;
};
export const ghApi = {
  get: async (path) => JSON.parse(gh([path])),
  post: async (path) => gh(["-X", "POST", path]),
};

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const arg = (k) => (process.argv.includes(k) ? process.argv[process.argv.indexOf(k) + 1] : undefined);
  try {
    await sweep({ api: ghApi, repo: arg("--repo"), dryRun: process.argv.includes("--dry-run") });
  } catch (e) {
    console.error(`::error::merge-queue stale cancel stopped: ${e.message}`);
    process.exit(1);
  }
}
