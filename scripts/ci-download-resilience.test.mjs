import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-4877: golangci-lint and gitleaks downloads returned HTTP 504 and failed unrelated PRs out of the merge queue.
// Both tools now come through scripts/fetch-verified.sh (5xx retry, sha256 check) with the tarball cached.
const read = (name) => readFileSync(new URL(`../${name}`, import.meta.url), "utf8");
const ci = read(".github/workflows/ci.yml");
const job = (text, name) => {
  const start = text.indexOf(`\n  ${name}:`);
  assert.ok(start >= 0, `job ${name} exists`);
  const next = text.slice(start + 1).search(/\n  [a-z0-9-]+:\n/);
  return next < 0 ? text.slice(start) : text.slice(start, start + 1 + next);
};

test("go-lint installs a pinned, checksum-verified, cached golangci-lint instead of the action's download", () => {
  const goLint = job(ci, "go-lint");
  assert.doesNotMatch(goLint, /golangci\/golangci-lint-action/);
  assert.match(goLint, /uses: actions\/cache@/);
  assert.match(goLint, /scripts\/fetch-verified\.sh/);
  assert.match(goLint, /GOLANGCI_LINT_VERSION: "?2\.13\.2"?/);
  assert.match(goLint, /GOLANGCI_LINT_SHA256: "?2277d43b98ec0054280f2ac26b53268bae97682444678a59a657dd565da021d6"?/);
  assert.match(goLint, /--new-from-merge-base="origin\/\$BASE_REF"/);
});

for (const [file, name] of [[".github/workflows/ci.yml", "gitleaks"], [".github/workflows/secret-scan.yml", "gitleaks"]]) {
  test(`${file} caches the gitleaks download`, () => {
    const g = job(read(file), name);
    assert.match(g, /uses: actions\/cache@/);
    assert.match(g, /path: \$\{\{ runner\.temp \}\}\/gitleaks-/);
    assert.match(g, /hashFiles\('scripts\/secret-scan\.sh'\)/);
  });
}

test("secret-scan.sh downloads through fetch-verified.sh, not bare curl", () => {
  const s = read("scripts/secret-scan.sh");
  assert.match(s, /fetch-verified\.sh/);
  assert.doesNotMatch(s, /\bcurl\b.*-o/);
});

test("ci.yml runs the fetch-verified and download-resilience tests", () => {
  assert.match(ci, /bash scripts\/fetch-verified\.test\.sh/);
  assert.match(ci, /scripts\/ci-download-resilience\.test\.mjs/);
});
