import assert from "node:assert/strict"
import { readFileSync, readdirSync } from "node:fs"
import { test } from "node:test"

const workflow = (name) => readFileSync(new URL(`../.github/workflows/${name}`, import.meta.url), "utf8")

test("workflow Ubuntu labels are pinned instead of silently migrating", () => {
  for (const name of readdirSync(new URL("../.github/workflows/", import.meta.url))) {
    if (name.endsWith(".yml")) assert.doesNotMatch(workflow(name).replace(/#.*/g, ""), /ubuntu-latest/, name)
  }
})

test("CD uses Node 24 actions and never asks setup-node to infer a package cache", () => {
  for (const name of ["cd-qualification.yml", "cd-deploy.yml"]) {
    const source = workflow(name)
    assert.doesNotMatch(source, /actions\/(?:upload-artifact|download-artifact|setup-node)@v4|docker\/(?:login|setup-buildx)-action@v3/)
    const setups = source.match(/uses: actions\/setup-node@v7\n        with:\n(?:          .*\n)+/g)
    assert.ok(setups?.length)
    for (const setup of setups) assert.match(setup, /package-manager-cache: false/)
  }
})

test("qualification stops only its persistent builder without removing cache state, even after build failure", () => {
  const source = workflow("cd-qualification.yml")
  assert.match(source, /id: buildx\n        uses: docker\/setup-buildx-action@v4/)
  assert.match(source, /if: \$\{\{ always\(\) && steps\.buildx\.outputs\.name != '' \}\}/)
  assert.match(source, /docker container stop --time 120 "buildx_buildkit_\$\{BUILDER_NAME\}0"/)
  assert.match(source, /cleanup: false/)
  assert.doesNotMatch(source, /docker buildx rm/)
})

test("the warning gate waits for build, test and lint jobs and has read-only permissions", () => {
  const source = workflow("ci.yml").split("  migration-prefix-check:")[0]
  for (const job of ["frontend-build", "frontend-test", "frontend-views-test", "backend-tests", "backend-agent-tests", "go-lint"]) assert.ok(source.includes(job))
  assert.match(source, /actions: read\n      checks: read/)
  assert.doesNotMatch(source, /: write|secrets\.(?!GITHUB_TOKEN)/)
  assert.match(source, /node scripts\/warning-budget.mjs/)
})

test("pull-request path filtering does not supply inputs ignored by the action", () => {
  const source = workflow("ci.yml")
  assert.doesNotMatch(source, /base:.*github\.base_ref/)
  assert.match(source, /ref:.*github\.event_name == 'push' && github\.sha \|\| ''/)
})

test("pnpm setup installs the pinned package without a newer bootstrap self-update", () => {
  assert.match(workflow("ci.yml"), /run: node --test scripts\/setup-pnpm\.test\.mjs/)
  for (const name of readdirSync(new URL("../.github/workflows/", import.meta.url))) {
    if (!name.endsWith(".yml")) continue
    const source = workflow(name)
    assert.doesNotMatch(source, /uses: pnpm\/action-setup@/, name)
    if (source.includes("cache: pnpm")) assert.match(source, /run: bash scripts\/setup-pnpm\.sh/, name)
  }
})

test("unneeded lifecycle scripts remain disabled with an explicit reviewed decision", () => {
  const { pnpm } = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"))
  for (const name of ["core-js", "protobufjs", "unicode-animations", "electron-winstaller", "sharp"]) {
    assert.ok(pnpm.ignoredBuiltDependencies?.includes(name), name)
    assert.ok(!pnpm.onlyBuiltDependencies.includes(name), name)
  }
  for (const name of ["esbuild", "electron", "msw", "unrs-resolver"]) assert.ok(pnpm.onlyBuiltDependencies.includes(name), name)
})

test("runner pinning preserves historical probe and floating-image cache comments", () => {
  const source = workflow("ci.yml")
  assert.match(source, /runs Linux 6\.6\.141, GitHub-hosted ubuntu-latest runs 6\.17\.0/)
  assert.match(source, /when ubuntu-latest moves to a new/)
})

test("a CI job that is new on a PR has a zero warning baseline until main runs it", () => {
  const budget = readFileSync(new URL("./warning-budget.mjs", import.meta.url), "utf8")
  const zero = budget.match(/zeroBaselineJobs = new Set\(\[([^\]]*)\]\)/)?.[1] ?? ""
  assert.match(zero, /"locale-parity"/)
})

test("the gitleaks job folded into CI by CHE-4385 has a zero warning baseline until main runs it", () => {
  const budget = readFileSync(new URL("./warning-budget.mjs", import.meta.url), "utf8")
  const zero = budget.match(/zeroBaselineJobs = new Set\(\[([^\]]*)\]\)/)?.[1] ?? ""
  assert.match(zero, /"gitleaks"/)
})

test("dependency-review blocks a PR that adds a high or critical dependency and prints nothing about existing ones (CHE-4666)", () => {
  const source = workflow("ci.yml")
  const job = source.match(/^  dependency-review:\n(?:(?:    .*|)\n)+/m)?.[0] ?? ""
  assert.match(job, /runs-on: ubuntu-24\.04\n/)
  assert.match(job, /uses: actions\/dependency-review-action@[0-9a-f]{40} # v\d+\.\d+\.\d+/)
  assert.match(job, /if: \$\{\{ github\.event_name == 'pull_request' \}\}/)
  assert.match(job, /fail-on-severity: high/)
  assert.match(job, /fail-on-scopes: runtime, development\n/)
  assert.match(job, /comment-summary-in-pr: never/)
  assert.doesNotMatch(job, /secrets\.|: write/)
  const backend = source.match(/^  backend:\n(?:(?:    .*|)\n)+/m)[0]
  assert.match(backend, /needs: \[[^\]]*\bdependency-review\b/)
  assert.match(backend, /"dependency-review":"always"/)
  const zero = readFileSync(new URL("./warning-budget.mjs", import.meta.url), "utf8").match(/zeroBaselineJobs = new Set\(\[([^\]]*)\]\)/)?.[1] ?? ""
  assert.match(zero, /"dependency-review"/)
})
