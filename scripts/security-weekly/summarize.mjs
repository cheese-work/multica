// CHE-3701: turn the weekly scanners' raw reports into one line per scanner (new / fixed / total by severity).
// Usage: node summarize.mjs <reports-dir> <previous-summary.json|-> <summary.json>
import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

const SEVERITIES = ["critical", "high", "medium", "low"];
const SCANNERS = ["semgrep", "govulncheck", "pnpm-audit", "gitleaks", "codeql", "trivy"];
const LEVELS = { error: "high", warning: "medium" };
const band = (score) => (score >= 9 ? "critical" : score >= 7 ? "high" : score >= 4 ? "medium" : "low");

const NORMALIZERS = {
  semgrep: (raw) => raw.results.map((r) => ({
    id: `${r.check_id}|${r.path}`,
    severity: { ERROR: "high", WARNING: "medium" }[r.extra.severity] ?? "low",
  })),
  // govulncheck reports only vulnerabilities the code can reach; it carries no severity, so each counts as high.
  govulncheck: (text) => [...new Set([...text.matchAll(/^Vulnerability #\d+: (GO-\d+-\d+)/gm)].map((m) => m[1]))]
    .map((id) => ({ id, severity: "high" })),
  "pnpm-audit": (raw) => Object.values(raw.advisories ?? {}).map((a) => ({
    id: `${a.github_advisory_id ?? a.id}|${a.module_name}`,
    severity: { critical: "critical", high: "high", moderate: "medium" }[a.severity] ?? "low",
  })),
  gitleaks: (raw) => raw.map((l) => ({ id: l.Fingerprint ?? `${l.RuleID}|${l.File}`, severity: "high" })),
  codeql: (sarif) => sarif.runs.flatMap((r) => {
    const scores = new Map((r.tool.driver.rules ?? []).map((x) => [x.id, Number(x.properties?.["security-severity"])]));
    return (r.results ?? []).map((x) => {
      const score = scores.get(x.ruleId);
      return {
        id: `${x.ruleId}|${x.locations?.[0]?.physicalLocation?.artifactLocation?.uri ?? ""}`,
        severity: Number.isFinite(score) ? band(score) : (LEVELS[x.level] ?? "low"),
      };
    });
  }),
  trivy: (raw) => (raw.Results ?? []).flatMap((r) => (r.Vulnerabilities ?? []).map((v) => ({
    id: `${r.Target}|${v.VulnerabilityID}|${v.PkgName}`,
    severity: { CRITICAL: "critical", HIGH: "high", MEDIUM: "medium" }[v.Severity] ?? "low",
  }))),
};

export const normalize = (scanner, raw) => NORMALIZERS[scanner](raw);

export function summarize(scanner, items, prev) {
  const ids = [...new Set(items.map((i) => i.id))].sort();
  const before = new Set(prev?.ids ?? []);
  const bySeverity = Object.fromEntries(SEVERITIES.map((s) => [s, items.filter((i) => i.severity === s).length]));
  const fresh = items.filter((i) => !before.has(i.id));
  return {
    scanner, total: items.length, bySeverity, ids,
    new: [...new Set(fresh.map((i) => i.id))].sort(),
    fixed: [...before].filter((id) => !ids.includes(id)).sort(),
    newHighCritical: [...new Set(fresh.filter((i) => i.severity === "critical" || i.severity === "high").map((i) => i.id))].sort(),
  };
}

export const line = (s) => `${s.scanner}: new ${s.new.length} / fixed ${s.fixed.length} / total ${s.total} (${SEVERITIES.map((v) => `${v} ${s.bySeverity[v]}`).join(", ")})`;

// A scanner with no report, or one that cannot be read, is NOT RUN: reading it as zero findings would hide a broken scan.
export function run(dir, previous) {
  const files = readdirSync(dir);
  const summaries = SCANNERS.map((scanner) => {
    const prev = previous?.summaries?.find((s) => s.scanner === scanner);
    const mine = files.filter((f) => f === `${scanner}.json` || f === `${scanner}.txt` || f.startsWith(`${scanner}-`));
    const notRun = (why) => ({ scanner, notRun: true, why, ids: prev?.ids ?? [] });
    if (!mine.length) return notRun("no report");
    try {
      const items = mine.flatMap((f) => {
        const text = readFileSync(join(dir, f), "utf8");
        return normalize(scanner, f.endsWith(".txt") ? text : JSON.parse(text));
      });
      return summarize(scanner, items, prev);
    } catch {
      return notRun("unreadable report");
    }
  });
  return { summaries, lines: summaries.map((s) => (s.notRun ? `${s.scanner}: NOT RUN (${s.why})` : line(s))) };
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const [dir, prevPath, out] = process.argv.slice(2);
  const previous = prevPath && prevPath !== "-" && existsSync(prevPath) ? JSON.parse(readFileSync(prevPath, "utf8")) : undefined;
  const { summaries, lines } = run(dir, previous);
  writeFileSync(out, JSON.stringify({ summaries }, null, 1));
  console.log(lines.join("\n"));
  if (summaries.some((s) => s.notRun)) process.exitCode = 1; // red run: a broken scanner must not pass as clean
}
