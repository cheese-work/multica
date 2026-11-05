import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { normalize, summarize, line, run } from "./summarize.mjs";

test("semgrep: ERROR is high, WARNING medium, INFO low; id ignores line numbers", () => {
  const raw = { results: [
    { check_id: "r.a", path: "a.go", start: { line: 1 }, extra: { severity: "ERROR" } },
    { check_id: "r.b", path: "b.ts", start: { line: 9 }, extra: { severity: "WARNING" } },
    { check_id: "r.c", path: "c.py", start: { line: 3 }, extra: { severity: "INFO" } },
  ] };
  assert.deepEqual(normalize("semgrep", raw), [
    { id: "r.a|a.go", severity: "high" }, { id: "r.b|b.ts", severity: "medium" }, { id: "r.c|c.py", severity: "low" },
  ]);
});

test("govulncheck text: one reachable vulnerability per distinct GO id, rated high", () => {
  const text = "Vulnerability #1: GO-2026-0001\n  details\nVulnerability #2: GO-2026-0002\nVulnerability #3: GO-2026-0001\n";
  assert.deepEqual(normalize("govulncheck", text), [
    { id: "GO-2026-0001", severity: "high" }, { id: "GO-2026-0002", severity: "high" },
  ]);
});

test("pnpm-audit: moderate maps to medium, info to low, id is advisory|module", () => {
  const raw = { advisories: {
    1: { github_advisory_id: "GHSA-x", module_name: "glob", severity: "critical" },
    2: { github_advisory_id: "GHSA-y", module_name: "ms", severity: "moderate" },
    3: { github_advisory_id: "GHSA-z", module_name: "ws", severity: "info" },
  } };
  assert.deepEqual(normalize("pnpm-audit", raw).map((i) => i.severity), ["critical", "medium", "low"]);
  assert.equal(normalize("pnpm-audit", raw)[0].id, "GHSA-x|glob");
});

test("gitleaks: every leak is high; empty or missing report is zero", () => {
  assert.deepEqual(normalize("gitleaks", [{ RuleID: "aws", File: "x.env", Fingerprint: "f1" }]), [{ id: "f1", severity: "high" }]);
  assert.deepEqual(normalize("gitleaks", []), []);
});

test("codeql sarif: security-severity picks the band, no score falls back to the level", () => {
  const sarif = { runs: [{
    tool: { driver: { rules: [
      { id: "go/sql", properties: { "security-severity": "9.8" } },
      { id: "js/xss", properties: { "security-severity": "7.2" } },
      { id: "js/misc", properties: {} },
    ] } },
    results: [
      { ruleId: "go/sql", level: "error", locations: [{ physicalLocation: { artifactLocation: { uri: "a.go" } } }] },
      { ruleId: "js/xss", level: "error", locations: [{ physicalLocation: { artifactLocation: { uri: "b.ts" } } }] },
      { ruleId: "js/misc", level: "warning", locations: [{ physicalLocation: { artifactLocation: { uri: "c.ts" } } }] },
    ],
  }] };
  assert.deepEqual(normalize("codeql", sarif).map((i) => i.severity), ["critical", "high", "medium"]);
});

test("trivy: CRITICAL/HIGH/MEDIUM/LOW/UNKNOWN, id carries target and package", () => {
  const raw = { Results: [{ Target: "web (alpine)", Vulnerabilities: [
    { VulnerabilityID: "CVE-1", PkgName: "openssl", Severity: "CRITICAL" },
    { VulnerabilityID: "CVE-2", PkgName: "zlib", Severity: "UNKNOWN" },
  ] }, { Target: "empty" }] };
  assert.deepEqual(normalize("trivy", raw), [
    { id: "web (alpine)|CVE-1|openssl", severity: "critical" }, { id: "web (alpine)|CVE-2|zlib", severity: "low" },
  ]);
});

test("summarize: new and fixed are set differences against the previous run", () => {
  const items = [{ id: "a", severity: "high" }, { id: "b", severity: "critical" }, { id: "c", severity: "low" }];
  const s = summarize("semgrep", items, { ids: ["a", "z"] });
  assert.deepEqual(s.bySeverity, { critical: 1, high: 1, medium: 0, low: 1 });
  assert.equal(s.total, 3);
  assert.deepEqual(s.new, ["b", "c"]);
  assert.deepEqual(s.fixed, ["z"]);
  assert.deepEqual(s.newHighCritical, ["b"]);
  assert.equal(line(s), "semgrep: new 2 / fixed 1 / total 3 (critical 1, high 1, medium 0, low 1)");
});

test("summarize: no previous run reports everything as new", () => {
  assert.equal(summarize("gitleaks", [{ id: "x", severity: "high" }], undefined).new.length, 1);
});

test("run: a scanner whose report is missing is `not run`, never zero findings", () => {
  const dir = mkdtempSync(join(tmpdir(), "sw-"));
  writeFileSync(join(dir, "gitleaks.json"), "[]");
  const { lines, summaries } = run(dir, undefined);
  assert.ok(lines.includes("gitleaks: new 0 / fixed 0 / total 0 (critical 0, high 0, medium 0, low 0)"));
  assert.ok(lines.includes("semgrep: NOT RUN (no report)"));
  assert.equal(summaries.find((s) => s.scanner === "semgrep").notRun, true);
});

test("run: an unreadable report is `not run`, never zero findings", () => {
  const dir = mkdtempSync(join(tmpdir(), "sw-"));
  writeFileSync(join(dir, "semgrep.json"), "{not json");
  assert.ok(run(dir, undefined).lines.includes("semgrep: NOT RUN (unreadable report)"));
});
