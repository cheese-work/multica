import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

// The merge queue fast-forwards main to the SHA the merge-group run already tested, so a `CI`
// push run repeats it. Post-merge work lives in main-build.yml (CHE-3325).
const read = (name) => readFileSync(new URL(`../.github/workflows/${name}`, import.meta.url), "utf8");
const ci = read("ci.yml");
const mainBuild = existsSync(new URL("../.github/workflows/main-build.yml", import.meta.url)) ? read("main-build.yml") : "";
const jobIds = (text) => [...text.slice(text.indexOf("\njobs:\n")).matchAll(/^  ([\w-]+):$/gm)].map((match) => match[1]);
const MOVED = ["push-go-cache-warm", "push-turbo-cache-warm", "daemon-artifact"];

test("ci.yml has no push trigger and keeps the PR, merge-queue, nightly and manual triggers", () => {
  const triggers = ci.slice(0, ci.indexOf("\nconcurrency:"));
  assert.doesNotMatch(triggers, /^  push:/m);
  for (const trigger of ["pull_request", "merge_group", "schedule", "workflow_dispatch"]) {
    assert.match(triggers, new RegExp(`^  ${trigger}:`, "m"));
  }
});

test("main-build.yml runs on push to main, one at a time, and holds the moved jobs", () => {
  assert.match(mainBuild, /^on:\n  push:\n    branches: \[main\]\n/m);
  assert.doesNotMatch(mainBuild.slice(0, mainBuild.indexOf("\njobs:")), /pull_request|merge_group/);
  assert.match(mainBuild, /^concurrency:\n  group: main-build-\$\{\{ github\.ref \}\}\n  cancel-in-progress: false$/m);
  for (const job of MOVED) {
    assert.ok(jobIds(mainBuild).includes(job), `${job} missing from main-build.yml`);
    assert.ok(!jobIds(ci).includes(job), `${job} still in ci.yml`);
  }
  assert.doesNotMatch(ci.match(/^  warning-budget:\n    needs: \[(.+)\]$/m)[1], /daemon-artifact|cache-warm/);
});

test("cache warm jobs wait for the merge-group signal and the daemon artifact builds both architectures", () => {
  const jobsSource = mainBuild.slice(mainBuild.indexOf("\njobs:\n") + 1);
  const headings = [...jobsSource.matchAll(/^  ([\w-]+):$/gm)];
  const section = (id) => {
    const index = headings.findIndex((match) => match[1] === id);
    return index < 0 ? "" : jobsSource.slice(headings[index].index, headings[index + 1]?.index);
  };
  for (const job of ["push-go-cache-warm", "push-turbo-cache-warm"]) {
    assert.match(section(job), /needs: merge-group\n/);
    assert.match(section(job), /if: \$\{\{ needs\.merge-group\.outputs\.merge_group_green == 'true' \}\}/);
  }
  assert.match(mainBuild, /^  merge-group:\n(?:.*\n)*?    outputs:\n      merge_group_green: \$\{\{ steps\.green\.outputs\.merge_group_green \}\}$/m);
  assert.match(mainBuild, /run: node scripts\/ci-scope\.mjs merge-group-green/);
  assert.match(mainBuild, /GH_TOKEN: \$\{\{ github\.token \}\}/);
  assert.match(mainBuild, /CI_PUSH_FULL: \$\{\{ vars\.CI_PUSH_FULL \}\}/);
  assert.match(mainBuild, /multica-daemon-linux-\$\{\{ matrix\.goarch \}\}/);
  assert.match(mainBuild, /goarch: \[amd64, arm64\]/);
});

test("merge-group-green CLI reports the merge-group signal and fails closed on lookup errors or the kill switch", (context) => {
  const directory = mkdtempSync(join(tmpdir(), "main-build-green-"));
  context.after(() => rmSync(directory, { recursive: true, force: true }));
  const output = join(directory, "outputs");
  const fixture = join(directory, "runs.json");
  const command = fileURLToPath(new URL("./ci-scope.mjs", import.meta.url));
  const sha = "a".repeat(40);
  const green = { event: "merge_group", head_sha: sha, status: "completed", conclusion: "success", head_repository: { full_name: "cheese-work/multica" } };
  writeFileSync(join(directory, "gh"), `#!/usr/bin/env node
const {readFileSync} = require('node:fs');
if (process.env.LOOKUP_FAIL === 'true') process.exit(1);
process.stdout.write(readFileSync(process.env.RUN_FIXTURE));
`, { mode: 0o700 });
  // Build the step env from the workflow itself: the runner sets GITHUB_* only, so a variable the
  // script reads (EVENT_NAME) must come from the step's own `env:` block.
  const step = mainBuild.slice(mainBuild.indexOf("id: green"), mainBuild.indexOf("run: node scripts/ci-scope.mjs merge-group-green"));
  const expressions = { "github.event_name": "push", "github.token": "token", "vars.CI_PUSH_FULL": "" };
  const stepEnv = (forceFull) => Object.fromEntries([...step.matchAll(/^ {10}(\w+): \$\{\{ ([\w.]+) \}\}$/gm)].map(([, key, expression]) => [
    key, key === "CI_PUSH_FULL" ? forceFull : expressions[expression],
  ]));
  for (const [runs, forceFull, failure, expected] of [
    [[green], "", "", "true"], [[], "", "", "false"],
    [[green], "true", "", "false"], [[green], "", "true", "false"],
  ]) {
    writeFileSync(fixture, JSON.stringify({ workflow_runs: runs }));
    writeFileSync(output, "");
    const result = spawnSync(process.execPath, [command, "merge-group-green"], {
      encoding: "utf8",
      env: { ...process.env, MERGE_GROUP_WAIT_MS: "0", PATH: `${directory}:${process.env.PATH}`, RUN_FIXTURE: fixture, LOOKUP_FAIL: failure,
        ...stepEnv(forceFull), GITHUB_REF: "refs/heads/main", GITHUB_SHA: sha,
        GITHUB_REPOSITORY: "cheese-work/multica", GITHUB_OUTPUT: output },
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(readFileSync(output, "utf8"), `merge_group_green=${expected}\n`);
  }
});
