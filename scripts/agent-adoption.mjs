#!/usr/bin/env node
// Agent-facing PR field and the daily adoption issue (CHE-5717, replaces CHE-2794's per-deploy job).
//   PR check (pr-guards.yml): node scripts/agent-adoption.mjs check --event "$GITHUB_EVENT_PATH" --files <list>
//     <list> is the PR's changed files, one per line, read through the API (the job never checks out the PR). Fails a PR that touches CLI commands, the agent brief or the daemon without `Agent-facing:`
//     (and an `Adoption item:` when the answer is yes).
//   Daily (autopilot, run in a checkout of this repo on X99):
//     node scripts/agent-adoption.mjs daily --repo <owner/repo> --project <id> --assignee-id <id> [--dry-run]
//     Reads the revision C00 runs (C00_API_URL /api/config, the same readback as deploy/cd/check-deploy-freshness.py),
//     collects the merged PRs between the last adoption issue's SHA and that revision, and opens ONE issue listing
//     the `Agent-facing: yes` ones. No such PR: opens nothing. The anchor only moves when an issue opens, so a PR is
//     listed once; PRs named in earlier adoption issues are skipped as a second guard.
//   Applies no instruction; that stays in `multica-cheese-work-instruction-change`.
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { liveLines } from "./fork-changelog.mjs";

