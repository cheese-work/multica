// Reports why a PR left the merge queue for failed checks: one sticky PR comment plus a
// `merge-queue-result` failure status on the PR head (so the head rollup goes red until a push).
// Usage: node scripts/merge-queue-dequeue-report.mjs --repo owner/name --pr 842 [--dry-run] [--event -2]
// --event picks a removal from the end of the PR timeline (default -1 = latest) to replay older ones.
import { spawnSync } from "node:child_process";
import { pathToFileURL } from "node:url";

export const MARKER = "<!-- merge-queue-dequeue-report -->";
export const CONTEXT = "merge-queue-result";
const FAILED_CHECKS = "failed_checks";
const MAX_LINES = 20;
const MAX_JOBS = 5;
const STATUS_MAX = 140;
// ponytail: line-anchored so a passing test's "handler panic:" WARN is not reported.
const FAILURE_LINE = /^(\s*--- FAIL|FAIL\t|panic:|##\[error\])/;
const TEST_FILE_LINE = /^\s*\S+_test\.go:\d+:/;

const clean = (line) => line.replace(/\u001b\[[0-9;]*m/g, "").replace(/^\d{4}-\d\d-\d\dT[\d:.]+Z /, "").trimEnd();

export function extractFailures(log, cap = MAX_LINES) {
  const lines = log.split("\n").map(clean);
  const out = [];
  lines.forEach((line, i) => {
    if (!FAILURE_LINE.test(line)) return;
    // The assertion text sits on the `_test.go:N:` line just above `--- FAIL`.
    if (/^\s*--- FAIL/.test(line) && TEST_FILE_LINE.test(lines[i - 1] ?? "")) out.push(lines[i - 1].trim());
    out.push(line.trim());
  });
  return out.slice(0, cap);
}

const firstFailingTest = (lines) =>
  lines.find((l) => l.startsWith("--- FAIL"))?.replace(/^--- FAIL:\s*(\S+).*/, "$1") ?? lines[0] ?? "unknown";

export function buildStatus({ baseSha, lines, targetUrl }) {
  const description = `failed in merge queue on main@${baseSha.slice(0, 7)}: ${firstFailingTest(lines)}`;
  return { state: "failure", context: CONTEXT, description: description.slice(0, STATUS_MAX), target_url: targetUrl };
}

// ponytail: events carry no head SHA; "since the head was committed" is the attempt window.
export const countAttempts = (events, headCommittedAt) =>
  events.filter((e) => e.reason === FAILED_CHECKS && e.createdAt >= headCommittedAt).length;

export function buildComment({ headSha, baseSha, attempts, failures, runUrls }) {
  const jobs = failures
    .map((f) => `### [${f.job}](${f.jobUrl})\n\`\`\`\n${f.lines.join("\n") || "(no failing lines found in the log)"}\n\`\`\``)
    .join("\n\n");
  return [
    MARKER,
    "## Removed from the merge queue: failed checks",
    "",
    `- PR head: \`${headSha.slice(0, 8)}\``,
    `- main base of the queue commit: \`${baseSha.slice(0, 7)}\``,
    `- Failed attempts for this head: ${attempts}`,
    `- Runs: ${runUrls.join(", ")}`,
    "",
    jobs,
    "",
    "The PR head checks stay green because only the queue build (PR + current `main`) fails. Push a fix to this branch; the `merge-queue-result` status clears with the new head. Re-enqueueing the same head fails the same way.",
  ].join("\n");
}

const QUERY = `query($owner:String!,$name:String!,$pr:Int!){repository(owner:$owner,name:$name){pullRequest(number:$pr){
  headRefOid
  commits(last:1){nodes{commit{committedDate}}}
  timelineItems(last:30,itemTypes:[REMOVED_FROM_MERGE_QUEUE_EVENT]){nodes{... on RemovedFromMergeQueueEvent{reason createdAt beforeCommit{oid}}}}
}}}`;

export async function report({ api, repo, pr, dryRun = false, eventIndex = -1, print = console.log }) {
  const [owner, name] = repo.split("/");
  const { pullRequest } = (await api.graphql(QUERY, { owner, name, pr })).repository;
  const events = pullRequest.timelineItems.nodes;
  const last = events.at(eventIndex);
  if (last?.reason !== FAILED_CHECKS) return;

  const headSha = pullRequest.headRefOid;
  const queueSha = last.beforeCommit.oid;
  const runs = (await api.get(`/repos/${repo}/actions/runs?event=merge_group&head_sha=${queueSha}&per_page=20`)).workflow_runs
    .filter((r) => r.conclusion === "failure");
  const failures = [];
  for (const run of runs) {
    const { jobs } = await api.get(`/repos/${repo}/actions/runs/${run.id}/jobs?filter=latest&per_page=100`);
    for (const job of jobs.filter((j) => j.conclusion === "failure").slice(0, MAX_JOBS)) {
      const lines = extractFailures(await api.text(`/repos/${repo}/actions/jobs/${job.id}/logs`));
      failures.push({ job: job.name, jobUrl: job.html_url, lines });
    }
  }
  // Queue branch name carries the main SHA the entry was built on.
  const baseSha = runs[0]?.head_branch.match(/pr-\d+-([0-9a-f]{40})$/)?.[1] ?? queueSha;
  const lines = failures.flatMap((f) => f.lines);
  const runUrls = runs.map((r) => r.html_url);
  const comment = buildComment({
    headSha, baseSha, runUrls, failures,
    attempts: countAttempts(events, pullRequest.commits.nodes[0].commit.committedDate),
  });
  const status = buildStatus({ baseSha, lines, targetUrl: runUrls[0] ?? "" });

  if (dryRun) {
    print(`--- sticky comment (PR #${pr}) ---\n${comment}`);
    print(`--- status on ${headSha} ---\n${JSON.stringify(status, null, 2)}`);
    return;
  }
  const existing = (await api.get(`/repos/${repo}/issues/${pr}/comments?per_page=100`)).find((c) => c.body?.startsWith(MARKER));
  if (existing) await api.send("PATCH", `/repos/${repo}/issues/comments/${existing.id}`, { body: comment });
  else await api.send("POST", `/repos/${repo}/issues/${pr}/comments`, { body: comment });
  await api.send("POST", `/repos/${repo}/statuses/${headSha}`, status);
}

const gh = (args, input) => {
  const r = spawnSync("gh", args, { input, encoding: "utf8", maxBuffer: 1 << 28 });
  if (r.status !== 0) throw new Error(`gh ${args.join(" ")} failed: ${r.stderr}`);
  return r.stdout;
};

export const ghApi = {
  graphql: async (query, vars) =>
    JSON.parse(gh(["api", "graphql", "-f", `query=${query}`, ...Object.entries(vars).flatMap(([k, v]) => [typeof v === "number" ? "-F" : "-f", `${k}=${v}`])])).data,
  // ponytail: first page only (100); a PR with >100 comments could miss its sticky comment.
  get: async (path) => JSON.parse(gh(["api", path])),
  text: async (path) => gh(["api", path]),
  send: async (method, path, body) => gh(["api", "-X", method, path, "--input", "-"], JSON.stringify(body)),
};

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const arg = (k) => (process.argv.includes(k) ? process.argv[process.argv.indexOf(k) + 1] : undefined);
  await report({ api: ghApi, repo: arg("--repo"), pr: Number(arg("--pr")), dryRun: process.argv.includes("--dry-run"), eventIndex: Number(arg("--event") ?? -1) });
}
