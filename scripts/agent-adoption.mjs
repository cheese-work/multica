#!/usr/bin/env node
// Agent-facing PR field and post-deploy adoption issue (CHE-2794). Pure functions plus two CLI modes:
//   PR check (CI):   node scripts/agent-adoption.mjs check --event "$GITHUB_EVENT_PATH"
//     Fails a PR that touches CLI commands, the agent brief or the daemon without the
//     `Agent-facing:` field (and an `Adoption item:` when the answer is yes).
//   Post-deploy:     node scripts/agent-adoption.mjs plan --prev <sha> --next <sha> --repo <owner/repo> --out <dir>
//     Reads merged PRs in prev..next from git and `gh`, writes <dir>/title.txt and <dir>/body.md and
//     prints `issue=true|false` plus the log line. Opening or reusing the issue stays with the workflow.
//     Classifies first. Only a range with an agent-facing PR fetches (ADOPTION_GENERATOR_TOKEN, at
//     ADOPTION_GENERATOR_REF, default the pinned CHE-2793 SHA) and runs the CHE-2793 generator, or runs
//     ADOPTION_MAP_CMD instead, with ADOPTION_PREV/NEXT/REPO in its env; its stdout is the adoption map.
//     A failing command, a missing token or an empty map fails the plan.
//   Dedup:           node scripts/agent-adoption.mjs reuse --next <sha> [--file <search.json>]
//     Reads `multica issue search --output json`; exit 0 + identifier = same-SHA issue exists, 1 = none,
//     2 = the lookup output is unusable (the workflow must not create after a 2).
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { liveLines } from "./fork-changelog.mjs";

const FIELD = /^Agent-facing:\s*(.*)$/i;
const ITEM = /^Adoption item:\s*(.*)$/i;
const HOSTS = ["X99", "MBP", "C00"];
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

