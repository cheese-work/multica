import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";

// CHE-3701: static checks on the weekly full-tree scan. A missing file reads as empty so a failing test names the file.
const read = (path) => (existsSync(new URL(`../../${path}`, import.meta.url)) ? readFileSync(new URL(`../../${path}`, import.meta.url), "utf8") : "");
const wf = read(".github/workflows/security-weekly.yml");

// CHE-4691: minute 0 is the busiest slot on GitHub, and the first 23:00 run never started. 23:23 ICT is 16:23 UTC.
test("runs Sunday 23:23 ICT (16:23 UTC), off the top of the hour", () => {
  assert.match(wf, /cron: "23 16 \* \* 0" # Sunday 23:23 ICT/);
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

// Luna FAIL on 7adaba3d6: Go autobuild runs the root Makefile, which CODEOWNERS does not cover, before the SARIF is sealed.
// CHE-4691: the first scheduled run failed because CodeQL does not support build-mode none for Go (JavaScript/TypeScript does).
// Go uses build-mode manual with one explicit `go build ./...` in server/, so no Makefile runs.
test("CodeQL: Go builds manually with one explicit go build (CodeQL rejects none for Go); no autobuild, no setup-go, no make", () => {
  const job = wf.slice(wf.indexOf("  codeql:"), wf.indexOf("  image-scan:"));
  assert.match(job, /language: go\s+build-mode: manual/);
  assert.doesNotMatch(job, /language: go\s+build-mode: none/);
  assert.match(job, /language: javascript-typescript\s+build-mode: none/);
  assert.match(job, /build-mode: \$\{\{ matrix\.build-mode \}\}/);
  assert.match(job, /if: matrix\.language == 'go'\n\s+working-directory: server\n\s+run: go build \.\/\.\.\.\n/);
  assert.doesNotMatch(job, /autobuild|setup-go|\bmake\b/);
  // the build step sits between init and analyze, or CodeQL traces nothing
  assert.ok(job.indexOf("codeql-action/init@") < job.indexOf("go build ./...") && job.indexOf("go build ./...") < job.indexOf("codeql-action/analyze@"));
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
  const jobs = wf.split(/^  (?=[a-z-]+:\n    (?:if: [^\n]*\n    )?runs-on:)/m).slice(1);
  assert.ok(jobs.length >= 6);
  for (const j of jobs) {
    if (j.includes("seal.sh")) assert.match(j, /uses: actions\/checkout@/, j.split("\n")[0]);
  }
});

test("never runs on a pull request: a PR could swap recipient.asc and read the sealed reports", () => {
  assert.doesNotMatch(wf, /^\s*pull_request(_target)?:/m);
  assert.match(wf, /^on:\n  schedule:/m);
});

// Terra and luna FAIL on f3b0150f8: workflow_dispatch lets a writer pick a branch (gh workflow run --ref), and the run uses that
// branch's own file and recipient.asc. CHE-4691 re-adds it behind a per-job guard so the baseline run can be started: the guard
// stops a mistaken dispatch, not an author who edits the file. CODEOWNERS on this file, seal.sh and the key is the real control.
test("triggers are schedule and workflow_dispatch only", () => {
  assert.match(wf, /^on:\n  schedule:\n    - cron: "[^"]+"[^\n]*\n  workflow_dispatch:\n\n/m);
  assert.doesNotMatch(wf, /^\s*(workflow_call|push|repository_dispatch|workflow_run):/m);
});

test("every job runs on main only: a dispatch from another ref skips them all", () => {
  const jobs = wf.split(/^  (?=[a-z-]+:\n    (?:if: [^\n]*\n    )?runs-on:)/m).slice(1);
  assert.equal(jobs.length, 6, "semgrep, govulncheck, pnpm-audit, gitleaks, codeql, image-scan");
  for (const j of jobs) assert.match(j, /^[a-z-]+:\n    if: github\.ref == 'refs\/heads\/main'\n    runs-on:/, j.split("\n")[0]);
});

test("nothing is written to a step summary", () => {
  assert.doesNotMatch(wf, /GITHUB_STEP_SUMMARY/);
});

// Luna's FAIL on cc18ed4c0: the encryption recipient must bind to the X99 key, not to whatever recipient.asc a PR ships.
test("seal pins the X99 key fingerprint, which matches the shipped recipient.asc", () => {
  const pin = wf.match(/^  SECURITY_WEEKLY_FPR: ([0-9A-F]{40})\s*$/m)?.[1];
  assert.ok(pin, "workflow env pins SECURITY_WEEKLY_FPR as a literal");
  const key = read("scripts/security-weekly/recipient.asc");
  const out = execFileSync("gpg", ["--batch", "--with-colons", "--show-keys"], { input: key, encoding: "utf8" });
  assert.equal(out.match(/^fpr:+([0-9A-F]{40}):/m)[1], pin);
  assert.match(read("scripts/security-weekly/seal.sh"), /SECURITY_WEEKLY_FPR/);
});

test("CODEOWNERS makes the owner review any change to the recipient, the seal script or the workflow", () => {
  const owners = read(".github/CODEOWNERS");
  for (const p of ["/scripts/security-weekly/", "/.github/workflows/security-weekly.yml"]) {
    assert.match(owners, new RegExp(`^${p.replace(/[.]/g, "\\.")}\\s+@congvc-dev`, "m"), p);
  }
});

// Terra's FAIL on 8dfebb045: a merged change to any script the scheduled job runs before sealing could expose the plaintext report.
test("the only checkout script a job runs is seal.sh (no setup-pnpm.sh: pnpm comes straight from npm)", () => {
  const scripts = [...wf.matchAll(/\bbash (scripts\/[^\s]+)/g)].map((m) => m[1]);
  assert.ok(scripts.length > 0);
  for (const s of scripts) assert.equal(s, "scripts/security-weekly/seal.sh", s);
  assert.match(wf, /npm install --prefix "\$RUNNER_TEMP\/pnpm" "pnpm@/);
});

// Luna medium on 7adaba3d6: package.json is unowned, so its `packageManager` field must not choose the pnpm version the job runs.
test("the pnpm version is pinned in the owner-protected workflow, not read from package.json", () => {
  assert.match(wf, /PNPM_VERSION: "\d+\.\d+\.\d+"/);
  assert.match(wf, /"pnpm@\$\{PNPM_VERSION\}"/);
  assert.doesNotMatch(wf, /require\("\.\/package\.json"\)|packageManager/);
});

// Luna medium on 7adaba3d6: a root .gitleaksignore or .gitleaks.toml is unowned and would drop findings before sealing. Gitleaks reads
// both from owner-protected paths only.
test("gitleaks reads its ignore list and config from scripts/security-weekly, not the repo root", () => {
  assert.match(wf, /gitleaks" git [^\n]*--gitleaks-ignore-path scripts\/security-weekly /);
  assert.match(wf, /gitleaks" git [^\n]*--config scripts\/security-weekly\/gitleaks\.toml /);
  assert.ok(existsSync(new URL("./.gitleaksignore", import.meta.url)));
  assert.ok(existsSync(new URL("./gitleaks.toml", import.meta.url)));
  assert.ok(!existsSync(new URL("../../.gitleaksignore", import.meta.url)), "root .gitleaksignore moved to the owned path");
  // Gitleaks 8.30.1 still reads <source>/.gitleaksignore despite --gitleaks-ignore-path (verified on X99), so the job deletes it first.
  assert.match(wf, /rm -f \.gitleaksignore\n[\s\S]*gitleaks" git /);
});

test("CODEOWNERS routes the whole security-weekly toolchain to the owner", () => {
  assert.match(read(".github/CODEOWNERS"), /^\/scripts\/security-weekly\/\s+@congvc-dev/m);
});

// Terra and luna FAIL on 517fb9e60: `go tool govulncheck` resolves the scanner through server/go.mod, which CODEOWNERS does not
// cover. A merged `replace` there would run substituted code before the report is sealed. The scanner is installed at a pinned
// version from outside any module, so the checkout cannot choose which code runs.
test("govulncheck is installed at a pinned version outside the checkout, never via go tool or the checkout's go.mod", () => {
  assert.doesNotMatch(wf, /go tool\b|go run\b/);
  assert.match(wf, /GOVULNCHECK_VERSION: "v\d+\.\d+\.\d+"/);
  assert.match(wf, /working-directory: \$\{\{ runner\.temp \}\}\n\s+env:\n\s+GOFLAGS: ""\n\s+GOWORK: "off"\n\s+run: go install golang\.org\/x\/vuln\/cmd\/govulncheck@\$\{GOVULNCHECK_VERSION\}/);
  assert.match(wf, /"\$\(go env GOPATH\)\/bin\/govulncheck" \.\/\.\.\./);
});

// Terra FAIL on 62a4a3c11: `pnpm audit` run in the checkout loads a root .pnpmfile.cjs, which CODEOWNERS does not cover. A merged
// hook would run before the report is sealed. The audit runs in a scratch directory holding only the lockfile and a stub manifest,
// so no checkout-controlled pnpm config, hook or workspace file is read.
test("pnpm audit runs in a scratch directory with only the lockfile, never in the checkout", () => {
  const block = wf.slice(wf.indexOf("  pnpm-audit:"), wf.indexOf("  gitleaks:"));
  assert.match(block, /audit_dir="\$RUNNER_TEMP\/pnpm-audit"/);
  assert.match(block, /cp pnpm-lock\.yaml "\$audit_dir\/"/);
  assert.match(block, /cd "\$audit_dir"/);
  assert.match(block, /pnpm audit --json > "\$RUNNER_TEMP\/reports\/pnpm-audit\.json"/);
  assert.doesNotMatch(block, /(^|\n)\s+pnpm audit --json > pnpm-audit\.json/);
  assert.doesNotMatch(block, /cp [^\n]*(package\.json|\.npmrc|pnpm-workspace)/, "no checkout config enters the scratch directory");
});

// Terra FAIL on 1243d6ecd: `python3 -m venv` run from the checkout puts the cwd first on sys.path, so a merged root venv.py
// (unowned) would run before sealing. -I drops the cwd and PYTHONPATH from sys.path.
test("python never imports from the checkout: the venv is built with python3 -I", () => {
  const pythons = [...wf.matchAll(/^\s+(?:[^#\n]*\s)?python3? [^\n]*/gm)].map((m) => m[0].trim()).filter((l) => !l.startsWith("#") && !l.includes("--config")).map((l) => l.slice(l.search(/python3? /)));
  assert.ok(pythons.length > 0);
  for (const p of pythons) assert.match(p, /^python3 -I /, p);
});

// Terra FAIL on 1243d6ecd: a committed symlink named like a report (semgrep.json -> /dev/stdout) would redirect plaintext into the
// public Actions log. Every report is written, sealed and uploaded from a fresh directory under RUNNER_TEMP, outside the checkout.
test("every report is written, sealed and uploaded from $RUNNER_TEMP/reports, never from the checkout", () => {
  const sealArgs = [...wf.matchAll(/\bbash scripts\/security-weekly\/seal\.sh ([^\n]+)/g)].map((m) => m[1]);
  assert.ok(sealArgs.length >= 6);
  for (const a of sealArgs) assert.match(a, /^"\$RUNNER_TEMP"\/reports\//, a);
  for (const u of uploads) assert.match(u, /path: \$\{\{ runner\.temp \}\}\/reports\//, u);
  const mkdirs = wf.match(/mkdir "\$RUNNER_TEMP\/reports"/g) ?? [];
  assert.equal(mkdirs.length, uploads.length, "each job makes its own reports directory");
  assert.doesNotMatch(wf, />\s*(\.\.\/)?(semgrep|govulncheck|gitleaks)\.(json|txt)\b/);
  assert.doesNotMatch(wf, /--report-path gitleaks\.json|--output trivy-|output: codeql-out|GITHUB_WORKSPACE\/pnpm-audit|--dir evidence/);
});

// Terra non-blocking note on 1243d6ecd: a root .semgrepignore is unowned and drops files from the scan before sealing.
test("semgrep deletes a root .semgrepignore before scanning", () => {
  assert.match(wf, /rm -f \.semgrepignore\n[\s\S]*semgrep" scan /);
});

// Luna medium on 91dbba8ed: Trivy reads <cwd>/.trivyignore by default, and a root .trivyignore is unowned, so it could hide image CVEs
// before sealing. Terra and Luna notes: Gitleaks inline `gitleaks:allow` in unowned source also drops findings.
test("trivy reads its ignore file from scripts/security-weekly, not the repo root", () => {
  assert.match(wf, /trivy" image [^\n]*--ignorefile scripts\/security-weekly\/\.trivyignore /);
  assert.ok(existsSync(new URL("./.trivyignore", import.meta.url)));
  assert.ok(!existsSync(new URL("../../.trivyignore", import.meta.url)));
  assert.equal((wf.match(/trivy" image /g) ?? []).length, (wf.match(/trivy" image [^\n]*--ignorefile /g) ?? []).length, "every trivy call");
  assert.match(wf, /rm -f \.trivyignore\n[\s\S]*trivy" image /);
});

test("gitleaks ignores inline gitleaks:allow comments", () => {
  assert.match(wf, /gitleaks" git [^\n]*--ignore-gitleaks-allow /);
});
