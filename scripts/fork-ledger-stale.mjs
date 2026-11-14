// CHE-3960: weekly stale check for docs/fork/. The ledger pins upstream main (UPSTREAM-FIXES.md Method); it is stale when upstream
// moved, when the pin is older than 7 days, or when there is no pin. Security-class rows are listed by id only, so the weekly
// reader re-checks whether upstream fixed them. Usage: node scripts/fork-ledger-stale.mjs [--file <assignee-id> <project-id>]
// (exit 1 = stale, needs network). With --file, a stale result files one Multica issue and exits 0 once the owner is told (a throw exits non-zero); scripts/upstream-pr-watch.mjs (the X99 watcher entry) does that on every poll,
// so an owner hears about it whether or not the fork saw any push that week.
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

const PIN = /checked against upstream `main` `([0-9a-f]{40})` \((\d{4}-\d\d-\d\d \d\d:\d\d) ICT\)/;
const MAX_DAYS = 7;

export function check({ ledger, upstreamHead, now }) {
  const reasons = [];
  const m = PIN.exec(ledger);
  if (!m) reasons.push("no upstream pin found in the ledger");
  else {
    if (m[1] !== upstreamHead) reasons.push(`upstream main moved: pin ${m[1].slice(0, 10)}, now ${upstreamHead.slice(0, 10)}`);
    const days = (now - new Date(`${m[2].replace(" ", "T")}:00+07:00`)) / 864e5;
    if (days > MAX_DAYS) reasons.push(`pin is older than ${MAX_DAYS} days (${days.toFixed(1)} days)`);
  }
  const withheld = [...ledger.matchAll(/^\|[^|]*\|\s*(FX\d+)\s*\|.*details withheld/gm)].map((r) => r[1]);
  return { stale: reasons.length > 0, reasons, withheld };
}

export const TITLE = "chore(fork): refresh docs/fork ledger (weekly stale check)";
const REFILE_DAYS = 7;

// An issue with the exact title that is under REFILE_DAYS old, open or closed, blocks a new one: the refresh PR moves the pin, and a
// cancelled issue must not be refiled on every 15-minute poll. `run` takes multica args and returns stdout.
export function fileRefresh(result, { assigneeId, project, now, run }) {
  if (!result.stale) return "fresh";
  const { issues } = JSON.parse(run(["issue", "search", TITLE, "--include-closed", "--limit", "20", "--output", "json"]));
  if (issues.some((i) => i.title === TITLE && (now - new Date(i.created_at)) / 864e5 < REFILE_DAYS)) return "recent";
  const lines = [`The weekly ledger check failed: ${result.reasons.join("; ")}.`];
  if (result.withheld.length) lines.push(`Withheld security-class rows to re-check against upstream: ${result.withheld.join(", ")}.`);
  lines.push("Refresh docs/fork/ per docs/fork/PROCESS.md (Ledger refresh) and move the upstream pin.");
  run(["issue", "create", "--title", TITLE, "--description", lines.join("\n"), "--assignee-id", assigneeId, "--project", project, "--status", "todo", "--property", "Readiness=ready"]);
  return "filed";
}

// Ids: dev team squad, Cheese Work Setup project. The upstream remote is overridable for tests only.
const SQUAD = "a1073a68-4965-44ff-a8fb-244341935110";
const SETUP_PROJECT = "5e621e12-eeb4-40a5-9571-25657d5b0ec5";

// The whole weekly job: read this checkout's ledger, compare with upstream `main`, file the refresh issue when stale.
// scripts/upstream-pr-watch.mjs calls this on every poll: X99's installed run.sh only ever starts that file from a fresh checkout.
export function weekly({ assigneeId = SQUAD, project = SETUP_PROJECT, now = new Date(), env = process.env } = {}) {
  const ledger = readFileSync(new URL("../docs/fork/UPSTREAM-FIXES.md", import.meta.url), "utf8");
  const remote = env.FORK_UPSTREAM_REMOTE ?? "https://github.com/multica-ai/multica.git";
  const upstreamHead = execFileSync("git", ["ls-remote", remote, "refs/heads/main"], { encoding: "utf8" }).split(/\s/)[0];
  const result = check({ ledger, upstreamHead, now });
  const outcome = fileRefresh(result, { assigneeId, project, now, run: (a) => execFileSync("multica", a, { encoding: "utf8" }) });
  return { result, outcome };
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  const at = process.argv.indexOf("--file");
  if (at > 0) {
    const [assigneeId, project] = process.argv.slice(at + 1);
    if (!assigneeId || !project) throw new Error("--file needs <assignee-id> <project-id>");
    const { result, outcome } = weekly({ assigneeId, project });
    console.log(result.stale ? `STALE: ${result.reasons.join("; ")}` : "fresh");
    if (result.withheld.length) console.log(`withheld security-class rows to re-check: ${result.withheld.join(", ")}`);
    console.log(`refresh issue: ${outcome}`);
  } else {
    const ledger = readFileSync(new URL("../docs/fork/UPSTREAM-FIXES.md", import.meta.url), "utf8");
    const upstreamHead = execFileSync("git", ["ls-remote", "https://github.com/multica-ai/multica.git", "refs/heads/main"], { encoding: "utf8" }).split(/\s/)[0];
    const r = check({ ledger, upstreamHead, now: new Date() });
    console.log(r.stale ? `STALE: ${r.reasons.join("; ")}` : "fresh");
    if (r.withheld.length) console.log(`withheld security-class rows to re-check: ${r.withheld.join(", ")}`);
    process.exit(r.stale ? 1 : 0);
  }
}
