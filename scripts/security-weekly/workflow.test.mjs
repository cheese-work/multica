import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";

// CHE-3701: static checks on the weekly full-tree scan. A missing file reads as empty so a failing test names the file.
const read = (path) => (existsSync(new URL(`../../${path}`, import.meta.url)) ? readFileSync(new URL(`../../${path}`, import.meta.url), "utf8") : "");
const wf = read(".github/workflows/security-weekly.yml");

test("runs Sunday 23:00 ICT (16:00 UTC) and on demand", () => {
  assert.match(wf, /cron: "0 16 \* \* 0"/);
  assert.match(wf, /^  workflow_dispatch:/m);
});

test("token stays read-only: no write permission anywhere, no secrets", () => {
  assert.match(wf, /^permissions:\n  contents: read\n/m);
  assert.doesNotMatch(wf, /:\s*write\b/);
  assert.doesNotMatch(wf, /secrets\./);
});

test("every job runs on a GitHub-hosted runner and nothing builds an image", () => {
  const lines = wf.split("\n").filter((l) => /^\s+runs-on:/.test(l));
  assert.ok(lines.length >= 6, "semgrep, govulncheck, pnpm-audit, gitleaks, codeql, image-scan");
  for (const l of lines) assert.match(l, /runs-on: ubuntu-24\.04\s*$/, l.trim());
  assert.doesNotMatch(wf, /docker (buildx )?build|docker\/build-push-action|cheese-x99|self-hosted/);
});

test("every checkout drops the token", () => {
  const checkouts = wf.match(/uses: actions\/checkout@/g) ?? [];
  assert.ok(checkouts.length > 0);
  assert.equal((wf.match(/persist-credentials: false/g) ?? []).length, checkouts.length);
});

test("semgrep uses the same rule packs as security-gate", () => {
  const gate = read("scripts/security-gate/security_gate.py").match(/SEMGREP_CONFIGS = \(([^)]*)\)/)[1];
  const packs = [...gate.matchAll(/"([^"]+)"/g)].map((m) => m[1]);
  assert.ok(packs.length > 0);
  for (const p of packs) assert.match(wf, new RegExp(`--config ${p.replace("/", "\\/")}\\b`), p);
});

test("each scanner writes the report name summarize.mjs reads", () => {
  for (const name of ["semgrep.json", "govulncheck.txt", "pnpm-audit.json", "gitleaks.json", "codeql-", "trivy-"]) {
    assert.ok(wf.includes(name), name);
  }
});

test("downloaded scanner binaries are checksum-pinned", () => {
  assert.equal((wf.match(/sha256sum -c/g) ?? []).length, 2, "gitleaks and trivy");
});

test("CodeQL Go builds (autobuild); only javascript-typescript may use build-mode none", () => {
  const job = wf.slice(wf.indexOf("  codeql:"), wf.indexOf("  image-scan:"));
  assert.match(job, /language: go\s+build-mode: autobuild/);
  assert.match(job, /language: javascript-typescript\s+build-mode: none/);
  assert.match(job, /build-mode: \$\{\{ matrix\.build-mode \}\}/);
  assert.match(job, /if: matrix\.language == 'go'[\s\S]*setup-go/);
});

// Luna's FAIL on b7242fd47: this repo is public, so every artifact and step summary is world-readable. Finding detail
// (ids, severities, upstream-shared labels) must not appear before the private upstream report.
const uploads = [...wf.matchAll(/uses: actions\/upload-artifact@[^\n]*\n((?:\s+[a-z-]+:[^\n]*\n)+)/g)].map((m) => m[1]);

test("every uploaded artifact is sealed: path is *.gpg only", () => {
  assert.ok(uploads.length >= 6, "one per scanner job");
  for (const u of uploads) assert.match(u, /path: [^\n]*\.gpg\s*\n/, u);
  assert.doesNotMatch(wf, /path: out\/|weekly-summary|triage\.json/);
});

test("every scanner job seals its report before upload", () => {
  const seals = wf.match(/bash scripts\/security-weekly\/seal\.sh /g) ?? [];
  assert.equal(seals.length, uploads.length);
});

test("no finding detail reaches a step summary or the log", () => {
  assert.doesNotMatch(wf, /GITHUB_STEP_SUMMARY[^\n]*(lines\.txt|newHighCritical|triage|summary\.json)/);
  assert.doesNotMatch(wf, /summarize\.mjs|triage\.mjs/, "summary and triage run on X99 after unsealing");
});

test("the recipient file is a PGP public key, never a private one", () => {
  const key = read("scripts/security-weekly/recipient.asc");
  assert.match(key, /^-----BEGIN PGP PUBLIC KEY BLOCK-----/);
  assert.doesNotMatch(key, /PRIVATE/);
});

test("filing issues needs credentials the scan must not hold: no apply step in the workflow", () => {
  assert.doesNotMatch(wf, /--apply|multica issue create/);
});

test("every job that seals checks out the repo first (seal.sh lives in the tree)", () => {
  const jobs = wf.split(/^  (?=[a-z-]+:\n    runs-on:)/m).slice(1);
  assert.ok(jobs.length >= 6);
  for (const j of jobs) {
    if (j.includes("seal.sh")) assert.match(j, /uses: actions\/checkout@/, j.split("\n")[0]);
  }
});
