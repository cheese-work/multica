#!/usr/bin/env node
// Agent-facing PR field and the daily adoption issue (CHE-5717, replaces CHE-2794's per-deploy job).
//   PR check (pr-guards.yml): node scripts/agent-adoption.mjs check --event "$GITHUB_EVENT_PATH" --files <list>
//     <list> is the PR's changed files, one per line, read through the API (the job never checks out the PR). Fails a PR that touches CLI commands, the agent brief or the daemon without `Agent-facing:`
//     (and an `Adoption item:` when the answer is yes).
//   Daily (autopilot, on X99; needs `gh`, `multica`, tailnet access to C00; no checkout needed):
//     node scripts/agent-adoption.mjs daily --repo <owner/repo> --project <id> --assignee-id <id> --initial-sha <sha> [--dry-run]
//     Reads the revision C00 runs (C00_API_URL /api/config, the same readback as deploy/cd/check-deploy-freshness.py),
//     requires C00 /readyz to answer 200 for that same revision and the revision to descend from the checkpoint,
//     collects the merged PRs between the checkpoint and that revision, and opens ONE issue listing the
//     `Agent-facing: yes` ones. No such PR: opens nothing.
//     Checkpoint = the SHA marker in the newest "Agent adoption: C00 deploy" issue, read by paging `multica issue list --project <id>`
//     newest first until it is found (never a relevance search). It only moves when an issue opens. No such issue yet:
//     `--initial-sha` is the explicit first boundary. Any unreadable, truncated or inconsistent input exits non-zero
//     and opens nothing; the next run retries from the same checkpoint. PRs named in the previous issue are skipped.
//     When a yes PR changes CLI source (a usage string or a subcommand counts, a new flag is not needed), the issue also carries a
//     per-host CLI build check against that PR's merge commit (CHE-9942 W4) and 0.5 h more estimate.
//     The issue body is the per-deploy adoption issue (CHE-9922 W5, CHE-10123): for each agent-facing PR its signal, base, target, owner and
//     one result-line slot in the adoption-reconcile format (the adoption-reconcile format doc in the private store), plus evidence rules A1 to A4
//     and the 7-day window. A PR states `Adoption signal:`, `Adoption base:` and `Adoption target:` in its description (`Adoption old-path:` is optional).
//   Applies no instruction; that stays in `multica-cheese-work-instruction-change`.
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { liveLines } from "./fork-changelog.mjs";

const FIELD = /^Agent-facing:\s*(.*)$/i;
const ITEM = /^Adoption item:\s*(.*)$/i;
const SIGNAL = /^Adoption signal:\s*(.*)$/i;
const BASE = /^Adoption base:\s*(.*)$/i;
const TARGET = /^Adoption target:\s*(.*)$/i;
const OLD_PATH = /^Adoption old-path:\s*(.*)$/i;
const ISSUE_KEY = /^(?:Closes|Fixes|Resolves|Refs)\s+([A-Z][A-Z0-9]*-\d+)\b/im;
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