// `map` is optional text from the CHE-2793 map generator, appended verbatim.
export function buildAdoption({ prev, next, prs, map = "" }) {
  const range = `${short(prev)}..${short(next)}`;
  const agentPrs = prs.map(classify).filter(Boolean);
  if (agentPrs.length === 0) return { issue: false, log: `no agent-facing PR in ${range}; no adoption issue`, title: "", body: "" };

  const lines = agentPrs.map(({ pr, adoptionItem, fieldMissing }) => {
    const detail = fieldMissing ? "field missing, path heuristic; author to confirm" : `adoption item: ${adoptionItem ?? "not stated"}`;
    return `- #${pr.number} ${pr.title} — ${detail}`;
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
    `<!-- adoption-key: sha:${next} -->`,
    `Deployed to C00: \`${next}\`. Previous deployed SHA: \`${prev}\`.`,
    "",
    "## Agent-facing PRs",
    ...lines,
    ...(map ? ["", "## Adoption map", map.trim()] : []),
    "",
    "## CLI readback per host",
    ...readback,
    "",
    "## Apply",
    "Use `multica-cheese-work-instruction-change` (cross-lab review, Cheese merges). The deploy job applies nothing.",
    "",
  ].join("\n");
  return { issue: true, log: `${agentPrs.length} agent-facing PR(s) in ${range}`, title: `Agent adoption: C00 deploy ${short(next)}`, body };
}

// The CHE-2793 map generator, run as a shell command. An empty map is an error: the issue must never
// ship without the instruction surfaces the generator found.
export function runMapCommand(command, { prev, next, repo }, env = process.env) {
  const map = execFileSync("sh", ["-c", command], {
    encoding: "utf8",
    env: { ...env, ADOPTION_PREV: prev, ADOPTION_NEXT: next, ADOPTION_REPO: repo },
  });
  if (!map.trim()) throw new Error("adoption map generator printed an empty map");
  return map;
}

// The reviewed CHE-2793 generator (multica-dotfiles PR #430 head). Repin to a main SHA once that PR merges.
export const DEFAULT_GENERATOR_REF = "3e781bd8fd2c53d610728b753c4f6f2e8fef613f";

const FORK_RANGE_COMMAND =
  'python3 "$ADOPTION_GENERATOR" fork-range --git-dir "$(git rev-parse --absolute-git-dir)" --prev "$ADOPTION_PREV" --next "$ADOPTION_NEXT" --repo "$ADOPTION_REPO"';

// The generator lives in the private multica-dotfiles repo; the token needs read access to it.
export function fetchGenerator(ref, token) {
  const path = join(mkdtempSync(join(tmpdir(), "adoption-generator-")), "upstream_sync_plan.py");
  const source = execFileSync("gh", ["api", `repos/cheese-work/multica-dotfiles/contents/scripts/upstream_sync_plan.py?ref=${ref}`, "-H", "Accept: application/vnd.github.raw+json"], {
    encoding: "utf8",
    env: { ...process.env, GH_TOKEN: token },
    maxBuffer: 16 * 1024 * 1024,
  });
  if (!source.trim()) throw new Error(`generator fetched empty from ref ${ref}`);
  writeFileSync(path, source);
  return path;
}

// ADOPTION_MAP_CMD overrides; otherwise fetch the generator and run it in fork-range mode.
export function planMap(env, ctx, fetch = fetchGenerator) {
  if (env.ADOPTION_MAP_CMD) return runMapCommand(env.ADOPTION_MAP_CMD, ctx, { ...process.env, ...env });
  let generator = env.ADOPTION_GENERATOR;
  if (!generator) {
    if (!env.ADOPTION_GENERATOR_TOKEN) throw new Error("ADOPTION_GENERATOR_TOKEN is not configured; the adoption map generator cannot be fetched");
    generator = fetch(env.ADOPTION_GENERATOR_REF || DEFAULT_GENERATOR_REF, env.ADOPTION_GENERATOR_TOKEN);
  }
  return runMapCommand(FORK_RANGE_COMMAND, ctx, { ...process.env, ...env, ADOPTION_GENERATOR: generator });
}

// Classify first; the generator is fetched and run only when the range has an agent-facing PR.
export function planRange({ prev, next, prs, env, fetchGenerator: fetch }) {
  if (!buildAdoption({ prev, next, prs }).issue) return buildAdoption({ prev, next, prs });
  return buildAdoption({ prev, next, prs, map: planMap(env, { prev, next, repo: env.ADOPTION_REPO ?? "" }, fetch) });
}

// Identifier of the issue whose body carries this deployed SHA's marker; the 12-character title is only a search key.
export function findExistingIssue(searchJson, next) {
  const { issues } = JSON.parse(searchJson);
  if (!Array.isArray(issues)) throw new Error("issue search output has no issues array");
  return issues.find((issue) => issue.description?.includes(`adoption-key: sha:${next}`))?.identifier ?? null;
}

const git = (...args) => execFileSync("git", args, { encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });

function collectPrs(prev, next, repo) {
  const prs = [];
  for (const row of git("log", "--first-parent", "--format=%H%x09%s", `${prev}..${next}`).split("\n").filter(Boolean)) {
    const [sha, subject] = row.split("\t");
    const number = /\(#(\d+)\)$|^Merge pull request #(\d+)/.exec(subject);
    if (!number) continue;
    const n = number[1] ?? number[2];
    const pr = JSON.parse(execFileSync("gh", ["api", `repos/${repo}/pulls/${n}`, "--jq", "{title,body}"], { encoding: "utf8" }));
    prs.push({
      number: Number(n),
      title: pr.title,
      body: pr.body ?? "",
      paths: git("diff", "--name-only", `${sha}^1`, sha).split("\n").filter(Boolean),
      diff: git("diff", "-U0", `${sha}^1`, sha, "--", "server/cmd/multica"),
    });
  }
  return prs;
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
    const paths = git("diff", "--name-only", `${pr.base.sha}...${pr.head.sha}`).split("\n").filter(Boolean);
    const errors = checkAgentFacing(pr.body ?? "", paths);
    for (const error of errors) console.log(`::error::${error}`);
    if (errors.length === 0) console.log("Agent-facing field: ok");
    return errors.length === 0 ? 0 : 1;
  }
  if (mode === "plan") {
    const prev = arg(argv, "prev");
    const next = arg(argv, "next");
    const repo = arg(argv, "repo");
    const out = planRange({ prev, next, prs: collectPrs(prev, next, repo), env: { ...env, ADOPTION_REPO: repo } });
    console.log(out.log);
    console.log(`issue=${out.issue}`);
    if (out.issue) {
      const dir = arg(argv, "out");
      mkdirSync(dir, { recursive: true });
      writeFileSync(`${dir}/title.txt`, out.title);
      writeFileSync(`${dir}/body.md`, out.body);
    }
    return 0;
  }
  if (mode === "reuse") {
    try {
      const existing = findExistingIssue(readFileSync(argv.includes("--file") ? arg(argv, "file") : 0, "utf8"), arg(argv, "next"));
      if (existing) console.log(existing);
      return existing ? 0 : 1;
    } catch (error) {
      console.error(`adoption issue lookup unusable: ${error.message}`);
      return 2;
    }
  }
  console.error("usage: agent-adoption.mjs check --event <file> | plan --prev <sha> --next <sha> --repo <owner/repo> --out <dir> | reuse --next <sha> [--file <search.json>]");
  return 2;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = main(process.argv.slice(2), process.env);
}
