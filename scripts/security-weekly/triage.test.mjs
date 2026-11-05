import assert from "node:assert/strict";
import test from "node:test";
import { summarize } from "./summarize.mjs";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { plan, issueArgs, apply } from "./triage.mjs";

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

test("plan: one entry per new high/critical finding; a NOT RUN scanner files nothing", () => {
  const summaries = [
    sum("semgrep", [{ id: "r|a.go#1", severity: "high" }, { id: "r|a.go#2", severity: "high" }], prev("semgrep")),
    { scanner: "trivy", notRun: true, why: "no report", ids: [] },
  ];
  const entries = plan(summaries, never);
  assert.deepEqual(entries.map((e) => `${e.scanner}:${e.id}`), ["semgrep:r|a.go#1", "semgrep:r|a.go#2"]);
  assert.ok(entries.every((e) => e.baseline === false));
});

test("plan: the first scheduled run (no previous summary) files its high/critical findings, marked baseline", () => {
  const entries = plan([sum("codeql", [{ id: "q|a.go|h#1", severity: "high" }, { id: "q|a.go|l#1", severity: "low" }], undefined)], never);
  assert.deepEqual(entries.map((e) => `${e.scanner}:${e.id}:${e.baseline}`), ["codeql:q|a.go|h#1:true"]);
});

test("issueArgs: a baseline finding is parked in backlog (no agent run per finding); a later new one is todo", () => {
  const status = (baseline) => { const a = issueArgs({ ...entry("semgrep", "a", "high", false), baseline }, { assigneeId: "s", project: "p", descriptionFile: "d.md" }); return a[a.indexOf("--status") + 1]; };
  assert.equal(status(true), "backlog");
  assert.equal(status(false), "todo");
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

const entry = (scanner, id, severity, shared) => ({ scanner, id, severity, upstreamShared: shared, title: `${scanner} ${severity}: ${id}`, body: `body of ${id}` });

test("apply: files every entry, a failing create does not stop the rest, and the failure is returned", () => {
  const seen = [];
  const run = (args) => {
    const body = readFileSync(args[args.indexOf("--description-file") + 1], "utf8");
    seen.push({ title: args[args.indexOf("--title") + 1], body });
    if (body === "body of a") throw new Error("duplicate");
  };
  const failed = apply([entry("semgrep", "a", "high", false), entry("codeql", "b", "critical", true)], { assigneeId: "squad", project: "proj", run });
  assert.deepEqual(seen.map((x) => x.title), ["Security weekly: semgrep high: a", "[upstream-private] Security weekly: codeql critical: b"]);
  assert.deepEqual(seen.map((x) => x.body), ["body of a", "body of b"]);
  assert.deepEqual(failed.map((e) => e.id), ["a"]);
});

test("issueArgs: created ready, so the squad picks it up without a triage step", () => {
  const a = issueArgs(entry("semgrep", "a", "high", false), { assigneeId: "s", project: "p", descriptionFile: "d.md" });
  assert.equal(a[a.indexOf("--property") + 1], "Readiness=ready");
});

test("CLI end to end: summary.json -> triage.json -> `multica issue create` per new high/critical, upstream-private titled", () => {
  const dir = mkdtempSync(join(tmpdir(), "sw-e2e-"));
  const bin = join(dir, "bin");
  mkdirSync(bin);
  const log = join(dir, "calls.log");
  writeFileSync(join(bin, "multica"), `#!/bin/sh\nprintf '%s\\n' "$*" >> ${log}\n`);
  chmodSync(join(bin, "multica"), 0o755);
  const summary = { summaries: [
    { ...sum("semgrep", [{ id: "r|ours.go|h#1", severity: "high" }], prev("semgrep")) },
    { ...sum("gitleaks", [{ id: "abc123:x.ts:k:1", severity: "high" }], prev("gitleaks")) },
    { ...sum("trivy", [{ id: "web|CVE-1|zlib", severity: "critical" }], undefined) }, // first scheduled run for this scanner
  ] };
  writeFileSync(join(dir, "summary.json"), JSON.stringify(summary));
  const here = new URL("./triage.mjs", import.meta.url).pathname;
  const env = { ...process.env, PATH: `${bin}:${process.env.PATH}` };
  const planned = spawnSync("node", [here, join(dir, "summary.json"), join(dir, "triage.json")], { cwd: dir, env, encoding: "utf8" });
  assert.equal(planned.status, 0, planned.stderr);
  const filed = spawnSync("node", [here, "--apply", join(dir, "triage.json"), "squad-1", "proj-1"], { cwd: dir, env, encoding: "utf8" });
  assert.equal(filed.status, 0, filed.stderr);
  const calls = readFileSync(log, "utf8").trim().split("\n");
  assert.equal(calls.length, 3);
  assert.equal(calls.filter((c) => c.includes("--status backlog")).length, 1);
  assert.ok(calls.find((c) => c.includes("CVE-1")).includes("--status backlog"));
  assert.ok(calls.every((c) => c.startsWith("issue create ") && c.includes("--assignee-id squad-1") && c.includes("--project proj-1")));
});