// The positive-use signal, activity base and target an agent-facing PR states (CHE-9922 W5). A placeholder counts as missing.
// An old-path count is optional and never enough alone (a count of 0 can mean no activity).
export function parseAdoptionSpec(body) {
  const spec = { signal: null, base: null, target: null, oldPath: null };
  for (const line of liveLines(typeof body === "string" ? body : "")) {
    for (const [key, re] of [["signal", SIGNAL], ["base", BASE], ["target", TARGET], ["oldPath", OLD_PATH]]) {
      const value = re.exec(line)?.[1].trim();
      if (value && !/^<.*>$/.test(value)) spec[key] = value;
    }
  }
  return spec;
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

// Only an explicit `Agent-facing: yes` counts. A missing field on an agent surface is the PR check's job (pr-guards.yml).
function classify(pr) {
  const { agentFacing, adoptionItem } = parseAgentFacing(pr.body);
  return agentFacing === "yes" ? { pr, adoptionItem, spec: parseAdoptionSpec(pr.body) } : null;
}

// 0.5 h to read + 0.25 h per PR (+ 0.5 h for the per-host CLI build check), rounded up to 0.5 h, capped at the 2 h issue limit.
const estimate = (n, cliBuild) => Math.min(2, Math.ceil((0.5 + 0.25 * n + (cliBuild ? 0.5 : 0)) * 2) / 2);

// prs: merged PRs in prev..next {number,title,author,body,paths,diff,mergeCommit}; seen: PR numbers earlier issues listed.
export function buildDaily({ prev, next, prs, seen = new Set(), repo = "cheese-work/multica" }) {
  const range = `${short(prev)}..${short(next)}`;
  const listed = new Set();
  const agentPrs = prs.filter((pr) => !seen.has(pr.number) && !listed.has(pr.number) && listed.add(pr.number)).map(classify).filter(Boolean);
  if (agentPrs.length === 0) return { issue: false, log: `no agent-facing PR in ${range}; no adoption issue`, title: "", body: "", prs: [] };

  const lines = agentPrs.flatMap(({ pr, adoptionItem, spec }) => prRow({ pr, adoptionItem, spec, next }));
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
  // The trigger is the CLI diff, not `paths`: gh lists only a PR's first 100 files, so a 3000-file upstream merge could be missed.
  const cliPrs = agentPrs.filter(({ pr }) => (pr.diff ?? "") !== "");
  const body = [
    `Estimate: ${estimate(agentPrs.length, cliPrs.length > 0)} h`,
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
    ...cliBuild(cliPrs.map(({ pr }) => pr), repo),
    ...EVIDENCE,
    "## Acceptance criteria",
    "1. Each listed PR's owner states per line which agent instruction or skill changes, or `none` with a reason (`multica issue get` read-back).",
    "2. Each change is applied through `multica-cheese-work-instruction-change` (cross-lab review), or the line records why none is needed.",
    "3. Every host row above reads back its flag (or the section says no new flags) before any instruction is applied.",
    "4. Each listed PR has one result line when its window ends (after the one 7-day extension at the latest), or this issue reports the gap.",
    ...(cliPrs.length > 0 ? ["5. Every host row in `CLI build per host` passes (or the section is absent) before any instruction is applied."] : []),
    "",
  ].join("\n");
  return { issue: true, log: `${agentPrs.length} agent-facing PR(s) in ${range}`, title: `Agent adoption: C00 deploy ${short(next)}`, body, prs: agentPrs.map(({ pr }) => pr.number) };
}

// A CLI change with no new flag (a usage string, a subcommand) gives the flag readback nothing to check. This section asks each host
// whether its CLI build contains every listed merge commit. A PR with no merge commit fails closed: an empty check would pass anything.
function cliBuild(prs, repo) {
  if (prs.length === 0) return [];
  const merges = prs.map((pr) => {
    if (!/^[0-9a-f]{40}$/.test(pr.mergeCommit ?? "")) throw new Error(`PR #${pr.number} changes CLI source but has no merge commit to check hosts against`);
    return `\`${pr.mergeCommit.slice(0, 10)}\``;
  });
  const command = `\`"$MULTICA_CLI_PATH" version\`, then \`gh api repos/${repo}/compare/<merge>...<host commit> --jq .status\``;
  return [
    "## CLI build per host",
    `These PRs change CLI source: ${prs.map((pr, i) => `#${pr.number} (${merges[i]})`).join(", ")}. Run on each host; paste the commit and the compare result for every listed merge commit.`,
    "",
    "| Host | Commands | Must contain | Result |",
    "| --- | --- | --- | --- |",
    ...HOSTS.map((host) => `| ${host} | ${command} | ${merges.join(", ")} | pending |`),
    "",
    "Pass: `ahead` or `identical` for each listed commit. Do not apply any instruction before every host row passes.",
    "",
  ];
}

// One PR: the `- #N` line (lastAdoption reads PR numbers from it) and its indented signal, base, target and result lines.
function prRow({ pr, adoptionItem, spec, next }) {
  const missing = ["signal", "base", "target"].filter((key) => !spec[key]).map((key) => `no ${key} line`);
  const key = ISSUE_KEY.exec(pr.body ?? "")?.[1] ?? "<KEY-N>";
  return [
    `- #${pr.number} ${pr.title} — agents change: ${adoptionItem ?? "not stated"} — owner: ${pr.author}`,
    `  - signal: ${spec.signal ?? "not stated"}`,
    `  - base: ${spec.base ?? "not stated"}`,
    `  - target: ${spec.target ?? "not stated"}`,
    ...(spec.oldPath ? [`  - old path (optional, never enough alone): ${spec.oldPath}`] : []),
    missing.length > 0
      ? `  - result: unknown — reason: ${missing.join(", ")}`
      : "  - result: pending — the window opens at the `deployed` write of the issue",
    `  - result line: ADOPTION-RESULT feature=pr-${pr.number} issue=${key} deployed_sha=${next} window_start=<UTC> window_end=<UTC> verdict=<PASS|FAIL|unknown>`,
  ];
}

const EVIDENCE = [
  "## Evidence and result lines",
  "The read-only C00 lane runs the queries and writes one ADOPTION-RESULT line per feature, as a comment on this issue, when the window ends.",
  "Format and field rules: the adoption-reconcile format doc in the private store. Replace the slot above: `deployed_sha` and `window_start` are the `deployed_sha` and `deployed_at` of the issue's `deployed` write (the slot shows the deploy SHA; use the issue's value if it differs), `window_end` is the measurement time, `verdict` is PASS, FAIL or unknown. A line that does not match the format is ignored.",
  "",
  "A PASS or a FAIL is valid only when all four hold. If one fails, the verdict is `unknown` and the reason goes on the line's comment:",
  "- A1. Signal coverage. The query covers every entry point of the new path that the PR names: each CLI command, route, UI action and event.",
  "- A2. Full observation window. The data source holds the whole window. Log rotation, a missing or restarted container, or shorter retention makes the evidence incomplete.",
  "- A3. Positive control. The same data source shows events for a known-used feature in the same window.",
  "- A4. Instrumented. Each entry point of the new path writes a row, an event or a log line at the deployed SHA.",
  "",
  "Window: 7 days from the `deployed` write for a SHA that contains the PR. Measure at the end of the window.",
  "- A PR with no signal, base or target line has result `unknown` with that reason. An old-path count alone never gives PASS: `0 old-command calls` can mean no activity.",
  "- If the activity base is below its minimum, or A1 to A4 do not all hold, the verdict is `unknown`, the issue stays `deployed` and the window extends once by 7 days, then report the gap here.",
  "- Below target at the end of the window: the issue stays `deployed`. Record a product fix here (make the new path the default, or refuse the old path). Skill text alone is not adoption.",
  "",
];

const TITLE = /^Agent adoption: C00 deploy /;
const MAX_PAGES = 100;

// Newest adoption issue's SHA (the range anchor) and the PR numbers it listed. list(offset) -> one page of
// `multica issue list --project <id> --sort created_at --direction desc --output json`; newest first, so the first adoption issue
// is the checkpoint and the walk stops there. Fails closed on a page that does not parse, an empty page that
// claims more, or a walk past maxPages.
export function lastAdoption(list, { maxPages = MAX_PAGES } = {}) {
  let offset = 0;
  for (let page = 0; page < maxPages; page++) {
    const { issues, has_more: more } = JSON.parse(list(offset));
    if (!Array.isArray(issues)) throw new Error("issue list output has no issues array");
    const found = issues.find((i) => TITLE.test(i.title ?? "") && MARKER.test(i.description ?? ""));
    if (found) {
      const text = found.description;
      return { sha: MARKER.exec(text)[1], prs: new Set([...text.matchAll(/^- #(\d+) /gm)].map((m) => Number(m[1]))) };
    }
    if (!more) return { sha: null, prs: new Set() };
    if (issues.length === 0) throw new Error("issue list returned an empty page but reports more");
    offset += issues.length;
  }
  throw new Error(`issue list walk hit its ${maxPages} page limit before reaching the last adoption issue`);
}

// deps: liveShort() -> C00's short commit, resolve(short) -> full SHA, list(offset) -> issue list page JSON,
// initial() -> the explicit first boundary (full SHA) or null, relation(prev, next) -> "ahead" | "behind" | ...,
// ready(next) -> throws unless C00 is healthy and still runs `next`, collectPrs(prev, next), create({title, body}).
export function daily(deps) {
  const next = deps.resolve(deps.liveShort());
  const { sha, prs: seen } = lastAdoption(deps.list);
  const prev = sha ?? deps.initial();
  if (!prev) throw new Error("no adoption issue exists yet and no --initial-sha was given; pass the first boundary explicitly");
  if (prev === next) return { log: `C00 still runs ${short(next)}; nothing deployed since the last adoption issue`, created: null };
  const relation = deps.relation(prev, next);
  if (relation !== "ahead") throw new Error(`C00 runs ${short(next)}, which is ${relation} the checkpoint ${short(prev)}, not a descendant (rollback or unrelated revision); nothing opened`);
  deps.ready(next);
  const plan = buildDaily({ prev, next, prs: deps.collectPrs(prev, next), seen, repo: deps.repo });
  if (!plan.issue) return { log: plan.log, created: null };
  return { log: plan.log, created: deps.create({ title: plan.title, body: plan.body }) };
}

const gh = (...args) => execFileSync("gh", args, { encoding: "utf8", maxBuffer: 256 * 1024 * 1024 });

// Added hunks under server/cmd/multica only; the flag readback must not pick up flags from other Go code.
// Test files (`*_test.go`) register fixture flags (`test --help`), not CLI flags, so they are dropped.
const isCliSource = (file) => file.startsWith("server/cmd/multica/") && !file.endsWith("_test.go");
export const cliDiff = (diff) =>
  diff.split(/^(?=diff --git )/m).filter((part) => isCliSource(/^diff --git a\/(\S+)/.exec(part)?.[1] ?? "")).join("");

// `gh pr diff` refuses a PR over 300 files (HTTP 406, an upstream merge has about 3000). The files API pages them, with a patch each.
export const diffFromFiles = (files) =>
  files.filter((f) => isCliSource(f.filename) && f.patch).map((f) => `diff --git a/${f.filename} b/${f.filename}\n+++ b/${f.filename}\n${f.patch}\n`).join("");

function prCliDiff(number, repo) {
  try {
    return cliDiff(gh("pr", "diff", String(number), "--repo", repo));
  } catch (error) {
    if (!/too_large|exceeded the maximum number of files/.test(`${error.stderr ?? ""}${error.message}`)) throw error;
    return diffFromFiles(JSON.parse(gh("api", "--paginate", "--slurp", `repos/${repo}/pulls/${number}/files`)).flat());
  }
}

// All reads go through the GitHub API: the checkout on an agent host is shallow and blobless, so local diffs fetch
// one blob at a time (about 1.5 s each). One `gh pr list` read gives bodies and paths (first 100 files per PR); a
// PR's diff is read only when it is agent-facing.
function collectPrs(prev, next, repo) {
  const since = gh("api", `repos/${repo}/commits/${prev}`, "--jq", ".commit.committer.date").trim().slice(0, 10);
  const listed = new Map(
    JSON.parse(gh("pr", "list", "--repo", repo, "--state", "merged", "--search", `merged:>=${since}`, "--limit", "1000", "--json", "number,title,body,author,files,mergeCommit"))
      .map((pr) => [pr.number, { title: pr.title, body: pr.body, author: pr.author?.login, paths: (pr.files ?? []).map((f) => f.path), mergeCommit: pr.mergeCommit?.oid }]),
  );
  const numbers = new Set();
  for (const subject of gh("api", "--paginate", `repos/${repo}/compare/${prev}...${next}`, "--jq", ".commits[].commit.message | split(\"\\n\")[0]").split("\n")) {
    const m = /\(#(\d+)\)$|^Merge pull request #(\d+)/.exec(subject);
    if (m) numbers.add(Number(m[1] ?? m[2]));
  }
  if (listed.size >= 1000) throw new Error("gh pr list hit its 1000 PR limit; the range is too wide to read in one call");
  // A `(#N)` commit with no merged PR in this repo is an upstream commit that came in with a sync merge: skipped.
  return [...numbers].filter((n) => listed.has(n)).sort((a, b) => a - b).map((n) => {
    const pr = listed.get(n);
    const out = { number: n, title: pr.title, author: pr.author, body: pr.body ?? "", paths: pr.paths, mergeCommit: pr.mergeCommit, diff: "" };
    if (classify(out)) out.diff = prCliDiff(n, repo);
    return out;
  });
}

const c00Url = (env, path) => `${(env.C00_API_URL || DEFAULT_C00_API_URL).replace(/\/$/, "")}${path}`;

// Health and SHA readback for `next` (the same pair a deploy readback checks): /readyz must answer 200 (curl -f
// fails on 5xx, so a rolling or failing deploy throws), and the revision on /api/config must be `next` before and after.
function ready(next, env) {
  const same = () => {
    if (!next.startsWith(liveShort(env))) throw new Error(`C00 revision changed during the readback; expected ${short(next)}`);
  };
  same();
  execFileSync("curl", ["-fsS", "--max-time", "15", "-o", "/dev/null", c00Url(env, "/readyz")], { encoding: "utf8" });
  same();
}

function liveShort(env) {
  const url = c00Url(env, "/api/config");
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
    const multica = (...args) => execFileSync("multica", args, { encoding: "utf8", maxBuffer: 256 * 1024 * 1024 });
    const out = daily({
      liveShort: () => liveShort(env),
      resolve: (shortSha) => gh("api", `repos/${repo}/commits/${shortSha}`, "--jq", ".sha").trim(),
      list: (offset) => multica("issue", "list", "--project", arg(argv, "project"), "--sort", "created_at", "--direction", "desc", "--limit", "100", "--offset", String(offset), "--output", "json"),
      initial: () => gh("api", `repos/${repo}/commits/${arg(argv, "initial-sha")}`, "--jq", ".sha").trim(),
      relation: (prev, next) => gh("api", `repos/${repo}/compare/${prev}...${next}`, "--jq", ".status").trim(),
      ready: (next) => ready(next, env),
      repo,
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
  console.error("usage: agent-adoption.mjs check --event <file> --files <list> | daily --repo <owner/repo> --project <id> --assignee-id <id> --initial-sha <sha> [--dry-run]");
  return 2;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = main(process.argv.slice(2), process.env);
}
