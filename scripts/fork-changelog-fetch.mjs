#!/usr/bin/env node
import { appendFileSync, rmSync } from "node:fs";
import { execFileSync } from "node:child_process";
import { pathToFileURL } from "node:url";
import { generateEntries, loadBackfill, optionsFromEnv, serializeEntries, writeAtomically } from "./fork-changelog.mjs";

export const FETCH_DEADLINE_MS = 180_000;
const annotate = (message) => console.warn(`::warning::${message.replace(/%/g, "%25").replace(/\r/g, "%0D").replace(/\n/g, "%0A")}`);

export async function fetchChangelog({
  token, out, firstParent, repository = "cheese-work/multica", fetchImpl = fetch,
  generator = generateEntries, deadlineMs = FETCH_DEADLINE_MS, summaryPath,
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
      const result = generator({ prs, firstParent }, options);
      checkDeadline();
      writeAtomically(out, serializeEntries(result));
      checkDeadline();
      // Merged PRs were already warned at PR time (pr-changelog-check); a plain log keeps every build free of annotations nobody can act on.
      for (const { number, message } of result.warnings) note(`PR #${number}: ${message}`);
      if (summaryPath) {
        const { shown, hidden, byReason } = result.counts;
        appendFileSync(summaryPath, `### Fork changelog\n\nShown: ${shown}\n\nHidden: ${hidden}\n${Object.entries(byReason).sort().map(([reason, count]) => `\n- ${reason}: ${count}`).join("")}\n`);
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
    const history = execFileSync("git", ["log", "--first-parent", "--format=%H%x00%B%x00", sha], {
      encoding: "utf8", timeout: FETCH_DEADLINE_MS, maxBuffer: 32 * 1024 * 1024,
    }).split("\0");
    const firstParent = [];
    for (let index = 0; index + 1 < history.length; index += 2) firstParent.push({ sha: history[index].trim(), message: history[index + 1] });
    await fetchChangelog({ token: env.GH_TOKEN, out, firstParent, repository: env.GITHUB_REPOSITORY,
      summaryPath: env.GITHUB_STEP_SUMMARY, deadlineMs: FETCH_DEADLINE_MS - (performance.now() - started), ...options });
  } catch (error) {
    try { rmSync(out, { force: true }); } catch {}
    annotate(`Fork entries unavailable for this build: ${error.message}`);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) await main(process.argv.slice(2), process.env);
