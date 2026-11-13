#!/usr/bin/env node
import { appendFileSync, rmSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { generateEntries, loadBackfill, optionsFromEnv, serializeEntries, writeAtomically } from "./fork-changelog.mjs";

export const FETCH_DEADLINE_MS = 180_000;
// CHE-4495 moved the C00 deploy from c00-production to c00-runtime (8 Nov 2026); older deploys stay in c00-production.
const DEPLOY_ENVIRONMENTS = ["c00-runtime", "c00-production"];
// A c00-runtime run makes one deployment per environment job; only the `deploy` job is a deploy.
const RUNTIME_ENVIRONMENT = "c00-runtime";
const DEPLOY_JOB = "deploy";
// GitHub caps concurrent requests (secondary limit); 100+ parallel job lookups got 403 and dropped every divider (CHE-5172).
const JOB_LOOKUP_CONCURRENCY = 8;

// "<UPSTREAM_VERSION at that commit>-<release stamp of the commit time>", e.g. 0.7.2-20261105-1426.
export function versionLabel(sha, { cwd = process.cwd() } = {}) {
  const git = (...args) => execFileSync("git", ["-C", cwd, ...args], { encoding: "utf8" }).trim();
  const upstream = git("show", `${sha}:UPSTREAM_VERSION`);
  const stamp = execFileSync("sh", [join(dirname(fileURLToPath(import.meta.url)), "release-stamp.sh"), git("show", "-s", "--format=%cI", sha)], { encoding: "utf8" }).trim();
  return `${upstream}-${stamp}`;
}

// GitHub flips a superseded deployment to INACTIVE, so "latest status" hides prior successes:
// a deployment counts when any status was SUCCESS, dated at the earliest one.
export const deploymentsFromGraphql = (json) =>
  (json?.data?.repository?.deployments?.nodes ?? []).flatMap((node) => {
    const successes = (node?.statuses?.nodes ?? []).filter((status) => status?.state === "SUCCESS" && status.createdAt)
      .sort((a, b) => Date.parse(a.createdAt) - Date.parse(b.createdAt));
    const [, runId, jobId] = /\/actions\/runs\/(\d+)\/job\/(\d+)/.exec(successes[0]?.logUrl ?? "") ?? [];
    return node?.commitOid && successes.length
      ? [{ sha: node.commitOid, state: "SUCCESS", createdAt: successes[0].createdAt, environment: node.environment, runId, jobId }] : [];
  });

// Pages the C00 deploy deployments (newest first) through GraphQL; throws on any failure.
// `report(message)` receives non-fatal read problems (CHE-5172) so they reach the CI annotation and step summary.
export async function fetchDeployments({ token, repository, fetchImpl = fetch, signal, report = () => {}, concurrency = JOB_LOOKUP_CONCURRENCY, retryDelayMs = 500 }) {
  const [owner, name] = repository.split("/");
  const query = `query($owner:String!,$name:String!,$after:String){repository(owner:$owner,name:$name){deployments(environments:${JSON.stringify(DEPLOY_ENVIRONMENTS)},first:100,after:$after,orderBy:{field:CREATED_AT,direction:DESC}){pageInfo{hasNextPage endCursor} nodes{commitOid environment statuses(first:20){nodes{state createdAt logUrl}}}}}}`;
  const all = [];
  for (let after = null, pages = 0; pages < 20; pages += 1) {
    const response = await fetchImpl("https://api.github.com/graphql", {
      method: "POST", signal,
      headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
      body: JSON.stringify({ query, variables: { owner, name, after } }),
    });
    if (!response.ok) throw new Error(`deployments: HTTP ${response.status}`);
    const json = await response.json();
    if (json.errors?.length) throw new Error(`deployments: ${json.errors[0].message}`);
    all.push(...deploymentsFromGraphql(json));
    const info = json.data?.repository?.deployments?.pageInfo;
    if (!info?.hasNextPage) break;
    after = info.endCursor;
  }
  return keepDeployJobs(all, { token, repository, fetchImpl, signal, report, concurrency, retryDelayMs });
}

// Reads one run's jobs; retries a 403/429/5xx (GitHub's secondary limits) a couple of times. Returns the job list or an error string.
async function readRunJobs(run, { token, repository, fetchImpl, signal, retryDelayMs }) {
  let problem = "";
  for (let attempt = 0; attempt < 3; attempt += 1) {
    if (attempt) await new Promise((resolve) => setTimeout(resolve, retryDelayMs * attempt));
    try {
      const response = await fetchImpl(`https://api.github.com/repos/${repository}/actions/runs/${run}/jobs?per_page=100`, {
        signal, headers: { Authorization: `Bearer ${token}`, Accept: "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28" },
      });
      if (response.ok) return (await response.json()).jobs ?? [];
      const message = await response.json().then((body) => body?.message, () => undefined);
      problem = `HTTP ${response.status}${message ? ` ${message}` : ""}`;
      if (response.status !== 403 && response.status !== 429 && response.status < 500) break;
    } catch (error) {
      if (signal?.aborted) throw error;
      problem = error.message;
    }
  }
  return problem;
}

// Drops c00-runtime deployments that belong to another job of the run (prepare-release-candidate); needs `actions: read`.
// A run whose jobs cannot be read loses only its own deploy; the rest keep their dividers (CHE-5172).
async function keepDeployJobs(deployments, { token, repository, fetchImpl, signal, report, concurrency, retryDelayMs }) {
  const runs = [...new Set(deployments.filter((d) => d.environment === RUNTIME_ENVIRONMENT).map((d) => d.runId))];
  const deployJobIds = new Set();
  const failures = [];
  let next = 0;
  const worker = async () => {
    while (next < runs.length) {
      const run = runs[next++];
      const jobs = await readRunJobs(run, { token, repository, fetchImpl, signal, retryDelayMs });
      if (typeof jobs === "string") failures.push({ run, problem: jobs });
      else for (const job of jobs) if (job.name === DEPLOY_JOB) deployJobIds.add(String(job.id));
    }
  };
  await Promise.all(Array.from({ length: Math.min(concurrency, runs.length) }, worker));
  if (failures.length) {
    report(`Deploy dividers incomplete: the jobs of ${failures.length} of ${runs.length} runs could not be read, so their deploys are missing (run ${failures[0].run}: ${failures[0].problem})`);
  }
  return deployments.filter((d) => d.environment !== RUNTIME_ENVIRONMENT || deployJobIds.has(d.jobId));
}
const annotate = (message) => console.warn(`::warning::${message.replace(/%/g, "%25").replace(/\r/g, "%0D").replace(/\n/g, "%0A")}`);

export async function fetchChangelog({
  token, out, firstParent, repository = "cheese-work/multica", fetchImpl = fetch,
  generator = generateEntries, deployments = null, deadlineMs = FETCH_DEADLINE_MS, summaryPath,
  warning = annotate, note = console.log, ...options
}) {
  const controller = new AbortController();
  const started = performance.now();
  let timer;
  const checkDeadline = () => {
    if (controller.signal.aborted || performance.now() - started >= deadlineMs) throw new Error("3-minute deadline exceeded");
  };
  try {
    rmSync(out, { force: true });
    if (!token) throw new Error("GH_TOKEN is unavailable");
    if (!/^[\w.-]+\/[\w.-]+$/.test(repository)) throw new Error("Invalid repository");
    if (!Array.isArray(firstParent)) throw new Error("First-parent history is unavailable");
    const start = Date.parse(`${options.startDate ?? "2026-10-11"}T00:00:00+07:00`);
    if (Number.isNaN(start)) throw new Error("Invalid start date");
    const deadline = new Promise((_, reject) => {
      timer = setTimeout(() => {
        controller.abort();
        reject(new Error("3-minute deadline exceeded"));
      }, deadlineMs);
    });
    const work = async () => {
      const prs = [];
      let stop = false;
      for (let pageNumber = 1; !stop; pageNumber += 1) {
        checkDeadline();
        const url = new URL(`https://api.github.com/repos/${repository}/pulls`);
        url.search = new URLSearchParams({ state: "closed", base: "main", sort: "updated", direction: "desc", per_page: "100", page: String(pageNumber) });
        const response = await fetchImpl(url.href, {
          headers: { Authorization: `Bearer ${token}`, Accept: "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28" },
          signal: controller.signal,
        });
        if (!response.ok) throw new Error(`GitHub returned HTTP ${response.status}`);
        const items = await response.json();
        checkDeadline();
        if (!Array.isArray(items)) throw new Error("GitHub returned an invalid PR list");
        for (const pr of items) {
          const updated = Date.parse(pr.updated_at);
          if (Number.isNaN(updated)) throw new Error("GitHub returned an invalid update date");
          if (updated < start) { stop = true; break; }
          if (pr.merged_at) prs.push({
            number: pr.number, title: pr.title, body: pr.body, headRef: pr.head?.ref,
            mergeSha: pr.merge_commit_sha, mergedAt: pr.merged_at,
          });
        }
        if (!items.length || !/;\s*rel="next"/.test(response.headers?.get("link") ?? "")) stop = true;
      }
      // Dividers are optional: a deployment-read failure keeps the entries and drops only `deploys`.
      let deploymentList = null;
      const dividerNotes = [];
      if (deployments) {
        // Dividers get half the remaining budget, so a slow read cannot cost the entries.
        const budget = new AbortController();
        let budgetTimer;
        const budgetExpired = new Promise((_, reject) => {
          budgetTimer = setTimeout(() => { budget.abort(); reject(new Error("deployment read exceeded its time budget")); }, Math.max(0, deadlineMs - (performance.now() - started)) / 2);
        });
        const abortWithParent = () => budget.abort();
        controller.signal.addEventListener("abort", abortWithParent, { once: true });
        try { deploymentList = await Promise.race([deployments(budget.signal, (note) => dividerNotes.push(note)), budgetExpired]); } catch (error) {
          dividerNotes.push(`Deploy dividers unavailable for this build: ${error.message}`);
        } finally {
          clearTimeout(budgetTimer);
          controller.signal.removeEventListener("abort", abortWithParent);
        }
      }
      checkDeadline();
      const result = generator({ prs, firstParent, deployments: deploymentList }, options);
      checkDeadline();
      writeAtomically(out, serializeEntries(result));
      checkDeadline();
      // Merged PRs were already warned at PR time (pr-guards); a plain log keeps every build free of annotations nobody can act on.
      for (const { number, message } of result.warnings) note(`PR #${number}: ${message}`);
      for (const note of dividerNotes) warning(note);
      if (summaryPath) {
        const { shown, hidden, byReason } = result.counts;
        appendFileSync(summaryPath, `### Fork changelog\n\nShown: ${shown}\n\nHidden: ${hidden}\n${Object.entries(byReason).sort().map(([reason, count]) => `\n- ${reason}: ${count}`).join("")}\n${dividerNotes.map((note) => `\n${note}\n`).join("")}`);
      }
      checkDeadline();
      return result;
    };
    return await Promise.race([work(), deadline]);
  } catch (error) {
    controller.abort();
    try { rmSync(out, { force: true }); } catch {}
    warning(`Fork entries unavailable for this build: ${error.message}`);
    if (summaryPath) {
      try { appendFileSync(summaryPath, "### Fork changelog\n\nFork entries unavailable for this build.\n"); } catch {}
    }
    return null;
  } finally {
    clearTimeout(timer);
  }
}

export async function main(argv, env) {
  const started = performance.now();
  const args = {};
  let out = "apps/web/.fork-changelog/entries.json";
  try {
    for (let index = 0; index < argv.length; index += 2) {
      if (!argv[index].startsWith("--") || !argv[index + 1]) throw new Error("Invalid argument");
      args[argv[index].slice(2)] = argv[index + 1];
    }
    out = args.out ?? out;
    rmSync(out, { force: true });
    if (!env.GH_TOKEN) {
      await fetchChangelog({ token: env.GH_TOKEN, out, firstParent: [], summaryPath: env.GITHUB_STEP_SUMMARY });
      return;
    }
    const options = { backfill: loadBackfill(), ...optionsFromEnv(env) };
    if (args.start) options.startDate = args.start;
    if (args["sync-prefix"]) options.syncPrefix = args["sync-prefix"];
    const sha = args.sha ?? env.GITHUB_SHA;
    if (!/^[a-f0-9]{40}$/.test(sha ?? "")) throw new Error("A qualified source SHA is required");
    const history = execFileSync("git", ["log", "--first-parent", "--format=%H%x00%cI%x00%B%x00", sha], {
      encoding: "utf8", timeout: FETCH_DEADLINE_MS, maxBuffer: 32 * 1024 * 1024,
    }).split("\0");
    const firstParent = [];
    for (let index = 0; index + 2 < history.length; index += 3) firstParent.push({ sha: history[index].trim(), time: history[index + 1], message: history[index + 2] });
    await fetchChangelog({ token: env.GH_TOKEN, out, firstParent, repository: env.GITHUB_REPOSITORY, versionOf: (commit) => { try { return versionLabel(commit); } catch { return commit.slice(0, 7); } },
      deployments: (signal, report) => fetchDeployments({ token: env.GH_TOKEN, repository: env.GITHUB_REPOSITORY, signal, report }),
      summaryPath: env.GITHUB_STEP_SUMMARY, deadlineMs: FETCH_DEADLINE_MS - (performance.now() - started), ...options });
  } catch (error) {
    try { rmSync(out, { force: true }); } catch {}
    annotate(`Fork entries unavailable for this build: ${error.message}`);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) await main(process.argv.slice(2), process.env);
