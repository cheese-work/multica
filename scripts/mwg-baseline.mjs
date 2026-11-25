#!/usr/bin/env node
// CHE-9348 (S3a of CHE-7574): baseline measurement for the Modern Web Guidance pilot readout.
//
// Read-only. It counts web PRs, their cross-lab review state, web findings, and transcript signals for the
// pilot agents over a window, and reports each metric in one of four states:
//   measured     counted from complete sources (n may be 0 or low)
//   insufficient measured, but below the data rules in the CHE-7574 plan (units, transcripts, reviews)
//   unavailable  a source it needs failed (error, auth failure, missing path, incomplete page set); no count is printed
//   not_run      no source is wired in (M2f needs sloplint data from CHE-6854)
// A source failure never becomes a count of 0. The exit code is 2 when any source failed.
//
// Live use:   node scripts/mwg-baseline.mjs --agent x99-claude-sonnet-200k --agent x99-claude-sonnet-design
// Fixtures:   node scripts/mwg-baseline.mjs --fixture-dir scripts/fixtures/mwg-baseline ...
// Env:        GH_BIN (default gh), MULTICA_CLI_PATH (default multica), GH_TOKEN, MULTICA_TOKEN are passed to the children.

import { execFile } from "node:child_process";
import { readFileSync, statSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { basename, join } from "node:path";
import { pathToFileURL } from "node:url";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

export class SourceError extends Error {
  constructor(source, message) {
    super(message);
    this.name = "SourceError";
    this.source = source;
  }
}

// Every external command the script may run. liveSources refuses anything else (S3a AC 7).
export const READ_ONLY_COMMANDS = [
  ["gh", "pr", "list"],
  ["gh", "pr", "view"],
  ["gh", "api"],
  ["multica", "agent", "list"],
  ["multica", "agent", "tasks"],
  ["multica", "issue", "get"],
  ["multica", "issue", "comment", "list"],
  ["multica", "issue", "run-messages"],
  ["multica", "issue", "runs"],
];

const DAY = 86_400_000;
const REASON_M2F = "no sloplint source (CHE-6854) is wired into this script";
const MIN_UNITS = 10;
const MAX_MISSING_SHARE = 0.2;
const FILES_CAP = 100; // gh pr list returns at most this many files per PR

// ---------------------------------------------------------------------------------------------------------------------
// Parsing

const FINDING_HEADER = /^\s*(?:(?:\d+[.)]|[-*•])\s+|#{1,6}\s+)\**\s*(P[0-3])\b/;
const CLASS_LINE = /^\s*(?:[-*]\s+)?Class:\s*(.+?)\s*$/i;
const RULE_LINE = /^\s*(?:[-*]\s+)?Rule:\s*(.+?)\s*$/i;
const WEB_RULE = /\b(layout|css|stylesheet|browser[- ]support|(?:outdated|deprecated|legacy)\s+(?:web\s+)?(?:api|browser))/i;
const GUIDANCE_TEXT = /modern[- ]web[- ]guidance|\bmwg\b|\bguidance\b|\bthe guide\b/i;
const FILE_REF = /`([^`\s]+\.[A-Za-z0-9]+)(?::\d+)?`/g;

function finishFinding(f) {
  const text = f.lines.join("\n");
  const files = [...(f.lines[0] ?? "").matchAll(FILE_REF)].map((m) => m[1]);
  return {
    severity: f.severity,
    class: f.class,
    rule: f.rule,
    web: f.class === "a11y" || WEB_RULE.test(f.rule ?? ""),
    files,
    text,
  };
}

// Findings are blocks that start at a P0..P3 header or at a `Class:` / `Rule:` line.
export function parseFindings(text) {
  const out = [];
  let cur = null;
  const flush = () => {
    if (cur) out.push(finishFinding(cur));
    cur = null;
  };
  for (const line of String(text ?? "").split(/\r?\n/)) {
    const header = FINDING_HEADER.exec(line);
    if (header) {
      flush();
      cur = { severity: header[1], lines: [line], class: null, rule: null };
      continue;
    }
    const cls = CLASS_LINE.exec(line);
    if (cls) {
      if (cur && cur.class !== null) flush();
      cur ??= { severity: null, lines: [], class: null, rule: null };
      cur.class = cls[1].toLowerCase();
      cur.lines.push(line);
      continue;
    }
    const rule = RULE_LINE.exec(line);
    if (rule) {
      if (cur && cur.rule !== null) flush();
      cur ??= { severity: null, lines: [], class: null, rule: null };
      cur.rule = rule[1];
      cur.lines.push(line);
      continue;
    }
    if (cur) cur.lines.push(line);
  }
  flush();
  return out;
}

const lab = (model) => {
  const m = String(model ?? "").toLowerCase();
  if (/claude|sonnet|opus|haiku|fable/.test(m)) return "anthropic";
  if (/gpt|codex|^o\d|openai/.test(m)) return "openai";
  return "unknown";
};

// A signed verdict says PASS, CHANGES, FAIL, APPROVED or REJECTED on its first line. NOT-RUN, "not reviewed" and
// "not signed" lines say no review happened, so they do not make a PR reviewed.
const isVerdict = (content) => {
  const first = String(content ?? "").split(/\r?\n/).find((l) => l.trim() !== "") ?? "";
  if (!/^\s*(?:\*\*)?Verdict(?:\*\*)?\s*:/i.test(first)) return false;
  if (/\bNOT[- ]RUN\b|\bnot (?:reviewed|signed)\b/i.test(first)) return false;
  return /\b(?:PASS|CHANGES|FAIL|APPROVED?|REJECT\w*)\b/i.test(first);
};

// The header is the verdict's first block: its lines up to the first blank line. Later text (findings, context) never
// names the review target and never signs.
const headerOf = (content) => {
  const lines = String(content ?? "").split(/\r?\n/);
  const start = lines.findIndex((l) => l.trim() !== "");
  if (start < 0) return "";
  const out = [];
  for (const l of lines.slice(start)) {
    if (l.trim() === "") break;
    out.push(l);
  }
  return out.join("\n");
};

// A signed verdict carries a "Reviewer:" field with a name in its header.
export function isSignedVerdict(content) {
  return /(?:^|[\s*_-])Reviewer(?:\*\*)?\s*:(?:\*\*)?\s*[\w@]/i.test(headerOf(content));
}

// A verdict signs one PR, named in its header: by one labeled head SHA (Head SHA, Reviewed head or Candidate; 7 or more hex digits) or, with no head SHA, by
// exactly one PR number. A head SHA decides over a PR number. Two different heads or two different PRs are ambiguous
// and sign nothing.
export function namesPr(content, unit) {
  const header = headerOf(content);
  const heads = new Set(
    [...header.matchAll(/\b(?:head(?:\s+sha)?|candidate)\s*[:=]?\s*`?([0-9a-f]{7,40})\b/gi)].map((m) => m[1].toLowerCase()),
  );
  if (heads.size > 0) {
    const oid = String(unit.headOid ?? "");
    return heads.size === 1 && oid !== "" && oid.startsWith([...heads][0]);
  }
  const prs = new Set([...header.matchAll(/(?:\bPR\s*#?|\/pull\/|(?<![\w/])#)(\d+)\b/gi)].map((m) => m[1]));
  return prs.size === 1 && prs.has(String(unit.number));
}

function guidanceSection(body) {
  const lines = String(body ?? "").split(/\r?\n/);
  const at = lines.findIndex((l) => /^#{2,4}\s*Modern Web Guidance\s*$/i.test(l.trim()));
  if (at < 0) return null;
  const out = [];
  for (const l of lines.slice(at + 1)) {
    if (/^#{1,4}\s/.test(l)) break;
    out.push(l);
  }
  return out.join("\n").trim();
}

const SECTION_EMPTY = /^(none|n\/a|na|no|not used|not applicable|nothing|-|—)\.?$/i;

// ---------------------------------------------------------------------------------------------------------------------
// Transcript signals

// A Bash command is cut into statements at unquoted ; & | ( ) ` and newlines. Quoted text, heredoc bodies and comments
// never start a statement, so a path or flag spelled inside a string, a grep pattern or a written file is not a call.
// A heredoc fed to sh or bash is read as a script; `sh -c "..."` is read as a script too.
const SHELLS = /^(?:sh|bash|zsh)$/;
function statementsOf(cmd, depth = 0) {
  const out = [];
  let cur = "";
  let quote = null;
  let tag = null;
  const flush = () => {
    if (cur.trim()) out.push(cur.trim());
    cur = "";
  };
  for (let i = 0; i < cmd.length; i++) {
    const ch = cmd[i];
    if (quote) {
      cur += ch;
      if (ch === "\\" && quote === '"') cur += cmd[++i] ?? "";
      else if (ch === quote) quote = null;
      continue;
    }
    if (ch === "'" || ch === '"') {
      quote = ch;
      cur += ch;
    } else if (ch === "\\") {
      cur += ch + (cmd[++i] ?? "");
    } else if (ch === "#" && /\s|^$/.test(cmd[i - 1] ?? " ")) {
      while (i < cmd.length && cmd[i] !== "\n") i++;
      i--;
    } else if (ch === "<" && cmd[i + 1] === "<" && cmd[i + 2] !== "<") {
      const m = /^<<-?\s*(['"]?)([A-Za-z_]\w*)\1/.exec(cmd.slice(i));
      if (m) {
        tag = m[2];
        cur += m[0];
        i += m[0].length - 1;
      } else cur += ch;
    } else if (ch === "\n") {
      const first = cur.trim().split(/\s+/)[0] ?? "";
      flush();
      if (tag) {
        const lines = [];
        for (i++; i < cmd.length; i++) {
          const end = cmd.indexOf("\n", i);
          const line = cmd.slice(i, end < 0 ? cmd.length : end);
          i = end < 0 ? cmd.length : end;
          if (line.trim() === tag) break;
          lines.push(line);
        }
        if (SHELLS.test(first) && depth < 3) out.push(...statementsOf(lines.join("\n"), depth + 1));
        tag = null;
      }
    } else if (";&|()`".includes(ch)) {
      flush();
    } else cur += ch;
  }
  flush();
  const nested = [];
  for (const st of out) {
    const m = SHELL_C.exec(st);
    if (m && depth < 3) nested.push(...statementsOf(m[2], depth + 1));
  }
  return [...out, ...nested];
}

const VALUE = String.raw`(?:"[^"]*"|'[^']*'|[^\s"'])*`;
const ENV = String.raw`(?:\w+=${VALUE}\s+)*`;
const SHELL_C = new RegExp(String.raw`^${ENV}(?:sh|bash|zsh)\s+(?:-\S+\s+)*-\w*c\s+(["'])([\s\S]*)\1\s*$`);
const WRAP = String.raw`(?:(?:exec|time|nohup|sudo|command|env)\s+(?:\w+=${VALUE}\s+)*|timeout\s+\S+\s+)*`;
const RUNNER = String.raw`(?:(?:npx|pnpx|bunx|npm\s+exec|pnpm\s+(?:dlx|exec)|yarn\s+dlx|sh|bash|zsh|node)\s+(?:-{1,2}\S+\s+)*)?`;
// The package runner name, or its entrypoint file at any path (the package's bin is skills/modern-web-guidance/modern-web.mjs).
const UNSANCTIONED = new RegExp(String.raw`^${ENV}${WRAP}${RUNNER}(?:\S*/)?(?:modern-web-guidance(?:@\S+)?|modern-web\.mjs)(?=\s|$|["'])`);
const MWG_EXEC = String.raw`^${ENV}${WRAP}(?:(?:sh|bash|zsh)\s+(?:-{1,2}\S+\s+)*)?["']?\S*scripts/mwg\.sh["']?\s+`;
const MWG_USE = new RegExp(`${MWG_EXEC}(?:search|retrieve)\\b`);
const MWG_CALL = new RegExp(`${MWG_EXEC}\\S`);

export function classifyCall(call, mwgEnd = Infinity) {
  const kinds = [];
  if (call?.name !== "Bash" || typeof call.input?.command !== "string") return { use: false, kinds };
  const statements = statementsOf(call.input.command);
  if (statements.some((st) => UNSANCTIONED.test(st))) kinds.push("unsanctioned");
  if (call.at && Date.parse(call.at) > mwgEnd && statements.some((st) => MWG_CALL.test(st))) kinds.push("late");
  return { use: statements.some((st) => MWG_USE.test(st)), kinds };
}

const mangle = (dir) => String(dir).replace(/[^A-Za-z0-9]/g, "-");

async function readTranscript(root, run) {
  const sessionId = run.result?.session_id;
  const dir = run.result?.work_dir;
  if (!sessionId || !dir) return null;
  const file = join(root, mangle(dir), `${sessionId}.jsonl`);
  let raw;
  try {
    raw = await readFile(file, "utf8");
  } catch (e) {
    if (e?.code === "ENOENT" || e?.code === "ENOTDIR") return null;
    throw new SourceError("transcripts", `cannot read transcript ${file}: ${e?.code ?? e?.message}`);
  }
  const calls = [];
  for (const line of raw.split("\n")) {
    if (!line.includes('"tool_use"')) continue;
    let rec;
    try {
      rec = JSON.parse(line);
    } catch {
      continue;
    }
    const content = rec?.message?.content;
    if (!Array.isArray(content)) continue;
    for (const part of content) {
      if (part?.type === "tool_use") calls.push({ name: part.name, input: part.input ?? {}, at: rec.timestamp ?? null });
    }
  }
  return calls;
}

// ---------------------------------------------------------------------------------------------------------------------
// Sources

const iso = (d) => d.toISOString().replace(/\.\d{3}Z$/, "Z");

function parseJson(stdout) {
  const text = String(stdout ?? "");
  try {
    return JSON.parse(text);
  } catch {
    const at = text.search(/^[[{]/m);
    if (at < 0) throw new Error("no JSON in output");
    return JSON.parse(text.slice(at));
  }
}

function defaultExec(env) {
  return async (cmd, args) => {
    const { stdout, stderr } = await execFileAsync(cmd, args, {
      env: { ...process.env, ...env },
      maxBuffer: 512 * 1024 * 1024,
    });
    return { stdout, stderr };
  };
}

// Server errors, rate limits and timeouts are retried (twice); auth failures and 4xx answers are not.
const TRANSIENT = /\b(?:50[0234]|429)\b|timed? ?out|ETIMEDOUT|ECONNRESET|EAI_AGAIN|temporar|rate limit/i;

export function liveSources({ exec, env = {}, retryDelayMs = 3000 } = {}) {
  const run = exec ?? defaultExec(env);
  const bins = { gh: env.GH_BIN || "gh", multica: env.MULTICA_CLI_PATH || "multica" };
  const call = async (source, prog, args) => {
    const allowed = READ_ONLY_COMMANDS.some((c) => c[0] === prog && c.slice(1).every((w, i) => args[i] === w));
    if (!allowed) throw new Error(`refusing a command outside READ_ONLY_COMMANDS: ${prog} ${args.join(" ")}`);
    let last;
    for (let attempt = 1; attempt <= 3; attempt++) {
      try {
        const r = await run(bins[prog], args);
        return typeof r === "string" ? { stdout: r, stderr: "" } : { stdout: r?.stdout ?? "", stderr: r?.stderr ?? "" };
      } catch (e) {
        last = e;
        const text = `${e?.stderr ?? ""} ${e?.message ?? ""}`;
        if (!TRANSIENT.test(text) || attempt === 3) break;
        await new Promise((resolve) => setTimeout(resolve, retryDelayMs * attempt));
      }
    }
    const detail = String(last?.stderr || last?.message || last).replace(/\s+/g, " ").trim() || "failed";
    const err = new SourceError(source, `${prog} ${args.slice(0, 3).join(" ")} failed: ${detail.slice(0, 240)}`);
    err.stderr = String(last?.stderr ?? "");
    throw err;
  };
  const json = async (source, prog, args) => {
    const r = await call(source, prog, args);
    try {
      return { data: parseJson(r.stdout), stderr: r.stderr };
    } catch (e) {
      throw new SourceError(source, `${prog} ${args.slice(0, 3).join(" ")}: unreadable output (${e.message})`);
    }
  };
  return {
    async agents() {
      return (await json("multica", "multica", ["agent", "list", "--include-archived", "--output", "json"])).data;
    },
    // PRs created in [start, end). The search takes whole seconds and is inclusive on both ends.
    async prList(repo, start, end, limit) {
      const range = `created:${iso(start)}..${iso(new Date(end.getTime() - 1000))}`;
      const args = ["pr", "list", "--repo", repo, "--state", "all", "--search", range, "--json", "number,headRefName,headRefOid,createdAt,files", "--limit", String(limit)];
      return (await json("github", "gh", args)).data;
    },
    async prBody(repo, number) {
      const args = ["pr", "view", String(number), "--repo", repo, "--json", "body"];
      return (await json("github", "gh", args)).data.body ?? "";
    },
    async prFiles(repo, number) {
      const args = ["api", `repos/${repo}/pulls/${number}/files`, "--paginate", "--jq", ".[].filename"];
      const r = await call("github", "gh", args);
      return r.stdout.split("\n").filter(Boolean).map((path) => ({ path }));
    },
    async issue(identifier) {
      try {
        return (await json("multica", "multica", ["issue", "get", identifier, "--output", "json"])).data;
      } catch (e) {
        if (/not found|404/i.test(`${e.stderr} ${e.message}`)) return null;
        throw e;
      }
    },
    async comments(identifier) {
      return (await json("multica", "multica", ["issue", "comment", "list", identifier, "--full", "--output", "json"])).data;
    },
    async tasks(agentId, before) {
      const args = ["agent", "tasks", agentId, "--limit", "200", "--output", "json", ...(before ? ["--before", before] : [])];
      const { data, stderr } = await json("multica", "multica", args);
      const next = /--before "([^"]+)"/.exec(stderr)?.[1] ?? null;
      return { items: data, next };
    },
    async runMessages(runId) {
      return (await json("multica", "multica", ["issue", "run-messages", runId, "--output", "json"])).data;
    },
    // Every run of one issue, from every agent, with no date limit.
    async issueRuns(issueId) {
      return (await json("multica", "multica", ["issue", "runs", issueId, "--output", "json"])).data;
    },
  };
}

export function fixtureSources(dir, opts = {}) {
  const load = (name) => JSON.parse(readFileSync(join(dir, name), "utf8"));
  const pageSize = opts.taskPageSize ?? 200;
  const cap = opts.prLimit ?? Infinity;
  return {
    async agents() {
      return load("agents.json");
    },
    async prList(repo, start, end, limit) {
      const all = load("gh-prs.json")[repo] ?? [];
      return all
        .filter((p) => Date.parse(p.createdAt) >= start.getTime() && Date.parse(p.createdAt) < end.getTime())
        .sort((a, b) => a.createdAt.localeCompare(b.createdAt))
        .slice(0, Math.min(limit, cap));
    },
    async prBody(repo, number) {
      const pr = (load("gh-prs.json")[repo] ?? []).find((p) => p.number === number);
      if (!pr) throw new SourceError("github", `fixture: no PR ${repo}#${number}`);
      return pr.body ?? "";
    },
    async prFiles(repo, number) {
      const pr = (load("gh-prs.json")[repo] ?? []).find((p) => p.number === number);
      if (!pr) throw new SourceError("github", `fixture: no PR ${repo}#${number}`);
      return pr.files;
    },
    async issue(identifier) {
      return load("issues.json")[identifier] ?? null;
    },
    async comments(identifier) {
      return load("comments.json")[identifier] ?? [];
    },
    async tasks(agentId, before) {
      const key = (t) => `${t.created_at}|${t.id}`;
      const sorted = [...(load("tasks.json")[agentId] ?? [])].sort((a, b) => key(b).localeCompare(key(a)));
      const rest = before ? sorted.filter((t) => key(t) < before) : sorted;
      const items = rest.slice(0, pageSize);
      return { items, next: rest.length > pageSize ? key(items.at(-1)) : null };
    },
    async runMessages(runId) {
      return load("run-messages.json")[runId] ?? [];
    },
    async issueRuns(issueId) {
      return Object.values(load("tasks.json")).flat().filter((t) => t.issue_id === issueId);
    },
  };
}

// ---------------------------------------------------------------------------------------------------------------------
// Measurement

// Runs fn over items with at most `limit` in flight; stops starting new work after the first rejection.
async function pool(items, limit, fn) {
  let next = 0;
  let error = null;
  const worker = async () => {
    while (error === null && next < items.length) {
      const item = items[next++];
      try {
        await fn(item);
      } catch (e) {
        error ??= e;
      }
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
  if (error !== null) throw error;
}

const CONCURRENCY = 6;
const round4 = (x) => Math.round(x * 10_000) / 10_000;
const ratio = (a, b) => (b === 0 ? null : round4(a / b));
const median = (xs) => {
  if (!xs.length) return null;
  const s = [...xs].sort((a, b) => a - b);
  const mid = s.length >> 1;
  return s.length % 2 ? s[mid] : (s[mid - 1] + s[mid]) / 2;
};
const mean = (xs) => (xs.length ? round4(xs.reduce((a, b) => a + b, 0) / xs.length) : null);

const isWebFile = (repo, path) => {
  if (repo.endsWith("/doop")) return /\.(html|css|tsx|jsx)$/i.test(path);
  return path.startsWith("apps/web/") || path.startsWith("packages/views/");
};

// A capability is one thing a metric can need. Each belongs to a source and fails on its own.
const CAP_SOURCE = { github: "github", agents: "multica", issues: "multica", tasks: "multica", confirm: "multica", transcripts: "transcripts" };
const NEEDS = {
  units: ["agents", "github", "issues"],
  M1: ["agents", "github", "issues", "tasks", "transcripts"],
  M1s: ["agents", "github", "issues"],
  M2: ["agents", "github", "issues"],
  M3: ["agents", "github", "issues"],
  M3p: ["agents", "tasks", "transcripts", "confirm"],
  M4: ["agents", "github", "issues", "tasks", "transcripts"],
};

export async function measure(opts) {
  const { agents: names, start, end, reviewCutoff, mwgEnd, transcriptRoot, repos, prLimit = 500 } = opts;
  const t0 = Date.now();
  const calls = { github: 0, multica: 0 };
  const count = (kind, fn) => (...a) => {
    calls[kind] += 1;
    return fn(...a);
  };
  const raw = opts.sources;
  const src = {
    agents: count("multica", raw.agents),
    prList: count("github", raw.prList),
    prFiles: count("github", raw.prFiles),
    prBody: count("github", raw.prBody),
    issue: count("multica", raw.issue),
    comments: count("multica", raw.comments),
    tasks: count("multica", raw.tasks),
    runMessages: count("multica", raw.runMessages),
    issueRuns: count("multica", raw.issueRuns),
  };
  const failed = {}; // capability -> error message
  const fail = (cap, e) => {
    failed[cap] ??= e?.message ?? String(e);
  };
  const mwgEndMs = mwgEnd.getTime();

  // Agents
  const agentById = new Map();
  let agentList = [];
  try {
    agentList = await src.agents();
    for (const a of agentList) agentById.set(a.id, a);
  } catch (e) {
    fail("agents", e);
  }
  const me = new Map();
  if (!failed.agents) {
    for (const name of names) {
      const a = agentList.find((x) => x.name === name);
      if (a) me.set(name, a);
      else fail("agents", new SourceError("multica", `agent not found: ${name}`));
    }
  }

  // Transcript root
  try {
    if (!statSync(transcriptRoot).isDirectory()) throw new Error("not a directory");
  } catch (e) {
    fail("transcripts", new SourceError("transcripts", `transcript root is not readable: ${transcriptRoot} (${e.code ?? e.message})`));
  }

  // PRs: slice the window until no search page is full, so a truncated list is never mistaken for a complete one.
  const prPool = new Map();
  const fetchSlice = async (repo, s, e) => {
    const page = await src.prList(repo, s, e, prLimit);
    if (page.length < prLimit) {
      for (const p of page) prPool.set(`${repo}#${p.number}`, { ...p, repo });
      return;
    }
    const span = e.getTime() - s.getTime();
    if (span <= 1000) throw new SourceError("github", `PR list incomplete: ${repo} still hit the limit of ${prLimit} on a one-second slice`);
    const mid = new Date(s.getTime() + Math.floor(span / 2000) * 1000);
    await fetchSlice(repo, s, mid);
    await fetchSlice(repo, mid, e);
  };
  if (!failed.agents) {
    try {
      for (const repo of repos) await fetchSlice(repo, start, end);
      for (const pr of prPool.values()) {
        if ((pr.files ?? []).length >= FILES_CAP) pr.files = await src.prFiles(pr.repo, pr.number);
      }
    } catch (e) {
      fail("github", e);
    }
  }

  const cache = { issue: new Map(), comments: new Map(), transcript: new Map(), messages: new Map() };
  const issueFor = async (identifier) => {
    if (!cache.issue.has(identifier)) cache.issue.set(identifier, await src.issue(identifier));
    return cache.issue.get(identifier);
  };
  const commentsFor = async (identifier) => {
    if (!cache.comments.has(identifier)) cache.comments.set(identifier, await src.comments(identifier));
    return cache.comments.get(identifier);
  };

  // Per-agent data
  const data = new Map();
  for (const name of names) {
    const agent = me.get(name);
    const d = { units: [], runs: [] };
    data.set(name, d);
    if (!agent || failed.agents) continue;
    if (!failed.github) await collectUnits(name, agent, d);
    await collectRuns(agent, d);
  }
  async function collectUnits(name, agent, d) {
    const mine = [...prPool.values()].filter((p) => {
      const at = Date.parse(p.createdAt);
      return at >= start.getTime() && at < end.getTime() && String(p.headRefName).startsWith(`agent/${name}/`) && (p.files ?? []).some((f) => isWebFile(p.repo, f.path));
    });
    mine.sort((a, b) => a.repo.localeCompare(b.repo) || a.number - b.number);
    const bodies = new Map();
    try {
      await pool(mine, CONCURRENCY, async (pr) => bodies.set(pr, await src.prBody(pr.repo, pr.number)));
    } catch (e) {
      fail("github", e);
      return;
    }
    const units = mine.map((pr) => ({ repo: pr.repo, number: pr.number, headRefName: pr.headRefName, headOid: String(pr.headRefOid ?? "").toLowerCase(), issue: null, issueId: null, reviewed: false, findings: [], section: guidanceSection(bodies.get(pr)), runs: [] }));
    d.units.push(...units);
    const enrich = async (unit) => {
      const ident = /^agent\/[^/]+\/([a-z]+-\d+)\b/i.exec(unit.headRefName)?.[1]?.toUpperCase();
      if (!ident || failed.issues) return;
      try {
        const issue = await issueFor(ident);
        if (!issue) return;
        unit.issue = ident;
        unit.issueId = issue.id;
        const verdicts = (await commentsFor(ident)).filter((c) => {
          if (c.author_type !== "agent" || !isVerdict(c.content) || !isSignedVerdict(c.content) || !namesPr(c.content, unit)) return false;
          if (Date.parse(c.created_at) >= reviewCutoff.getTime()) return false;
          const reviewer = agentById.get(c.author_id);
          return reviewer && lab(reviewer.model) !== "unknown" && lab(reviewer.model) !== lab(agent.model);
        });
        unit.reviewed = verdicts.length > 0;
        unit.findings = verdicts.flatMap((c) => parseFindings(c.content));
      } catch (e) {
        fail("issues", e);
      }
    };
    await pool(units, CONCURRENCY, enrich);
  }
  async function collectRuns(agent, d) {
    if (!failed.tasks) {
      try {
        const tasks = [];
        let before = null;
        for (let page = 0; ; page++) {
          if (page >= 500) throw new SourceError("multica", "run list incomplete: more than 500 pages");
          const { items, next } = await src.tasks(agent.id, before);
          tasks.push(...items);
          const oldest = items.reduce((m, t) => Math.min(m, Date.parse(t.created_at)), Infinity);
          if (!next || oldest < start.getTime()) break;
          before = next;
        }
        const started = tasks.filter((t) => t.started_at && Date.parse(t.created_at) < end.getTime());
        d.runs = started.filter((t) => Date.parse(t.created_at) >= start.getTime());
      } catch (e) {
        fail("tasks", e);
        return;
      }
      // Every run of the unit's issue by this agent, however old, so transcript coverage is judged on all of them.
      try {
        await pool(d.units.filter((u) => u.issueId), CONCURRENCY, async (unit) => {
          unit.runs = (await src.issueRuns(unit.issueId)).filter((t) => t.agent_id === agent.id && t.started_at && Date.parse(t.created_at) < end.getTime());
        });
      } catch (e) {
        fail("tasks", e);
      }
    }
  }

  // Transcripts
  const transcriptOf = async (run) => {
    if (!cache.transcript.has(run.id)) cache.transcript.set(run.id, await readTranscript(transcriptRoot, run));
    return cache.transcript.get(run.id);
  };
  if (!failed.transcripts && !failed.tasks && !failed.agents) {
    try {
      for (const d of data.values()) {
        for (const run of new Set([...d.runs, ...d.units.flatMap((u) => u.runs)])) await transcriptOf(run);
      }
    } catch (e) {
      fail("transcripts", e);
    }
  }
  const have = (run) => cache.transcript.get(run.id) ?? null;

  // M3p hits and their confirmation
  const m3p = new Map();
  if (!failed.transcripts && !failed.tasks && !failed.agents) {
    for (const [name, d] of data) {
      const hits = [];
      for (const run of d.runs) {
        for (const c of have(run) ?? []) {
          for (const kind of classifyCall(c, mwgEndMs).kinds) hits.push({ run, kind, at: c.at, command: c.input.command });
        }
      }
      try {
        for (const h of hits) {
          if (!cache.messages.has(h.run.id)) cache.messages.set(h.run.id, await src.runMessages(h.run.id));
          h.confirmed = cache.messages.get(h.run.id).some((m) => classifyCall({ name: m.tool ?? m.name, input: m.input, at: m.created_at }, mwgEndMs).kinds.includes(h.kind));
        }
      } catch (e) {
        fail("confirm", e);
      }
      m3p.set(name, hits);
    }
  }

  // Nodes
  const unavailable = (needs) => {
    const sources = [...new Set(needs.map((c) => CAP_SOURCE[c]))];
    const down = [...new Set(needs.filter((c) => failed[c]).map((c) => CAP_SOURCE[c]))];
    return down.length ? { state: "unavailable", needs: sources, failed: down } : null;
  };
  const guard = (metric, build) => unavailable(NEEDS[metric]) ?? build();

  const result = {};
  for (const name of names) {
    const d = data.get(name);
    const web = d.units.length;
    const reviewed = d.units.filter((u) => u.reviewed);
    const transcriptState = (u) => {
      if (failed.tasks || failed.transcripts) return "unavailable";
      const ok = u.runs.filter((r) => have(r)).length;
      return u.runs.length > 0 && ok === u.runs.length ? "full" : ok > 0 ? "partial" : "missing";
    };
    const full = d.units.filter((u) => transcriptState(u) === "full");
    const missingShare = (pred) => (web === 0 ? 1 : d.units.filter(pred).length / web);
    const qualityState = (n) => (n < MIN_UNITS || missingShare((u) => !u.reviewed) > MAX_MISSING_SHARE ? "insufficient" : "measured");

    const node = {
      units: guard("units", () => ({
        state: "measured",
        web,
        reviewed: reviewed.length,
        unreviewed: web - reviewed.length,
        unlinked: d.units.filter((u) => !u.issue).length,
        list: d.units.map((u) => ({
          repo: u.repo,
          number: u.number,
          issue: u.issue,
          reviewed: u.reviewed,
          m2: u.reviewed ? u.findings.filter((f) => f.web).length : null,
          transcript: transcriptState(u),
        })),
      })),
    };
    const metrics = {
      M1: guard("M1", () => {
        const used = (u) => u.runs.some((r) => (have(r) ?? []).some((c) => classifyCall(c).use));
        const hits = full.filter(used).length;
        const state = full.length < MIN_UNITS || missingShare((u) => transcriptState(u) !== "full" || !u.reviewed) > MAX_MISSING_SHARE ? "insufficient" : "measured";
        return { state, n: full.length, hits, value: ratio(hits, full.length) };
      }),
      M1s: guard("M1s", () => {
        const hits = d.units.filter((u) => u.section !== null && u.section !== "" && !SECTION_EMPTY.test(u.section)).length;
        return { state: "measured", n: web, hits, value: ratio(hits, web) };
      }),
      M2: guard("M2", () => {
        const findings = reviewed.reduce((n, u) => n + u.findings.filter((f) => f.web).length, 0);
        return { state: qualityState(reviewed.length), n: reviewed.length, findings, perReviewedPr: ratio(findings, reviewed.length) };
      }),
      M2f: { state: "not_run", reason: REASON_M2F },
      M3: guard("M3", () => {
        const harmed = (u) => u.findings.some((f) => (f.severity === "P0" || f.severity === "P1") && (GUIDANCE_TEXT.test(f.text) || f.files.some((p) => u.section && (u.section.includes(p) || u.section.includes(basename(p))))));
        return { state: qualityState(reviewed.length), n: reviewed.length, unitsWithHarm: reviewed.filter(harmed).length };
      }),
      M3p: guard("M3p", () => {
        const hits = m3p.get(name) ?? [];
        const withTranscript = d.runs.filter((r) => have(r)).length;
        const state = d.runs.length > 0 && 1 - withTranscript / d.runs.length > MAX_MISSING_SHARE ? "insufficient" : "measured";
        return {
          state,
          runs: d.runs.length,
          runsWithTranscript: withTranscript,
          coverage: ratio(withTranscript, d.runs.length),
          hits: hits.length,
          unconfirmed: hits.filter((h) => !h.confirmed).length,
          hitList: hits
            .map((h) => ({ run: h.run.id, kind: h.kind, at: h.at, confirmed: h.confirmed }))
            .sort((a, b) => String(a.at).localeCompare(String(b.at)) || a.run.localeCompare(b.run)),
        };
      }),
      M4: guard("M4", () => {
        const bytes = d.runs.map((r) => r.result?.init_context?.parts?.find((p) => p.part === "skills")?.bytes).filter((x) => typeof x === "number");
        const secs = d.runs.filter((r) => r.completed_at).map((r) => (Date.parse(r.completed_at) - Date.parse(r.started_at)) / 1000);
        const perUnit = full.map((u) => u.runs.reduce((n, r) => n + (have(r) ?? []).length, 0));
        return {
          state: "measured",
          runs: d.runs.length,
          listingBytesMedian: median(bytes),
          runDurationSecMedian: median(secs),
          units: full.length,
          toolCallsPerUnitMean: mean(perUnit),
          toolCallsPerUnitMedian: median(perUnit),
        };
      }),
    };
    result[name] = { ...node, metrics };
  }
  const agents = {};
  for (const [name, r] of Object.entries(result)) agents[name] = { units: r.units, metrics: r.metrics };

  const sources = {};
  for (const s of ["github", "multica", "transcripts"]) {
    const errors = Object.entries(failed).filter(([c]) => CAP_SOURCE[c] === s).map(([, m]) => m);
    sources[s] = errors.length ? { ok: false, error: errors[0] } : { ok: true };
  }
  return {
    window: { start: start.toISOString(), end: end.toISOString(), reviewCutoff: reviewCutoff.toISOString(), mwgEnd: mwgEnd.toISOString() },
    agents,
    sources,
    apiCalls: { ...calls, total: calls.github + calls.multica },
    wallSeconds: round4((Date.now() - t0) / 1000),
    exitCode: Object.values(sources).every((s) => s.ok) ? 0 : 2,
  };
}

// ---------------------------------------------------------------------------------------------------------------------
// Output

const pct = (v) => (v === null ? "n/a" : `${(v * 100).toFixed(1)}%`);

export function render(result) {
  const out = [];
  const w = result.window;
  out.push(`Modern Web Guidance baseline  window ${w.start} .. ${w.end}  review cutoff ${w.reviewCutoff}  skill end ${w.mwgEnd}`);
  for (const [s, v] of Object.entries(result.sources)) out.push(`source ${s}: ${v.ok ? "ok" : `FAILED - ${v.error}`}`);
  for (const [name, a] of Object.entries(result.agents)) {
    out.push("", `agent ${name}`);
    const line = (label, node, text) => out.push(`  ${label.padEnd(5)} ${node.state === "unavailable" ? `unavailable  needs ${node.needs.join(", ")}; failed ${node.failed.join(", ")}` : `${node.state.padEnd(12)} ${text(node)}`}`);
    line("units", a.units, (n) => `web ${n.web}, reviewed ${n.reviewed}, unreviewed ${n.unreviewed}, unlinked ${n.unlinked}`);
    const m = a.metrics;
    line("M1", m.M1, (n) => `n=${n.n} hits=${n.hits} share=${pct(n.value)}`);
    line("M1s", m.M1s, (n) => `n=${n.n} hits=${n.hits} share=${pct(n.value)} (self-report, not used)`);
    line("M2", m.M2, (n) => `n=${n.n} findings=${n.findings} per reviewed PR=${n.perReviewedPr ?? "n/a"}`);
    out.push(`  M2f   ${m.M2f.state}  ${m.M2f.reason}`);
    line("M3", m.M3, (n) => `n=${n.n} units with harm=${n.unitsWithHarm}`);
    line("M3p", m.M3p, (n) => `runs=${n.runs} with transcript=${n.runsWithTranscript} coverage=${pct(n.coverage)} hits=${n.hits} unconfirmed=${n.unconfirmed}`);
    line("M4", m.M4, (n) => `runs=${n.runs} listing bytes median=${n.listingBytesMedian} duration s median=${n.runDurationSecMedian} units=${n.units} tool calls per unit mean=${n.toolCallsPerUnitMean} median=${n.toolCallsPerUnitMedian}`);
  }
  out.push("", `wall ${result.wallSeconds}s  api calls github=${result.apiCalls.github} multica=${result.apiCalls.multica}  exit ${result.exitCode}`);
  return `${out.join("\n")}\n`;
}

// ---------------------------------------------------------------------------------------------------------------------
// CLI

const USAGE = `usage: mwg-baseline.mjs --agent NAME [--agent NAME ...] [--days 14] [--start ISO --end ISO] [--review-cutoff ISO] [--mwg-end ISO]
  [--repo OWNER/REPO ...] [--transcript-root DIR] [--pr-limit 100] [--fixture-dir DIR] [--json] [--commands]
Exit 0 when every source answered, 2 when any source failed (affected metrics print "unavailable", never a count), 64 on bad arguments.`;

function parseArgs(argv) {
  const o = { agent: [], repo: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--json" || a === "--commands" || a === "--help") o[a.slice(2)] = true;
    else if (a.startsWith("--") && i + 1 < argv.length) {
      const key = a.slice(2);
      const val = argv[++i];
      if (key === "agent" || key === "repo") o[key].push(val);
      else o[key] = val;
    } else throw new Error(`bad argument: ${a}`);
  }
  return o;
}

export async function main(argv, env = process.env, io = { write: (s) => process.stdout.write(s) }) {
  let o;
  try {
    o = parseArgs(argv);
    if (o.help) {
      io.write(`${USAGE}\n`);
      return 0;
    }
    if (o.commands) {
      io.write(`${READ_ONLY_COMMANDS.map((c) => c.join(" ")).join("\n")}\n`);
      return 0;
    }
    if (!o.agent.length) throw new Error("--agent is required");
    const date = (v, label) => {
      const d = new Date(v);
      if (Number.isNaN(d.getTime())) throw new Error(`bad ${label}: ${v}`);
      return d;
    };
    const end = o.end ? date(o.end, "--end") : new Date(Math.floor(Date.now() / 1000) * 1000);
    const start = o.start ? date(o.start, "--start") : new Date(end.getTime() - Number(o.days ?? 14) * DAY);
    const result = await measure({
      sources: o["fixture-dir"] ? fixtureSources(o["fixture-dir"]) : liveSources({ env }),
      agents: o.agent,
      start,
      end,
      reviewCutoff: o["review-cutoff"] ? date(o["review-cutoff"], "--review-cutoff") : end,
      mwgEnd: o["mwg-end"] ? date(o["mwg-end"], "--mwg-end") : end,
      transcriptRoot: o["transcript-root"] ?? join(env.HOME ?? "", ".claude", "projects"),
      repos: o.repo.length ? o.repo : ["cheese-work/multica", "cheese-work/doop"],
      prLimit: Number(o["pr-limit"] ?? 100),
    });
    io.write(o.json ? `${JSON.stringify(result, null, 2)}\n` : render(result));
    return result.exitCode;
  } catch (e) {
    io.write(`${e.message}\n${USAGE}\n`);
    return 64;
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = await main(process.argv.slice(2), process.env);
}
