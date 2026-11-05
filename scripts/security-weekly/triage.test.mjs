import assert from "node:assert/strict";
import test from "node:test";
import { summarize } from "./summarize.mjs";
import { plan, issueArgs } from "./triage.mjs";

const never = { path: () => false, pkg: () => false, commit: () => false };
const sum = (scanner, items, prev) => summarize(scanner, items, prev);
const prev = (scanner) => ({ scanner, ids: ["old|x#1"] });

test("summarize: new high/critical carry their severity and a first run is flagged baseline", () => {
  const first = sum("semgrep", [{ id: "r|a.go#1", severity: "high" }], undefined);
  assert.equal(first.baseline, true);
  const next = sum("semgrep", [
    { id: "r|a.go#1", severity: "high" }, { id: "m|b.go#1", severity: "medium" }, { id: "c|c.go#1", severity: "critical" },
  ], { scanner: "semgrep", ids: ["r|a.go#1"] });
  assert.equal(next.baseline, false);
  assert.deepEqual(next.newFindings, [{ id: "c|c.go#1", severity: "critical" }]);
});

test("plan: one entry per new high/critical finding; baseline and NOT RUN scanners file nothing", () => {
  const summaries = [
    sum("semgrep", [{ id: "r|a.go#1", severity: "high" }, { id: "r|a.go#2", severity: "high" }], prev("semgrep")),
    sum("codeql", [{ id: "q|a.go#1", severity: "high" }], undefined),
    { scanner: "trivy", notRun: true, why: "no report", ids: [] },
  ];
  const entries = plan(summaries, never);
  assert.deepEqual(entries.map((e) => `${e.scanner}:${e.id}`), ["semgrep:r|a.go#1", "semgrep:r|a.go#2"]);
});

test("plan: upstream-shared is decided per scanner kind", () => {
  const has = { path: (p) => p === "shared.go", pkg: (n) => n === "undici", commit: (c) => c === "abc123" };
  const hit = (scanner, id) => plan([sum(scanner, [{ id, severity: "high" }], prev(scanner))], has)[0].upstreamShared;
  assert.equal(hit("semgrep", "r|shared.go#1"), true);
  assert.equal(hit("semgrep", "r|ours.go#1"), false);
  assert.equal(hit("codeql", "q|shared.go#1"), true);
  assert.equal(hit("gitleaks", "abc123:ours.ts:generic-api-key:20"), true);
  assert.equal(hit("gitleaks", "def456:ours.ts:generic-api-key:20"), false);
  assert.equal(hit("pnpm-audit", "GHSA-1|undici"), true);
  assert.equal(hit("pnpm-audit", "GHSA-1|ours-only"), false);
  assert.equal(hit("trivy", "web (node)|CVE-1|undici"), true);
  assert.equal(hit("govulncheck", "GO-2026-1"), null);
});

test("issueArgs: assigned to the squad; upstream-shared is labelled private and never a public PR", () => {
  const [shared] = plan([sum("semgrep", [{ id: "r|a.go#1", severity: "critical" }], prev("semgrep"))], { ...never, path: () => true });
  const [ours] = plan([sum("semgrep", [{ id: "r|a.go#1", severity: "high" }], prev("semgrep"))], never);
  const a = issueArgs(shared, { assigneeId: "squad-1", project: "proj-1", descriptionFile: "d.md" });
  assert.deepEqual(a.slice(0, 3), ["issue", "create", "--title"]);
  assert.ok(a[3].startsWith("[upstream-private] "));
  assert.ok(a.includes("squad-1") && a.includes("proj-1") && a.includes("d.md"));
  assert.match(shared.body, /private/i);
  assert.match(shared.body, /never a public upstream/i);
  assert.ok(!issueArgs(ours, { assigneeId: "s", project: "p", descriptionFile: "d.md" })[3].includes("[upstream-private]"));
  assert.doesNotMatch(ours.body, /upstream/i);
});

test("plan: when upstream cannot be read every lookup is unknown, never `not shared`", () => {
  const unknown = { path: () => null, pkg: () => null, commit: () => null };
  const [e] = plan([sum("semgrep", [{ id: "r|a.go#1", severity: "high" }], prev("semgrep"))], unknown);
  assert.equal(e.upstreamShared, null);
});
