// Reports why a PR left the merge queue for failed checks: one sticky PR comment plus a
// `merge-queue-result` failure status on the PR head (so the head rollup goes red until a push).
// Usage: node scripts/merge-queue-dequeue-report.mjs --repo owner/name --pr 842 [--dry-run] [--event -2] [--head-sha <sha>]
// --head-sha is the PR head from the dequeue event payload (a snapshot); if the head moved, nothing is written.
// --event replays an older removal and is dry-run only.
import { spawnSync } from "node:child_process";
import { pathToFileURL } from "node:url";

export const MARKER = "<!-- merge-queue-dequeue-report -->";
export const CONTEXT = "merge-queue-result";
const FAILED_CHECKS = "failed_checks";
const MAX_LINES = 20;
const MAX_JOBS = 5;
const STATUS_MAX = 140;
// GitHub dequeues on the first failed check; the merge_group run is usually still going. Wait for it to finish.
const POLL_MS = 30_000;
const MAX_POLLS = 20;
const WAIT_MIN = (POLL_MS * MAX_POLLS) / 60_000;
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

// Never "unknown": fall back to the failed job, then to the reason GitHub gave for the removal.
const firstFailingTest = (lines, jobs) =>
  lines.find((l) => l.startsWith("--- FAIL"))?.replace(/^--- FAIL:\s*(\S+).*/, "$1") ?? lines[0] ?? jobs[0] ?? FAILED_CHECKS;

// `note` (timeout wording) leads, so the 140-char cut never drops it.
export function buildStatus({ baseSha, lines, jobs = [], targetUrl, note }) {
  const base = `failed in merge queue on main@${baseSha.slice(0, 7)}: ${firstFailingTest(lines, jobs)}`;
  const description = note ? `${note}; ${base}` : base;
  return { state: "failure", context: CONTEXT, description: description.slice(0, STATUS_MAX), target_url: targetUrl };
}

// A removal carries no head SHA, so bind it to its head by timeline order (commit dates can be backdated):
// anything after the last push belongs to the current head, anything before it to an old head.
const PUSHES = new Set(["PullRequestCommit", "HeadRefForcePushedEvent"]);
const isRemoval = (n) => n.__typename === "RemovedFromMergeQueueEvent";
const sinceLastPush = (nodes) => nodes.slice(nodes.findLastIndex((n) => PUSHES.has(n.__typename)) + 1);
export const countAttempts = (nodes) => sinceLastPush(nodes).filter((n) => isRemoval(n) && n.reason === FAILED_CHECKS).length;

export function buildComment({ headSha, baseSha, attempts, failures, runUrls, note }) {
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
    ...(note ? ["", `**${note}**`] : []),
    "",
    jobs,
    "",
    "The PR head checks stay green because only the queue build (PR + current `main`) fails. Push a fix to this branch; the `merge-queue-result` status clears with the new head. Re-enqueueing the same head fails the same way.",
  ].join("\n");
}

const QUERY = `query($owner:String!,$name:String!,$pr:Int!){repository(owner:$owner,name:$name){pullRequest(number:$pr){
  headRefOid
  timelineItems(last:100,itemTypes:[REMOVED_FROM_MERGE_QUEUE_EVENT,PULL_REQUEST_COMMIT,HEAD_REF_FORCE_PUSHED_EVENT]){nodes{__typename ... on RemovedFromMergeQueueEvent{reason createdAt beforeCommit{oid}}}}
}}}`;

