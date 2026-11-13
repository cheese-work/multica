// CHE-3701: turn the weekly scanners' raw reports into one line per scanner (new / fixed / total by severity).
// Usage: node summarize.mjs <reports-dir> <previous-summary.json|-> <summary.json> [checkout-root]
import { createHash } from "node:crypto";
import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

const SEVERITIES = ["critical", "high", "medium", "low"];
const SCANNERS = ["semgrep", "govulncheck", "pnpm-audit", "gitleaks", "codeql", "trivy"];
const LEVELS = { error: "high", warning: "medium" };
const band = (score) => (score >= 9 ? "critical" : score >= 7 ? "high" : score >= 4 ? "medium" : "low");

// Every part must report: a half-run scanner (one CodeQL language, one image) is NOT RUN, not a smaller total.
const REQUIRED = {
  codeql: ["codeql-go.sarif", "codeql-javascript-typescript.sarif"],
  trivy: ["trivy-backend.json", "trivy-web.json"],
};

// Stable per-instance id: `<rule>|<path>|<hash of the flagged source text>#<n>`. Lines move between runs, so the line is
// never in the id; the text is. A replaced occurrence gets a new hash (new + fixed); a shifted one keeps its hash.
// n only separates identical-text duplicates in one file. Needs the checkout the scan ran on (`root`).
// ponytail: an unreadable file hashes to "", so its instances fall back to rank within rule+file.
const flaggedText = (root, path, start, end) => {
  try {
    const lines = readFileSync(join(root, path), "utf8").split("\n");
    return lines.slice(Math.max(start - 1, 0), Math.max(end, start)).map((l) => l.trim().replace(/\s+/g, " ")).join("\n");
  } catch {
    return "";
  }
};
const instances = (found, root) => {
  const seen = new Map();
  return [...found].sort((a, b) => a.line - b.line).map(({ rule, path, line, endLine, severity }) => {
    const text = flaggedText(root, path, line, endLine);
    const key = `${rule}|${path}|${text ? createHash("sha1").update(text).digest("hex").slice(0, 12) : ""}`;
    const n = (seen.get(key) ?? 0) + 1;
    seen.set(key, n);
    return { id: `${key}#${n}`, severity };
  });
};

