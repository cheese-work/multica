import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";
import { normalize, summarize, line, run } from "./summarize.mjs";

// A throwaway checkout: finding ids hash the flagged source text, so the files must exist.
const repo = (files) => {
  const root = mkdtempSync(join(tmpdir(), "sw-repo-"));
  for (const [path, text] of Object.entries(files)) {
    mkdirSync(dirname(join(root, path)), { recursive: true });
    writeFileSync(join(root, path), text);
  }
  return root;
};

test("semgrep: ERROR is high, WARNING medium, INFO low; id is rule|path|content hash, never the line", () => {
  const root = repo({ "a.go": "bad()\n", "b.ts": "\nbad()\n", "c.py": "bad()\n" });
  const raw = { results: [
    { check_id: "r.a", path: "a.go", start: { line: 1 }, end: { line: 1 }, extra: { severity: "ERROR" } },
    { check_id: "r.b", path: "b.ts", start: { line: 2 }, end: { line: 2 }, extra: { severity: "WARNING" } },
    { check_id: "r.c", path: "c.py", start: { line: 1 }, end: { line: 1 }, extra: { severity: "INFO" } },
  ] };
  const out = normalize("semgrep", raw, root);
  assert.deepEqual(out.map((i) => i.severity).sort(), ["high", "low", "medium"]);
  const a = out.find((i) => i.id.startsWith("r.a|"));
  assert.match(a.id, /^r\.a\|a\.go\|[0-9a-f]{12}#1$/);
  assert.equal(a.id.split("|")[2], out.find((i) => i.id.startsWith("r.b|")).id.split("|")[2]); // same text, same hash
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

test("summarize: findings sharing an id count once, so new never exceeds total", () => {
  const s = summarize("semgrep", [
    { id: "r.a|a.go", severity: "high" },
    { id: "r.a|a.go", severity: "high" },
    { id: "r.b|b.go", severity: "low" },
  ], undefined);
  assert.equal(s.total, 2);
  assert.deepEqual(s.bySeverity, { critical: 0, high: 1, medium: 0, low: 1 });
  assert.equal(s.new.length, 2);
});

test("run: codeql with only one language, or trivy with only one image, is `not run`", () => {
  const dir = mkdtempSync(join(tmpdir(), "sw-"));
  const sarif = JSON.stringify({ runs: [{ tool: { driver: { rules: [] } }, results: [] }] });
  writeFileSync(join(dir, "codeql-javascript-typescript.sarif"), sarif);
  writeFileSync(join(dir, "trivy-backend.json"), JSON.stringify({ Results: [] }));
  const { lines } = run(dir, undefined);
  assert.ok(lines.includes("codeql: NOT RUN (missing codeql-go.sarif)"));
  assert.ok(lines.includes("trivy: NOT RUN (missing trivy-web.json)"));
});

const sg = (line, path = "a.go") => ({ check_id: "r.a", path, start: { line }, end: { line }, extra: { severity: "ERROR" } });
const cq = (line, uri = "a.go") => ({ ruleId: "go/sql", level: "error", locations: [{ physicalLocation: { artifactLocation: { uri }, region: { startLine: line } } }] });
const cqSarif = (...results) => ({ runs: [{ tool: { driver: { rules: [{ id: "go/sql", properties: { "security-severity": "8.0" } }] } }, results }] });
const text = (...lines) => lines.join("\n") + "\n";
const scan = (scanner, raw, root, prev) => summarize(scanner, normalize(scanner, raw, root), prev);

for (const [scanner, mk, wrap] of [["semgrep", sg, (...r) => ({ results: r })], ["codeql", cq, cqSarif]]) {
  test(`${scanner}: replacing an occurrence with a different one of the same rule in the same file is new + fixed`, () => {
    const was = scan(scanner, wrap(mk(2)), repo({ "a.go": text("x", "old(input)", "y") }), undefined);
    const now = scan(scanner, wrap(mk(3)), repo({ "a.go": text("x", "y", "other(input)") }), was);
    assert.equal(now.total, 1);
    assert.equal(now.new.length, 1);
    assert.equal(now.fixed.length, 1);
    assert.equal(now.newHighCritical.length, 1);
  });

  test(`${scanner}: a line shift or whitespace edit with the same flagged text changes nothing`, () => {
    const was = scan(scanner, wrap(mk(2)), repo({ "a.go": text("x", "old(input)") }), undefined);
    const now = scan(scanner, wrap(mk(5)), repo({ "a.go": text("a", "b", "c", "d", "    old(input)  ") }), was);
    assert.equal(now.new.length + now.fixed.length, 0);
    assert.equal(now.total, 1);
  });

  test(`${scanner}: an added occurrence is new and a removed one is fixed; identical text counts twice`, () => {
    const one = scan(scanner, wrap(mk(1)), repo({ "a.go": text("old(input)", "keep") }), undefined);
    const two = scan(scanner, wrap(mk(1), mk(3)), repo({ "a.go": text("old(input)", "keep", "extra(input)") }), one);
    assert.deepEqual([two.total, two.new.length, two.fixed.length, two.newHighCritical.length], [2, 1, 0, 1]);
    const back = scan(scanner, wrap(mk(1)), repo({ "a.go": text("old(input)", "keep") }), two);
    assert.deepEqual([back.total, back.new.length, back.fixed.length], [1, 0, 1]);
    const dup = scan(scanner, wrap(mk(1), mk(2)), repo({ "a.go": text("same(input)", "same(input)") }), undefined);
    assert.equal(dup.total, 2);
  });
}

test("semgrep: ids do not depend on report order", () => {
  const root = repo({ "a.go": text("one()", "two()") });
  assert.deepEqual(normalize("semgrep", { results: [sg(2), sg(1)] }, root).map((i) => i.id).sort(),
    normalize("semgrep", { results: [sg(1), sg(2)] }, root).map((i) => i.id).sort());
});

test("a flagged file that cannot be read still yields an id (rule|path), never a crash", () => {
  const ids = normalize("semgrep", { results: [sg(4, "gone.go")] }, repo({})).map((i) => i.id);
  assert.deepEqual(ids, ["r.a|gone.go|#1"]);
});
