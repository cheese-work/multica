// CHE-3701: turn each NEW high/critical finding in summary.json into one issue plan entry, and say whether the
// finding touches code shared with upstream (multica-ai/multica). Upstream-shared findings are reported privately.
// Usage: node triage.mjs <summary.json> <triage.json>                          plan only, no credentials, runs in CI
//        node triage.mjs --apply <triage.json> <assignee-id> <project-id>      files the issues; needs the multica CLI login
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

const strip = (s) => s.replace(/#\d+$/, "");
// Where the finding lives, per scanner. null = cannot tell (govulncheck ids carry no package).
const SHARED = {
  semgrep: (id, has) => has.path(strip(id.split("|")[1])),
  codeql: (id, has) => has.path(strip(id.split("|")[1])),
  gitleaks: (id, has) => has.commit(id.split(":")[0]),
  "pnpm-audit": (id, has) => has.pkg(id.split("|")[1]),
  trivy: (id, has) => has.pkg(id.split("|")[2]),
  govulncheck: () => null,
};

export function plan(summaries, has) {
  return summaries
    .filter((s) => !s.notRun) // a scanner with no previous scheduled summary files its findings too, marked baseline
    .flatMap((s) => (s.newFindings ?? []).map(({ id, severity }) => {
      const upstreamShared = SHARED[s.scanner]?.(id, has) ?? null;
      const lines = [
        `Weekly security scan found a new ${severity} finding.`, "", `- Scanner: ${s.scanner}`, `- Finding: \`${id}\``, `- Severity: ${severity}`,
      ];
      if (upstreamShared) {
        lines.push("", "This finding touches code shared with upstream. Report it privately to the upstream maintainers (their security contact). Never a public upstream issue or PR.");
      } else if (upstreamShared === null) {
        lines.push("", "Upstream sharing is unknown for this scanner. Check the package or file against upstream before any report.");
      }
      return { scanner: s.scanner, id, severity, upstreamShared, baseline: !!s.baseline, title: `${s.scanner} ${severity}: ${id}`, body: lines.join("\n") };
    }));
}

export const issueArgs = (e, { assigneeId, project, descriptionFile }) => [
  "issue", "create",
  "--title", `${e.upstreamShared ? "[upstream-private] " : ""}Security weekly: ${e.title}`,
  "--description-file", descriptionFile,
  "--assignee-id", assigneeId,
  "--project", project,
  "--priority", e.severity === "critical" ? "urgent" : "high",
  // Baseline findings predate the scan: park them in backlog so a first run of hundreds starts no agent run per finding.
  "--status", e.baseline ? "backlog" : "todo",
  "--property", "Readiness=ready",
];

// Upstream lookups run against the `upstream` remote's default branch (fetched by the caller).
const git = (...a) => { try { execFileSync("git", a, { stdio: "ignore" }); return true; } catch { return false; } };
export const upstreamLookups = (ref = "upstream/main") => !git("rev-parse", "--verify", "--quiet", ref)
  ? { path: () => null, pkg: () => null, commit: () => null } // upstream unavailable: unknown, never "not shared"
  : {
  path: (p) => git("cat-file", "-e", `${ref}:${p}`),
  pkg: (n) => git("grep", "-q", "-F", "--", n, ref, "--", "pnpm-lock.yaml", "server/go.sum"),
  commit: (c) => git("merge-base", "--is-ancestor", c, ref),
};

// Files one issue per entry. A failed create (multica refuses an active duplicate by default) never stops the rest;
// the failed entries are returned so the caller can report them and exit red.
export function apply(entries, { assigneeId, project, run = (args) => execFileSync("multica", args, { stdio: "inherit" }) }) {
  const dir = mkdtempSync(join(process.cwd(), ".triage-"));
  const failed = [];
  try {
    for (const e of entries) {
      const descriptionFile = join(dir, "d.md");
      writeFileSync(descriptionFile, e.body);
      try { run(issueArgs(e, { assigneeId, project, descriptionFile })); } catch (err) {
        console.error(`triage: could not file ${e.scanner} ${e.id}: ${err.message}`);
        failed.push(e);
      }
    }
  } finally { rmSync(dir, { recursive: true, force: true }); }
  return failed;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  if (process.argv[2] === "--apply") {
    const [, , , file, assigneeId, project] = process.argv;
    if (apply(JSON.parse(readFileSync(file, "utf8")), { assigneeId, project }).length) process.exitCode = 1;
  } else {
    const [summaryPath, out] = process.argv.slice(2);
    const entries = plan(JSON.parse(readFileSync(summaryPath, "utf8")).summaries, upstreamLookups());
    writeFileSync(out, JSON.stringify(entries, null, 1));
    console.log(`triage: ${entries.length} new high/critical finding(s), ${entries.filter((e) => e.upstreamShared).length} upstream-shared`);
  }
}
