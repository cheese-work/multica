#!/usr/bin/env node
// Fork changelog line generator (CHE-1631 line contract, CHE-1634 layer 2). Pure
// functions plus two CLI modes:
//   PR check (CI):  node scripts/fork-changelog.mjs --event "$GITHUB_EVENT_PATH"
//     Prints `Entry: [<Section>] "<text>"` or `Hidden: <reason> — add ...` to the
//     job summary and fails a feat/fix/perf PR without a valid Changelog line, and
//     (CHE-4016) one with `Changelog: none` that changes user-facing files. The
//     workflow passes the changed files with --files <newline-separated list>; an
//     absent or unreadable list warns and passes.
//   Local preview:  node scripts/fork-changelog.mjs --fixture <file> --out <path>
//     (or `pnpm changelog:preview`) writes the entries for a fixture, with no
//     credentials and no network. The default output is git-ignored. Add
//     --backfill scripts/fork-changelog-backfill.json to include the hand-written
//     entries for PRs merged before the live rule (CHE-2196).
// Optional flags/env: --start / FORK_CHANGELOG_START (YYYY-MM-DD, 00:00 ICT) and
// --sync-prefix / FORK_CHANGELOG_SYNC_PREFIX.
import { appendFileSync, mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
import { pathToFileURL } from "node:url";

const DEFAULT_START = "2026-10-11";
const DEFAULT_SYNC_PREFIX = "upstream-sync/";
const MAX_CODE_POINTS = 140;
const ICT_OFFSET_MS = 7 * 60 * 60 * 1000;

const LINE = /^Changelog(?:\((feat|fix|perf)\))?:\s?([^\n]*)$/;
const SECTIONS = { feat: "New Features", fix: "Bug Fixes", perf: "Improvements" };
const REQUIRES_LINE = new Set(["feat", "fix", "perf"]);

// Paths where `Changelog: none` is honest (CHE-4016). Everything else is user-facing.
// Root-only prefixes; test-ish directories match at any depth.
const INTERNAL_PREFIXES = [".github/", ".agents/", ".planning/", ".plans/", "deploy/", "scripts/", "docs/", "design/"];
const INTERNAL_ANYWHERE = /(^|\/)(__tests__|tests|e2e|fixtures)\//;
const INTERNAL_FILE = /(\.md|_test\.go|\.test\.(ts|tsx|mjs)|\.spec\.[^/]+)$/;
const MAX_LISTED_FILES = 5;

export const isInternalPath = (path) =>
  INTERNAL_PREFIXES.some((prefix) => path.startsWith(prefix)) || INTERNAL_ANYWHERE.test(path) || INTERNAL_FILE.test(path);

// Everything invisible is stripped by Unicode property, not by list: default-ignorable
// (zero-width, bidi, fillers, variation selectors, tags), format, control, unassigned,
// private use, surrogates and line/paragraph separators. Valid emoji survive: a base
// keeps its skin tone, one VS16 and a joiner that leads into another pictograph; a
// keycap keeps its VS16; the three tag flags (England, Scotland, Wales) stay whole.
const INVISIBLE = "\\p{Default_Ignorable_Code_Point}\\p{Cf}\\p{Cc}\\p{Cn}\\p{Co}\\p{Cs}\\p{Zl}\\p{Zp}";
const tagFlag = (code) => `\u{1F3F4}${[...code].map((ch) => String.fromCodePoint(0xe0000 + ch.charCodeAt(0))).join("")}\u{E007F}`;
const TAG_FLAGS = ["gbeng", "gbsct", "gbwls"].map(tagFlag).join("|");
const STRIP_INVISIBLE = new RegExp(
  `(${TAG_FLAGS}|[0-9#*]\\ufe0f\\u20e3)|(\\p{Extended_Pictographic}[\\u{1F3FB}-\\u{1F3FF}]*)(\\ufe0f)?(\\u200d(?=\\p{Extended_Pictographic}))?|[${INVISIBLE}]`,
  "gu",
);
// Credential and issue-id probe only: NFKD folds look-alikes (full-width letters) and
// splits accented letters into base plus mark, then every mark is dropped so no mark can
// split a pattern or compose into a different letter first.
// Dash look-alikes fold to a hyphen (NFKD already folds U+2011 and U+FF0D).
const DASHES = /[\u2010-\u2015\u2212\ufe58\ufe63]/g;
const PROBE_MARKS = /[\p{Cf}\p{Mn}\p{Me}]/gu;
const TOKENS = [
  /ghp_[A-Za-z0-9]{10,}/,
  /github_pat_[A-Za-z0-9_]{10,}/,
  /\bsk-[A-Za-z0-9_-]{10,}/,
  /\bAKIA[0-9A-Z]{12,}/,
  /\beyJ[\w-]+\.[\w-]+\.[\w-]+/,
];

// Drops HTML comments (an unclosed one runs to the end) and fenced code. A fence
// closes only on the same character, at least as long, with nothing else on the line.
function liveLines(body) {
  const text = body.replace(/\r\n?/g, "\n").replace(/<!--[\s\S]*?(-->|$)/g, "");
  let fence = null;
  return text.split("\n").filter((line) => {
    if (fence) {
      const close = /^ {0,3}(`{3,}|~{3,})\s*$/.exec(line);
      if (close && close[1][0] === fence[0] && close[1].length >= fence.length) fence = null;
      return false;
    }
    const open = /^ {0,3}(`{3,}|~{3,})/.exec(line);
    if (open) {
      fence = open[1];
      return false;
    }
    return true;
  });
}

const result = (status, text, override, warnings, overLimit = false) => ({ status, text, override, warnings, overLimit });

export function parseChangelogLine(body) {
  const matches = liveLines(typeof body === "string" ? body : "")
    .map((line) => LINE.exec(line))
    .filter(Boolean);
  if (matches.length === 0) return result("missing", null, null, []);
  const warnings = [];
  if (matches.length > 1) warnings.push("More than one Changelog line: the first one is used.");

  const [, override, raw] = matches[0];
  const spaced = raw.trim().replace(/[\t\v\f]/g, " ");
  const stripped = spaced.replace(STRIP_INVISIBLE, (match, whole, base, selector, joiner) => {
    if (whole) return whole;
    return base ? `${base}${selector ?? ""}${joiner ?? ""}` : "";
  });
  const cleaned = stripped.replace(/\s+/g, " ").trim();
  if (stripped !== spaced) warnings.push("Stripped invisible or control characters from the sentence.");
  if (cleaned === "") return result("missing", null, null, warnings);
  if (/<[^\s<>][^<>]*>/.test(cleaned)) {
    warnings.push("The Changelog line holds a <placeholder>.");
    return result("missing", null, null, warnings);
  }
  if (cleaned.toLowerCase() === "none") return result("none", null, null, warnings);
  const probe = cleaned.normalize("NFKD").replace(PROBE_MARKS, "").replace(DASHES, "-");
  if (TOKENS.some((token) => token.test(probe))) {
    warnings.push("The sentence looks like a credential and is hidden.");
    return result("parse-error", null, null, warnings);
  }
  if (/\bCHE-\d+\b/i.test(probe)) {
    warnings.push("The sentence names an issue id and is hidden: users cannot open it.");
    return result("parse-error", null, null, warnings);
  }

  // Never cut: the PR check fails an over-limit line so its author shortens it, and
  // entries merged before that rule keep their complete sentence.
  const overLimit = [...cleaned].length > MAX_CODE_POINTS;
  if (overLimit) warnings.push(`The Changelog line is over ${MAX_CODE_POINTS} characters: shorten it to one short sentence.`);
  return result("text", cleaned, override ?? null, warnings, overLimit);
}

const titleType = (title) => /^([a-z]+)(?:\([^)]*\))?!?:/i.exec(title ?? "")?.[1].toLowerCase() ?? null;

export function sectionFor(title, override) {
  return SECTIONS[override] ?? SECTIONS[titleType(title)] ?? "Improvements";
}

// --- revert detection -------------------------------------------------------

const REVERT_TITLE = /^Revert\s+"(.+)"(?:\s*\(#\d+\))?\s*$/i;
const REVERT_BODY = /Reverts\s+cheese-work\/multica#(\d+)/gi;

const isRevertPr = (pr) =>
  REVERT_TITLE.test(pr.title ?? "") || /Reverts\s+cheese-work\/multica#\d+/i.test(pr.body ?? "");

// PR numbers a revert PR points at: body line, then the quoted title.
function revertTargets(pr, prs) {
  const targets = new Set([...(pr.body ?? "").matchAll(REVERT_BODY)].map((m) => Number(m[1])));
  const inner = REVERT_TITLE.exec(pr.title ?? "")?.[1];
  if (inner) {
    const numbered = /\(#(\d+)\)\s*$/.exec(inner);
    const stripped = inner.replace(/\s*\(#\d+\)\s*$/, "");
    const byTitle = prs.find((p) => p !== pr && (p.title === inner || p.title === stripped));
    if (numbered) targets.add(Number(numbered[1]));
    else if (byTitle) targets.add(byTitle.number);
  }
  return targets;
}

// Edges point from a reverting commit to the commit it reverts. A revert that is
// itself reverted no longer hides its target.
function revertedShas(prs, commits) {
  const shas = new Set([...commits.map((c) => c.sha), ...prs.map((p) => p.mergeSha)]);
  const byNumber = new Map(prs.map((p) => [p.number, p]));
  const reverters = new Map(); // target sha -> Set of reverting shas
  const link = (from, to) => {
    if (!from || !to || from === to) return;
    if (!reverters.has(to)) reverters.set(to, new Set());
    reverters.get(to).add(from);
  };
  for (const pr of prs) {
    for (const n of revertTargets(pr, prs)) link(pr.mergeSha, byNumber.get(n)?.mergeSha);
  }
  for (const commit of commits) {
    for (const [, prefix] of (commit.message ?? "").matchAll(/This reverts commit ([0-9a-f]{7,40})/gi)) {
      link(commit.sha, [...shas].find((s) => s.startsWith(prefix.toLowerCase())));
    }
  }
  const memo = new Map();
  const isReverted = (sha) => {
    if (memo.has(sha)) return memo.get(sha);
    memo.set(sha, false); // cycle guard
    const value = [...(reverters.get(sha) ?? [])].some((by) => !isReverted(by));
    memo.set(sha, value);
    return value;
  };
  const isReverter = (sha) => [...reverters.values()].some((from) => from.has(sha));
  return { isReverted, isReverter };
}

// --- evaluation -------------------------------------------------------------

export function optionsFromEnv(env) {
  const options = {};
  if (env.FORK_CHANGELOG_START) options.startDate = env.FORK_CHANGELOG_START;
  if (env.FORK_CHANGELOG_SYNC_PREFIX) options.syncPrefix = env.FORK_CHANGELOG_SYNC_PREFIX;
  return options;
}

const hidden = (reason, warnings = [], type = null) => ({ kind: "hidden", reason, warnings, type });

// One PR on its own: what the PR check prints and the generator's per-PR step.
export function evaluatePr(pr, options = {}) {
  const prefix = options.syncPrefix ?? DEFAULT_SYNC_PREFIX;
  const type = titleType(pr.title);
  if ((pr.headRef ?? "").startsWith(prefix)) return hidden("upstream-sync", [], type);
  if (isRevertPr(pr)) return hidden("reverted", [], type);
  const parsed = parseChangelogLine(pr.body);
  if (parsed.status !== "text") return hidden(parsed.status, parsed.warnings, type);
  return {
    kind: "entry",
    section: sectionFor(pr.title, parsed.override),
    text: parsed.text,
    warnings: parsed.warnings,
    overLimit: parsed.overLimit,
  };
}

// files: changed paths, or null when the list could not be read.
export function formatCheck(evaluation, files = null) {
  if (evaluation.kind === "entry") {
    return { line: `Entry: [${evaluation.section}] "${evaluation.text}"`, failing: evaluation.overLimit, warnings: evaluation.warnings };
  }
  if (evaluation.reason === "none" && REQUIRES_LINE.has(evaluation.type)) {
    const line = `Hidden: none — add "Changelog: <sentence>" to publish`;
    if (files === null) {
      return { line, failing: false, warnings: [...evaluation.warnings, "Could not read the changed files: the `Changelog: none` check was skipped."] };
    }
    const userFacing = files.filter((file) => !isInternalPath(file));
    if (userFacing.length > 0) {
      const shown = userFacing.slice(0, MAX_LISTED_FILES).join(", ");
      const more = userFacing.length > MAX_LISTED_FILES ? `, and ${userFacing.length - MAX_LISTED_FILES} more` : "";
      return {
        line,
        failing: true,
        warnings: evaluation.warnings,
        error: `This \`${evaluation.type}\` PR changes user-facing code. Write one sentence a user understands in \`Changelog:\`, or use a non-user-facing type (for example \`ci\`, \`test\`, \`chore\`). User-facing files: ${shown}${more}`,
      };
    }
  }
  const missingLine = evaluation.reason === "missing" || evaluation.reason === "parse-error";
  return {
    line: `Hidden: ${evaluation.reason} — add "Changelog: <sentence>" to publish`,
    failing: missingLine && REQUIRES_LINE.has(evaluation.type),
    warnings: evaluation.warnings,
  };
}

// Hand-written entries for PRs merged before the live rule (default: next to this script).
export const loadBackfill = (path = new URL("./fork-changelog-backfill.json", import.meta.url)) =>
  JSON.parse(readFileSync(path, "utf8")).entries;

const startMs = (startDate) => {
  const ms = Date.parse(`${startDate}T00:00:00+07:00`);
  if (Number.isNaN(ms)) throw new Error(`Invalid start date: ${startDate}`);
  return ms;
};
const ictDate = (ms) => new Date(ms + ICT_OFFSET_MS).toISOString().slice(0, 10);

// Hidden reasons that mean "a user-facing PR may have shipped without a note" (CHE-4015).
const COUNTED_HIDDEN = new Set(["none", "missing", "parse-error"]);

// One divider per successful C00 deploy whose SHA is in the first-parent history (newest
// first). A redeploy or rollback of a SHA keeps its earliest success; the qualified build
// (commits[0]) is the Live deploy. A PR belongs to the oldest deploy that contains it: the
// deploy nearest to it at or after it in history, else Live.
function planDeploys(commits, deployments, options) {
  const index = new Map(commits.map((c, i) => [c.sha, i]));
  const earliest = new Map();
  for (const { sha, state, createdAt } of deployments) {
    const at = Date.parse(createdAt);
    if (String(state).toUpperCase() !== "SUCCESS" || !index.has(sha) || index.get(sha) === 0 || Number.isNaN(at)) continue;
    if (!(at >= (earliest.get(sha) ?? Infinity))) earliest.set(sha, at);
  }
  const versionOf = options.versionOf ?? ((sha) => sha.slice(0, 7));
  // Live is the newest build: never dated before a previous deploy that finished later.
  const liveTime = Math.max(Date.parse(commits[0]?.time ?? ""), ...earliest.values());
  const deploys = [
    { sha: commits[0].sha, version: versionOf(commits[0].sha), deployedAt: new Date(Number.isNaN(liveTime) ? (options.now ?? new Date()) : liveTime).toISOString(), hiddenCount: 0, live: true },
    ...[...earliest].sort((a, b) => index.get(a[0]) - index.get(b[0]))
      .map(([sha, at]) => ({ sha, version: versionOf(sha), deployedAt: new Date(at).toISOString(), hiddenCount: 0 })),
  ];
  const deployFor = (sha) => {
    const position = index.get(sha);
    if (position === undefined) return null;
    return deploys.slice(1).findLast((d) => index.get(d.sha) <= position) ?? deploys[0];
  };
  return { deploys, deployFor };
}

export function generateEntries({ prs, firstParent = null, deployments = null }, options = {}) {
  const start = startMs(options.startDate ?? DEFAULT_START);
  const commits = (firstParent ?? []).map((c) => (typeof c === "string" ? { sha: c, message: "" } : c));
  const onMain = firstParent ? new Set(commits.map((c) => c.sha)) : null;
  const considered = prs.filter((pr) => !onMain || onMain.has(pr.mergeSha));
  const { isReverted, isReverter } = revertedShas(considered, commits);

  const plan = firstParent && deployments && commits.length ? planDeploys(commits, deployments, options) : null;
  const entries = [];
  const hiddenList = [];
  const noteHidden = (pr, reason) => {
    hiddenList.push({ number: pr.number, reason });
    const deploy = plan && COUNTED_HIDDEN.has(reason) ? plan.deployFor(pr.mergeSha) : null;
    if (deploy) deploy.hiddenCount += 1;
  };
  const warnings = [];
  for (const pr of considered) {
    const mergedMs = Date.parse(pr.mergedAt);
    let hide = null;
    if ((pr.headRef ?? "").startsWith(options.syncPrefix ?? DEFAULT_SYNC_PREFIX)) hide = "upstream-sync";
    else if (Number.isNaN(mergedMs)) hide = "parse-error";
    else if (mergedMs < start) hide = "before-start-date";
    else if (isRevertPr(pr) || isReverter(pr.mergeSha) || isReverted(pr.mergeSha)) hide = "reverted";
    if (hide) {
      noteHidden(pr, hide);
      continue;
    }
    const evaluation = evaluatePr(pr, options);
    for (const message of evaluation.warnings) warnings.push({ number: pr.number, message });
    if (evaluation.kind === "hidden") {
      noteHidden(pr, evaluation.reason);
      continue;
    }
    const deploy = plan?.deployFor(pr.mergeSha);
    entries.push({
      id: `fork-pr-${pr.number}`,
      number: pr.number,
      mergedAt: new Date(mergedMs).toISOString(),
      date: ictDate(mergedMs),
      section: evaluation.section,
      text: evaluation.text,
      ...(deploy && { deploy: deploy.live ? "live" : deploy.sha }),
    });
  }
  // Backfill entries (CHE-2196) predate the live rule; a live PR entry of the same number wins.
  const live = new Set(entries.map((e) => e.number));
  entries.push(...(options.backfill ?? []).filter((e) => !live.has(e.number)));
  entries.sort((a, b) => Date.parse(b.mergedAt) - Date.parse(a.mergedAt) || b.number - a.number);

  const byReason = {};
  for (const { reason } of hiddenList) byReason[reason] = (byReason[reason] ?? 0) + 1;
  return {
    entries,
    ...(plan && { deploys: plan.deploys }),
    hidden: hiddenList,
    warnings,
    counts: { shown: entries.length, hidden: hiddenList.length, byReason },
  };
}

export const serializeEntries = ({ entries, deploys }) => `${JSON.stringify({ entries, deploys }, null, 2)}\n`;

// Delete the target, write a temp file, rename on success: a failure leaves no file.
export function writeAtomically(path, text) {
  rmSync(path, { force: true });
  mkdirSync(dirname(path), { recursive: true });
  const temp = `${path}.${process.pid}.tmp`;
  try {
    writeFileSync(temp, text);
    renameSync(temp, path);
  } catch (error) {
    rmSync(temp, { force: true });
    throw error;
  }
}

// --- CLI --------------------------------------------------------------------

function parseArgs(argv) {
  const args = {};
  for (let i = 0; i < argv.length; i += 2) {
    const key = argv[i];
    if (!key.startsWith("--") || argv[i + 1] === undefined) throw new Error(`Bad argument: ${key}`);
    args[key.slice(2)] = argv[i + 1];
  }
  return args;
}

function readFiles(path) {
  if (!path) return null;
  try {
    return readFileSync(path, "utf8").split("\n").filter(Boolean);
  } catch {
    return null;
  }
}

// Workflow-command data: escape %, CR and LF so a file name or PR text cannot start a forged command.
const escapeAnnotation = (text) => String(text).replace(/%/g, "%25").replace(/\r/g, "%0D").replace(/\n/g, "%0A");

function runCheck(eventPath, filesPath, options, env) {
  const { pull_request: pr } = JSON.parse(readFileSync(eventPath, "utf8"));
  if (!pr) throw new Error("The event has no pull_request");
  if (pr.state === "closed") {
    console.log("Skipped: the PR is closed.");
    return 0;
  }
  const input = { number: pr.number, title: pr.title ?? "", body: pr.body ?? "", headRef: pr.head?.ref ?? "" };
  const check = formatCheck(evaluatePr(input, options), readFiles(filesPath));
  console.log(check.line.replace(/[\r\n]+/g, " "));
  for (const message of check.warnings) console.log(`::warning::${escapeAnnotation(message)}`);
  if (check.failing) {
    console.log(`::error::${escapeAnnotation(check.error ?? 'This PR has no valid Changelog line (feat/fix/perf need one; any line over 140 characters fails). Add "Changelog: <sentence>" or "Changelog: none" (none is only for internal-only changes).')}`);
  }
  if (env.GITHUB_STEP_SUMMARY) {
    const lines = ["### Fork changelog", "", "```text", check.line.replace(/`/g, "'"), "```"];
    for (const message of check.warnings) lines.push("", `- Warning: ${message}`);
    appendFileSync(env.GITHUB_STEP_SUMMARY, `${lines.join("\n")}\n`);
  }
  return check.failing ? 1 : 0;
}

function runPreview(fixturePath, outPath, options) {
  const fixture = JSON.parse(readFileSync(fixturePath, "utf8"));
  const generated = generateEntries(fixture, { ...options, versionOf: (sha) => fixture.versions?.[sha] ?? sha.slice(0, 7) });
  writeAtomically(outPath, serializeEntries(generated));
  const { shown, hidden: count } = generated.counts;
  console.log(`Wrote ${outPath}: ${shown} shown, ${count} hidden.`);
  return 0;
}

export function main(argv, env) {
  try {
    const args = parseArgs(argv);
    const options = { ...optionsFromEnv(env) };
    if (args.start) options.startDate = args.start;
    if (args["sync-prefix"]) options.syncPrefix = args["sync-prefix"];
    if (args.backfill) options.backfill = loadBackfill(args.backfill);
    if (args.event) return runCheck(args.event, args.files, options, env);
    if (args.fixture && args.out) return runPreview(args.fixture, args.out, options);
    throw new Error("Use --event <file> or --fixture <file> --out <path>");
  } catch (error) {
    console.error(`fork-changelog: ${error.message}`);
    return 1;
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exitCode = main(process.argv.slice(2), process.env);
}