const NORMALIZERS = {
  semgrep: (raw, root) => instances(raw.results.map((r) => ({
    rule: r.check_id, path: r.path,
    line: r.start?.line ?? 0, endLine: r.end?.line ?? r.start?.line ?? 0,
    severity: { ERROR: "high", WARNING: "medium" }[r.extra.severity] ?? "low",
  })), root),
  // govulncheck reports only vulnerabilities the code can reach; it carries no severity, so each counts as high.
  govulncheck: (text) => [...new Set([...text.matchAll(/^Vulnerability #\d+: (GO-\d+-\d+)/gm)].map((m) => m[1]))]
    .map((id) => ({ id, severity: "high" })),
  "pnpm-audit": (raw) => Object.values(raw.advisories ?? {}).map((a) => ({
    id: `${a.github_advisory_id ?? a.id}|${a.module_name}`,
    severity: { critical: "critical", high: "high", moderate: "medium" }[a.severity] ?? "low",
  })),
  gitleaks: (raw) => raw.map((l) => ({ id: l.Fingerprint ?? `${l.RuleID}|${l.File}`, severity: "high" })),
  codeql: (sarif, root) => sarif.runs.flatMap((r) => {
    const scores = new Map((r.tool.driver.rules ?? []).map((x) => [x.id, Number(x.properties?.["security-severity"])]));
    return instances((r.results ?? []).map((x) => {
      const score = scores.get(x.ruleId);
      const loc = x.locations?.[0]?.physicalLocation;
      return {
        rule: x.ruleId, path: loc?.artifactLocation?.uri ?? "",
        line: loc?.region?.startLine ?? 0, endLine: loc?.region?.endLine ?? loc?.region?.startLine ?? 0,
        severity: Number.isFinite(score) ? band(score) : (LEVELS[x.level] ?? "low"),
      };
    }), root);
  }),
  trivy: (raw) => (raw.Results ?? []).flatMap((r) => (r.Vulnerabilities ?? []).map((v) => ({
    id: `${r.Target}|${v.VulnerabilityID}|${v.PkgName}`,
    severity: { CRITICAL: "critical", HIGH: "high", MEDIUM: "medium" }[v.Severity] ?? "low",
  }))),
};

export const normalize = (scanner, raw, root = ".") => NORMALIZERS[scanner](raw, root);

const RANK = { critical: 3, high: 2, medium: 1, low: 0 };
export function summarize(scanner, rawItems, prev) {
  // One finding per id (worst severity wins) so total, by-severity and new count the same thing.
  const worst = new Map();
  for (const i of rawItems) if (!worst.has(i.id) || RANK[i.severity] > RANK[worst.get(i.id).severity]) worst.set(i.id, i);
  const items = [...worst.values()];
  const ids = [...new Set(items.map((i) => i.id))].sort();
  const before = new Set(prev?.ids ?? []);
  const bySeverity = Object.fromEntries(SEVERITIES.map((s) => [s, items.filter((i) => i.severity === s).length]));
  const fresh = items.filter((i) => !before.has(i.id));
  return {
    scanner, baseline: !prev, total: items.length, bySeverity, ids,
    new: [...new Set(fresh.map((i) => i.id))].sort(),
    fixed: [...before].filter((id) => !ids.includes(id)).sort(),
    newHighCritical: [...new Set(fresh.filter((i) => i.severity === "critical" || i.severity === "high").map((i) => i.id))].sort(),
    newFindings: fresh.filter((i) => i.severity === "critical" || i.severity === "high").sort((a, b) => (a.id < b.id ? -1 : 1)),
  };
}

export const line = (s) => `${s.scanner}: new ${s.new.length} / fixed ${s.fixed.length} / total ${s.total} (${SEVERITIES.map((v) => `${v} ${s.bySeverity[v]}`).join(", ")})`;

// A scanner with no report, or one that cannot be read, is NOT RUN: reading it as zero findings would hide a broken scan.
export function run(dir, previous, root = ".") {
  const files = readdirSync(dir);
  const summaries = SCANNERS.map((scanner) => {
    const prev = previous?.summaries?.find((s) => s.scanner === scanner);
    const mine = files.filter((f) => f === `${scanner}.json` || f === `${scanner}.txt` || f.startsWith(`${scanner}-`));
    const notRun = (why) => ({ scanner, notRun: true, why, ids: prev?.ids ?? [] });
    if (!mine.length) return notRun("no report");
    const missing = (REQUIRED[scanner] ?? []).find((f) => !files.includes(f));
    if (missing) return notRun(`missing ${missing}`);
    try {
      const items = mine.flatMap((f) => {
        const text = readFileSync(join(dir, f), "utf8");
        return normalize(scanner, f.endsWith(".txt") ? text : JSON.parse(text), root);
      });
      return summarize(scanner, items, prev);
    } catch {
      return notRun("unreadable report");
    }
  });
  return { summaries, lines: summaries.map((s) => (s.notRun ? `${s.scanner}: NOT RUN (${s.why})` : line(s))) };
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? "").href) {
  const [dir, prevPath, out, root] = process.argv.slice(2);
  const previous = prevPath && prevPath !== "-" && existsSync(prevPath) ? JSON.parse(readFileSync(prevPath, "utf8")) : undefined;
  const { summaries, lines } = run(dir, previous, root);
  writeFileSync(out, JSON.stringify({ summaries }, null, 1));
  console.log(lines.join("\n"));
  if (summaries.some((s) => s.notRun)) process.exitCode = 1; // red run: a broken scanner must not pass as clean
}
