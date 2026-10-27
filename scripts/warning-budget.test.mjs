import assert from "node:assert/strict"
import { spawnSync } from "node:child_process"
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { test } from "node:test"
import { fileURLToPath } from "node:url"
import { warningCounts, warningRegressions } from "./warning-budget.mjs"

const fixture = JSON.parse(readFileSync(new URL("./fixtures/warnings/new-warning.json", import.meta.url)))

test("a new warning in a build fixture exceeds main's warning budget", () => {
  assert.deepEqual(warningRegressions(warningCounts([]), warningCounts(fixture)), [
    { warning: "frontend-build: DeprecationWarning: legacy API is deprecated", baseline: 0, current: 1 },
  ])
})

test("unchanged warnings are allowed, but another occurrence is not", () => {
  const baseline = warningCounts(fixture)
  assert.deepEqual(warningRegressions(baseline, baseline), [])
  const current = warningCounts([{ ...fixture[0], log: `${fixture[0].log}\n${fixture[0].log}` }])
  assert.equal(warningRegressions(baseline, current)[0]?.current, 2)
})

test("annotations catch action warnings and notices without matching command echoes", () => {
  const warnings = warningCounts([{ name: "frontend-test", log: "2026-10-27T05:00:00Z ##[command]echo warning fixture", annotations: [
    { annotation_level: "warning", message: "Node.js 20 is deprecated" },
    { annotation_level: "notice", message: "ubuntu-latest migration" },
    { annotation_level: "failure", message: "not a warning" },
  ] }])
  assert.equal(Object.keys(warnings).length, 2)
})

test("PID and ANSI noise do not turn an existing warning into a new one", () => {
  const baseline = warningCounts(fixture)
  const current = warningCounts([{ ...fixture[0], log: fixture[0].log.replace("node:123", "node:456").replace("DeprecationWarning", "\u001b[33mDeprecationWarning\u001b[0m") }])
  assert.deepEqual(warningRegressions(baseline, current), [])
})

test("warning annotations mirrored in logs count once", () => {
  const warnings = warningCounts([{ name: "build", log: "2026-10-27T05:00:00Z ##[warning]new warning", annotations: [{ annotation_level: "warning", message: "new warning" }] }])
  assert.deepEqual(warnings, { "build: new warning": 1 })
})

test("test names and echoed commands are not diagnostics", () => {
  assert.deepEqual(warningCounts([{ name: "test", log: "ok 1 - rejects a Warning: fixture\n# Subtest: a warning fixture\n2026-10-27T05:00:00Z ##[command]echo Warning: fixture", annotations: [] }]), {})
})

test("Docker, CSS, eslint and React diagnostics are included", () => {
  const log = "WARNING! Your credentials are stored unencrypted\n#29 93.38 Found 2 warnings while optimizing generated CSS:\n8:12 warning Missing dependency\n# Warning: React act update\n⚠ Invalid next.config options detected:"
  assert.equal(Object.keys(warningCounts([{ name: "build", log, annotations: [] }])).length, 5)
  const baseline = warningCounts([{ name: "build", log, annotations: [] }])
  assert.deepEqual(warningRegressions(baseline, warningCounts([{ name: "build", log: log.replace("#29 93.38", "#31 12.78"), annotations: [] }])), [])
})

test("timestamped application events are not compiler or test-harness diagnostics", () => {
  const baseline = warningCounts([{ name: "backend-tests", log: '2026/10/27 04:58:17 WARN retry index="migrate_budget_1793077097572337861_1689744238.index_name"', annotations: [] }])
  const current = warningCounts([{ name: "backend-tests", log: '2026/10/28 06:00:22 WARN retry index="migrate_budget_1793078000000000000_42.index_name"', annotations: [] }])
  assert.deepEqual(warningRegressions(baseline, current), [])
  assert.deepEqual(current, {})
  assert.equal(Object.keys(warningCounts([{ name: "backend-tests", log: "warning: C compiler diagnostic\n2026/10/28 06:00:22.123456 WARN expected negative-control event", annotations: [] }])).length, 1)
})

test("the CI entrypoint fails a new-warning fixture and passes unchanged main evidence", () => {
  const directory = mkdtempSync(join(tmpdir(), "warning-budget-"))
  const command = fileURLToPath(new URL("./warning-budget.mjs", import.meta.url))
  try {
    writeFileSync(join(directory, "gh"), `#!/usr/bin/env node
const endpoint = process.argv[3]
const scenario = process.env.BUDGET_FIXTURE
let result
if (endpoint.endsWith('/actions/runs/2')) result = {id:2,workflow_id:3,created_at:'2026-10-27T05:00:00Z',head_sha:'candidate',html_url:'candidate-run'}
else if (endpoint.includes('/actions/workflows/3/runs?')) result = {workflow_runs: scenario === 'missing-baseline' ? [] : [{id:1,created_at:'2026-10-26T05:00:00Z',head_sha:'main',html_url:'main-run'}]}
else if (endpoint.includes('/jobs?')) {
  const baseline = endpoint.includes('/runs/1/')
  result = {jobs:[{id:baseline?11:12,name:'frontend-build',status:scenario==='incomplete'?'in_progress':'completed',conclusion:'success',check_run_url:'https://api.github.com/check-runs/'+(baseline?21:22)}]}
} else if (endpoint.includes('/annotations?')) result = []
else if (endpoint.endsWith('/logs')) {
  process.stdout.write(endpoint.includes('/jobs/12/') && scenario==='new-warning' ? 'Warning: fixture diagnostic' : '')
  process.exit(0)
} else throw new Error('Unexpected API request '+endpoint)
process.stdout.write(JSON.stringify(result))
`, { mode: 0o700 })
    for (const [scenario, expected] of [["new-warning", 1], ["unchanged", 0], ["missing-baseline", 1], ["incomplete", 1]]) {
      const result = spawnSync(process.execPath, [command], { cwd: directory, encoding: "utf8", env: { ...process.env, PATH: `${directory}:${process.env.PATH}`, GITHUB_REPOSITORY: "cheese-work/multica", GITHUB_RUN_ID: "2", BUDGET_FIXTURE: scenario, GITHUB_STEP_SUMMARY: "" } })
      assert.equal(result.status, expected, `${scenario}: ${result.stderr}`)
      if (scenario === "new-warning") {
        const report = JSON.parse(readFileSync(join(directory, "warning-budget-report.json")))
        assert.deepEqual(report.regressions, [{ warning: "frontend-build: Warning: fixture diagnostic", baseline: 0, current: 1 }])
      }
    }
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
})