const FIELD = /^Agent-facing:\s*(.*)$/i;
const ITEM = /^Adoption item:\s*(.*)$/i;
const HOSTS = ["X99", "MBP", "C00"];
const MARKER = /<!-- agent-adoption-daily: sha:([0-9a-f]{40}) -->/;
const DEFAULT_C00_API_URL = "https://congvc-c00.taila6fa8a.ts.net:19444";
// CLI commands, the daemon (which renders the agent brief) and server-side brief builders; tests excluded.
const AGENT_SURFACE = [
  /^server\/cmd\/multica\/(?!.*_test\.go$)/,
  /^server\/internal\/daemon\/(?!.*_test\.go$)/,
  /^server\/internal\/handler\/[^/]*brief[^/]*\.go$/,
  /^server\/internal\/handler\/agent\.go$/,
  /^server\/internal\/issueworkflow\/(?!.*_test\.go$)/,
];
const FLAG = /(\w+)\.(?:Persistent)?Flags\(\)\.\w+\((?:&[\w.]+,\s*)?"([a-z][a-z0-9-]*)"/;

const short = (sha) => sha.slice(0, 12);

export function parseAgentFacing(body) {
  let agentFacing = null;
  let adoptionItem = null;
  for (const line of liveLines(typeof body === "string" ? body : "")) {
    const field = FIELD.exec(line);
    if (field && /^(yes|no)$/i.test(field[1].trim())) agentFacing = field[1].trim().toLowerCase();
    const item = ITEM.exec(line);
    const value = item?.[1].trim();
    if (value && !/^<.*>$/.test(value)) adoptionItem = value;
  }
  return { agentFacing, adoptionItem };
}

export const touchesAgentSurface = (paths) => paths.some((path) => AGENT_SURFACE.some((re) => re.test(path)));

export function checkAgentFacing(body, paths) {
  if (!touchesAgentSurface(paths)) return [];
  const { agentFacing, adoptionItem } = parseAgentFacing(body);
  if (!agentFacing) return ["This PR touches CLI commands, the agent brief or the daemon: add `Agent-facing: yes` or `Agent-facing: no` to the description."];
  if (agentFacing === "yes" && !adoptionItem) return ["`Agent-facing: yes` needs an `Adoption item:` line saying what agents must change."];
  return [];
}

// Added `<recv>.Flags()` / `.PersistentFlags()` lines of a Go diff. A `fooBarCmd` receiver names its
// command ("foo bar"); `rootCmd` is the top level; any other receiver (`cmd`, `c`, `command`) falls
// back to the file name (`cmd_issue_label.go` -> "issue label"), which the issue body asks to verify.
export function newCliFlags(diff) {
  const flags = [];
  let file = "";
  for (const line of diff.split("\n")) {
    if (line.startsWith("+++ ")) file = line.slice(4).replace(/^b\//, "");
    if (!line.startsWith("+") || line.startsWith("+++")) continue;
    const match = FLAG.exec(line);
    if (!match) continue;
    const [, receiver, flag] = match;
    const named = /^(\w+)Cmd$/.exec(receiver)?.[1];
    const command = named === "root" ? ""
      : named ? named.replace(/([A-Z])/g, " $1").toLowerCase()
      : /([^/]*?)\.go$/.exec(file)?.[1].replace(/^cmd_/, "").replaceAll("_", " ") ?? "";
    flags.push({ command, flag });
  }
  return flags;
}

function classify(pr) {
  const { agentFacing, adoptionItem } = parseAgentFacing(pr.body);
  if (agentFacing === "no") return null;
  if (agentFacing === "yes") return { pr, adoptionItem, fieldMissing: false };
  return touchesAgentSurface(pr.paths) ? { pr, adoptionItem: null, fieldMissing: true } : null;
}

// 0.5 h to read + 0.25 h per PR, rounded up to 0.5 h, capped at the 2 h issue limit.
const estimate = (n) => Math.min(2, Math.ceil((0.5 + 0.25 * n) * 2) / 2);

// prs: merged PRs in prev..next {number,title,author,body,paths,diff}; seen: PR numbers earlier issues listed.
export function buildDaily({ prev, next, prs, seen = new Set() }) {
  const range = `${short(prev)}..${short(next)}`;
  const listed = new Set();
  const agentPrs = prs.filter((pr) => !seen.has(pr.number) && !listed.has(pr.number) && listed.add(pr.number)).map(classify).filter(Boolean);
  if (agentPrs.length === 0) return { issue: false, log: `no agent-facing PR in ${range}; no adoption issue`, title: "", body: "", prs: [] };

  const lines = agentPrs.map(({ pr, adoptionItem, fieldMissing }) => {
    const change = fieldMissing ? "field missing, path heuristic; author to confirm" : `agents change: ${adoptionItem ?? "not stated"}`;
    return `- #${pr.number} ${pr.title} — ${change} — owner: @${pr.author}`;
  });
  const flags = agentPrs.flatMap(({ pr }) => newCliFlags(pr.diff ?? ""));
  const readback = flags.length === 0
    ? ["No new CLI flags in this range. No host readback needed."]
    : [
        "Run each command on the host itself and paste the result. The command path comes from the Go variable name; correct it if the flag sits elsewhere.",
        "",
        "| Host | Command | Result |",
        "| --- | --- | --- |",
        ...flags.flatMap(({ command, flag }) =>
          HOSTS.map((host) => `| ${host} | \`${["multica", command, "--help"].filter(Boolean).join(" ")} \\| grep -- --${flag}\` | pending |`)),
        "",
        "Do not apply any instruction before every host row reads back with its flag listed.",
      ];
  const body = [
    `Estimate: ${estimate(agentPrs.length)} h`,
    "",
    `<!-- agent-adoption-daily: sha:${next} -->`,
    `Deployed to C00: \`${next}\`. Previous adoption SHA: \`${prev}\`.`,
    "",
    "## Agent-facing PRs",
    ...lines,
    "",
    "## CLI readback per host",
    ...readback,
    "",
    "## Acceptance criteria",
    "1. Each listed PR's owner states per line which agent instruction or skill changes, or `none` with a reason (`multica issue get` read-back).",
    "2. Each change is applied through `multica-cheese-work-instruction-change` (cross-lab review), or the line records why none is needed.",
    "3. Every host row above reads back its flag (or the section says no new flags) before any instruction is applied.",
    "",
  ].join("\n");
  return { issue: true, log: `${agentPrs.length} agent-facing PR(s) in ${range}`, title: `Agent adoption: C00 deploy ${short(next)}`, body, prs: agentPrs.map(({ pr }) => pr.number) };
}

// Newest adoption issue's SHA (the range anchor) and every PR number any adoption issue listed.
export function lastAdoption(searchJson) {
  const { issues } = JSON.parse(searchJson);
  if (!Array.isArray(issues)) throw new Error("issue search output has no issues array");
  const mine = issues
    .map((i) => ({ created: i.created_at ?? "", text: i.description ?? "", sha: MARKER.exec(i.description ?? "")?.[1] }))
    .filter((i) => i.sha);
  mine.sort((a, b) => b.created.localeCompare(a.created));
  const prs = new Set(mine.flatMap(({ text }) => [...text.matchAll(/^- #(\d+) /gm)].map((m) => Number(m[1]))));
  return { sha: mine[0]?.sha ?? null, prs };
}

// deps: liveShort() -> C00's short commit, resolve(short) -> full SHA, search() -> issue search JSON,
// bootstrap(next) -> anchor when no adoption issue exists yet, collectPrs(prev, next), create({title, body}) -> identifier.
export function daily(deps) {
  const next = deps.resolve(deps.liveShort());
  const { sha, prs: seen } = lastAdoption(deps.search());
  const prev = sha ?? deps.bootstrap(next);
  if (prev === next) return { log: `C00 still runs ${short(next)}; nothing deployed since the last adoption issue`, created: null };
  const plan = buildDaily({ prev, next, prs: deps.collectPrs(prev, next), seen });
  if (!plan.issue) return { log: plan.log, created: null };
  return { log: plan.log, created: deps.create({ title: plan.title, body: plan.body }) };
}

const git = (...args) => execFileSync("git", args, { encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });

function collectPrs(prev, next, repo) {
  const prs = [];
  for (const row of git("log", "--first-parent", "--format=%H%x09%s", `${prev}..${next}`).split("\n").filter(Boolean)) {
    const [sha, subject] = row.split("\t");
    const number = /\(#(\d+)\)$|^Merge pull request #(\d+)/.exec(subject);
    if (!number) continue;
    const n = number[1] ?? number[2];
    const pr = JSON.parse(execFileSync("gh", ["api", `repos/${repo}/pulls/${n}`, "--jq", "{title,body,author:.user.login}"], { encoding: "utf8" }));
    prs.push({
      number: Number(n),
      title: pr.title,
      author: pr.author,
      body: pr.body ?? "",
      paths: git("diff", "--name-only", `${sha}^1`, sha).split("\n").filter(Boolean),
      diff: git("diff", "-U0", `${sha}^1`, sha, "--", "server/cmd/multica"),
    });
  }
  return prs;
}

function liveShort(env) {
  const url = `${(env.C00_API_URL || DEFAULT_C00_API_URL).replace(/\/$/, "")}/api/config`;
  const out = execFileSync("curl", ["-fsS", "--max-time", "15", url], { encoding: "utf8" });
  const commit = JSON.parse(out).server_commit;
  if (!/^[0-9a-f]{7,40}$/.test(commit ?? "")) throw new Error(`C00 /api/config has no usable server_commit (got ${JSON.stringify(commit)})`);
  return commit;
}

function arg(argv, name) {
  const at = argv.indexOf(`--${name}`);
  if (at < 0 || !argv[at + 1]) throw new Error(`missing --${name}`);
  return argv[at + 1];
}

export function main(argv, env) {
  const [mode] = argv;
  if (mode === "check") {
    const { pull_request: pr } = JSON.parse(readFileSync(arg(argv, "event"), "utf8"));
    let paths;
    try {
      paths = readFileSync(arg(argv, "files"), "utf8").split("\n").filter(Boolean);
    } catch (error) {
      console.log(`::error::Could not read the changed-file list (${error.code ?? error.message}); re-run the check.`);
      return 1;
    }
    const errors = checkAgentFacing(pr.body ?? "", paths);
    for (const error of errors) console.log(`::error::${error}`);
    if (errors.length === 0) console.log("Agent-facing field: ok");
    return errors.length === 0 ? 0 : 1;
  }
  if (mode === "daily") {
    const repo = arg(argv, "repo");
    const dry = argv.includes("--dry-run");
    const multica = (...args) => execFileSync("multica", args, { encoding: "utf8" });
    const out = daily({
      liveShort: () => liveShort(env),
      resolve: (shortSha) => git("rev-parse", "--verify", `${shortSha}^{commit}`).trim(),
      search: () => multica("issue", "search", "agent-adoption-daily", "--include-closed", "--limit", "100", "--output", "json"),
      bootstrap: (next) => git("rev-list", "-1", `--before=${new Date(Date.now() - 864e5).toISOString()}`, next).trim(),
      collectPrs: (prev, next) => collectPrs(prev, next, repo),
      create: ({ title, body }) => {
        if (dry) return `(dry run) ${title}\n${body}`;
        const file = join(mkdtempSync(join(tmpdir(), "adoption-")), "body.md");
        writeFileSync(file, body);
        // Project, assignee and Readiness at creation (CHE-466).
        const created = multica("issue", "create", "--title", title, "--description-file", file, "--allow-external-file",
          "--project", arg(argv, "project"), "--assignee-id", arg(argv, "assignee-id"), "--status", "todo",
          "--property", "Readiness=ready", "--output", "json");
        return JSON.parse(created).identifier;
      },
    });
    console.log(out.log);
    if (out.created) console.log(`opened ${out.created}`);
    return 0;
  }
  console.error("usage: agent-adoption.mjs check --event <file> --files <list> | daily --repo <owner/repo> --project <id> --assignee-id <id> [--dry-run]");
  return 2;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = main(process.argv.slice(2), process.env);
}
