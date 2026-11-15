import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

// CHE-3955: the mirror pushes upstream history, which contains .github/workflows files.
// GITHUB_TOKEN can never hold `workflows` permission, so the push uses a c00-multicado
// App token. The App key lives only in the `ci-app-token` environment (branch policy: main).
const wf = readFileSync(new URL("../.github/workflows/mirror-upstream-main.yml", import.meta.url), "utf8");

test("the mirror job holds the ci-app-token environment", () => {
  assert.match(wf, /^\s+environment: ci-app-token\s*$/m);
});

test("the mirror mints a pinned App token with contents and workflows write on this repo only", () => {
  assert.match(wf, /uses: actions\/create-github-app-token@[0-9a-f]{40}/);
  assert.match(wf, /app-id: \$\{\{ secrets\.C00_MULTICADO_APP_ID \}\}/);
  assert.match(wf, /private-key: \$\{\{ secrets\.C00_MULTICADO_CI_PRIVATE_KEY \}\}/);
  assert.match(wf, /repositories: multica\s*$/m);
  assert.match(wf, /permission-contents: write/);
  assert.match(wf, /permission-workflows: write/);
  assert.doesNotMatch(wf, /continue-on-error/, "a failed mint must fail the run, not fall back");
});

test("the push never uses GITHUB_TOKEN and the job token is read-only", () => {
  assert.doesNotMatch(wf, /github\.token|secrets\.GITHUB_TOKEN/);
  assert.match(wf, /steps\.app\.outputs\.token/);
  assert.doesNotMatch(wf, /^\s*contents: write/m, "job token must not write contents");
  assert.match(wf, /^permissions:\s*\n\s+contents: read\s*$/m);
});