export async function report({ api, repo, pr, dryRun = false, eventIndex = -1, eventHeadSha, print = console.log, sleep = (ms) => new Promise((r) => setTimeout(r, ms)) }) {
  const [owner, name] = repo.split("/");
  // Fail closed: the removal must be the newest timeline item (no push or later removal followed it) and the head must
  // still be the one from the event. Re-run after every wait: the PR can move while the queue run finishes.
  const current = async () => {
    const { pullRequest } = (await api.graphql(QUERY, { owner, name, pr })).repository;
    let timeline = pullRequest.timelineItems.nodes;
    if (eventIndex !== -1) {
      if (!dryRun) throw new Error("replaying an older removal needs --dry-run");
      timeline = timeline.slice(0, timeline.indexOf(timeline.filter(isRemoval).at(eventIndex)) + 1);
    }
    const last = timeline.at(-1);
    if (!last || !isRemoval(last) || last.reason !== FAILED_CHECKS) return null;
    if (eventHeadSha && eventHeadSha !== pullRequest.headRefOid) return null;
    return { timeline, headSha: pullRequest.headRefOid, queueSha: last.beforeCommit.oid };
  };
  let state = await current();
  if (!state) return;
  const { queueSha, headSha: startHead } = state;
  const fetchRuns = async () => (await api.get(`/repos/${repo}/actions/runs?event=merge_group&head_sha=${queueSha}&per_page=20`)).workflow_runs;
  // GitHub can dequeue before the merge_group run exists: an empty list is "not started yet", not "no failure".
  const settled = (runs) => runs.length > 0 && runs.every((r) => r.status === "completed");
  let allRuns = await fetchRuns();
  for (let i = 0; i < MAX_POLLS && !settled(allRuns); i++) {
    await sleep(POLL_MS);
    state = await current();
    if (!state || state.queueSha !== queueSha || state.headSha !== startHead) return;
    allRuns = await fetchRuns();
  }
  const timedOut = !settled(allRuns);
  const note = !timedOut ? undefined : allRuns.length ? `merge_group run still running after ${WAIT_MIN} min` : `no merge_group run found after ${WAIT_MIN} min`;
  const { timeline, headSha } = state;
  // A run that is still going after the wait keeps its already-failed jobs.
  const runs = allRuns.filter((r) => r.conclusion === "failure" || r.status !== "completed");
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
  // No run to link: point at the queue commit so the reader can look.
  const runUrls = runs.length ? runs.map((r) => r.html_url) : timedOut ? [`https://github.com/${repo}/commit/${queueSha}`] : [];
  const comment = buildComment({
    headSha, baseSha, runUrls, failures, note,
    attempts: countAttempts(timeline),
  });
  const status = buildStatus({ baseSha, lines, jobs: failures.map((f) => f.job), targetUrl: runUrls[0] ?? "", note });

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

export const ATTEMPTS = 3;
export const MAX_WAIT_MS = 10 * 60 * 1000;
const FALLBACK_WAIT_MS = 60 * 1000;
const sleep = (ms) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
const isRateLimit = (stderr) => /rate limit/i.test(stderr) && /HTTP (403|429)/.test(stderr);

export class RateLimitError extends Error {}

// Wait until the limit resets (cap 10 min); a missing or unreadable reset time waits a fixed minute.
export const waitMs = (resetEpoch, now = Date.now()) => {
  const reset = Number(resetEpoch);
  return resetEpoch.trim() && Number.isFinite(reset) ? Math.min(Math.max(reset * 1000 - now, 0), MAX_WAIT_MS) : FALLBACK_WAIT_MS;
};
export const callName = (args) => args.find((a) => a === "graphql" || a.startsWith("/")) ?? args.join(" ");

// One wait allowance (MAX_WAIT_MS) for the whole run, shared by every API call, so the job always ends inside its timeout.
// GraphQL has its own limit bucket; every REST call shares `core`. Once the allowance is spent, a rate-limited call fails fast.
export const makeGh = ({ spawn = spawnSync, sleep: nap = sleep } = {}) => {
  let left = MAX_WAIT_MS;
  const waitForReset = (call) => {
    const resource = call === "graphql" ? "graphql" : "core";
    const r = spawn("gh", ["api", "rate_limit", "-q", `.resources.${resource}.reset`], { encoding: "utf8" });
    const ms = Math.min(waitMs(r.status === 0 ? r.stdout : ""), left);
    left -= ms;
    nap(ms);
  };
  return (args, input) => {
    for (let attempt = 1; ; attempt++) {
      const r = spawn("gh", args, { input, encoding: "utf8", maxBuffer: 1 << 28 });
      if (r.status === 0) return r.stdout;
      const message = `gh ${args.join(" ")} failed: ${r.stderr}`;
      if (!isRateLimit(r.stderr)) throw new Error(message);
      if (attempt === ATTEMPTS || left <= 0) throw Object.assign(new RateLimitError(message), { call: callName(args) });
      waitForReset(callName(args));
    }
  };
};
const gh = makeGh();

// Newer gh (runner) refuses output with terminal escape sequences; job logs carry ANSI colour, clean() strips it.
export const logArgs = (path) => ["api", "--allow-escape-sequences", path];

export const ghApi = {
  graphql: async (query, vars) =>
    JSON.parse(gh(["api", "graphql", "-f", `query=${query}`, ...Object.entries(vars).flatMap(([k, v]) => [typeof v === "number" ? "-F" : "-f", `${k}=${v}`])])).data,
  // ponytail: first page only (100); a PR with >100 comments could miss its sticky comment.
  get: async (path) => JSON.parse(gh(["api", path])),
  text: async (path) => gh(logArgs(path)),
  send: async (method, path, body) => gh(["api", "-X", method, path, "--input", "-"], JSON.stringify(body)),
};

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const arg = (k) => (process.argv.includes(k) ? process.argv[process.argv.indexOf(k) + 1] : undefined);
  const pr = Number(arg("--pr"));
  try {
    await report({ api: ghApi, repo: arg("--repo"), pr, dryRun: process.argv.includes("--dry-run"), eventHeadSha: arg("--head-sha"), eventIndex: Number(arg("--event") ?? -1) });
  } catch (e) {
    // Advisory report: a GitHub rate limit is infrastructure, not a PR fault, so it must not paint the PR red.
    if (!(e instanceof RateLimitError)) throw e;
    console.log(`::warning::merge-queue dequeue report skipped for PR #${pr}: GitHub rate limit persisted after ${ATTEMPTS} attempts on gh api ${e.call}`);
  }
}
