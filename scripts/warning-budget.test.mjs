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

test("ANSI sequences are stripped before warning detection", () => {
  assert.deepEqual(warningCounts([{ name: "build", log: "\u001b[2KWar\u001b[33mning: fixture diagnostic\u001b[0m", annotations: [] }]), { "build: Warning: fixture diagnostic": 1 })
})

test("warning annotations mirrored in logs count once", () => {
  const warnings = warningCounts([{ name: "build", log: "2026-10-27T05:00:00Z ##[warning]new warning", annotations: [{ annotation_level: "warning", message: "new warning" }] }])
  assert.deepEqual(warnings, { "build: new warning": 1 })
})

test("test names and echoed commands are not diagnostics", () => {
  assert.deepEqual(warningCounts([{ name: "test", log: "ok 1 - rejects a Warning: fixture\n# Subtest: a warning fixture\n2026-10-27T05:00:00Z ##[command]echo Warning: fixture", annotations: [] }]), {})
})

test("runner command headers are metadata but action output groups are diagnostics", () => {
  const log = "2026-10-27T05:00:00Z ##[group]Run node scripts/check-image-budget.mjs\n2026-10-27T05:00:00Z env:\n2026-10-27T05:00:00Z   PR_BODY: Expected: DeprecationWarning: example\n2026-10-27T05:00:00Z ##[endgroup]\n2026-10-27T05:00:00Z ##[group]Running tool\n2026-10-27T05:00:00Z Warning: real diagnostic\n2026-10-27T05:00:00Z ##[endgroup]"
  assert.deepEqual(warningCounts([{ name: "build", log, annotations: [] }]), { "build: Warning: real diagnostic": 1 })
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

test("a BOM at a GitHub log chunk boundary does not defeat the application WARN skip", () => {
  const log = '2026-10-29T19:17:20.5264970Z 2026/10/29 19:17:20 WARN plugins: queue full dropped_total=784\n\uFEFF2026-10-29T19:17:20.5267014Z 2026/10/29 19:17:20 WARN plugins: queue full dropped_total=785'
  assert.deepEqual(warningCounts([{ name: "backend-tests", log, annotations: [] }]), {})
})

test("eslint warnings follow file and rule rather than line numbers", () => {
  const job = (file, line) => ({
    name: "frontend-build",
    log: `2026-10-27T05:00:00Z /home/runner/work/multica/multica/${file}\n2026-10-27T05:00:00Z ##[warning] ${line}:6 warning Hook dependency changes (at line ${line + 3}) react-hooks/exhaustive-deps`,
    annotations: [{ annotation_level: "warning", path: file, start_line: line, message: `Hook dependency changes (at line ${line + 3})` }],
  })
  const baseline = warningCounts([job("packages/views/first.tsx", 54)])
  assert.deepEqual(baseline, { "frontend-build: packages/views/first.tsx: react-hooks/exhaustive-deps": 1 })
  assert.deepEqual(warningRegressions(baseline, warningCounts([job("packages/views/first.tsx", 55)])), [])
  assert.equal(warningRegressions(baseline, warningCounts([job("packages/views/second.tsx", 55)])).length, 1)
  const repeated = job("packages/views/first.tsx", 55)
  repeated.log += `\n${repeated.log}`
  assert.equal(warningRegressions(baseline, warningCounts([repeated]))[0].current, 2)
  const coreRule = { ...job("packages/views/first.tsx", 55), log: "/home/runner/work/multica/multica/packages/views/first.tsx\n55:6 warning Expected braces around the statement curly", annotations: [] }
  assert.deepEqual(warningCounts([coreRule]), { "frontend-build: packages/views/first.tsx: curly": 1 })
})

test("an eslint annotation GitHub files under .github counts once with its log line", () => {
  const log = "2026-10-27T05:00:00Z /home/runner/work/multica/multica/packages/core/platform/core-provider.tsx\n2026-10-27T05:00:00Z ##[warning]  169:5  warning  React Hook useMemo has missing dependencies: 'apiBaseUrl'. Either include them or remove the dependency array  react-hooks/exhaustive-deps"
  const message = "React Hook useMemo has missing dependencies: 'apiBaseUrl'. Either include them or remove the dependency array"
  const withLog = warningCounts([{ name: "frontend-build", log, annotations: [] }])
  const withBoth = warningCounts([{ name: "frontend-build", log, annotations: [{ annotation_level: "warning", path: ".github", start_line: 169, message }] }])
  assert.deepEqual(withBoth, withLog)
  assert.deepEqual(warningRegressions(withLog, withBoth), [])
})

test("the same eslint message in two files stays collision-safe when annotation retention changes", () => {
  const detail = "Unexpected any. Specify a different type."
  const log = ["first.ts", "second.ts"].map((file) => `2026-10-27T05:00:00Z /home/runner/work/multica/multica/packages/${file}\n2026-10-27T05:00:00Z ##[warning]  1:1  warning  ${detail}  @typescript-eslint/no-explicit-any`).join("\n")
  const annotation = { annotation_level: "warning", path: ".github", start_line: 1, message: detail }
  const job = (annotations) => ({ name: "frontend-build", log, annotations })
  const baseline = warningCounts([job([annotation])])
  const current = warningCounts([job([annotation, annotation])])
  assert.deepEqual(current, baseline)
  assert.deepEqual(warningRegressions(baseline, current), [])
  assert.deepEqual(baseline, { "frontend-build: packages/first.ts: @typescript-eslint/no-explicit-any": 1, "frontend-build: packages/second.ts: @typescript-eslint/no-explicit-any": 1 })
})

test("lowercase npm warn diagnostics are counted", () => {
  assert.deepEqual(warningCounts([{ name: "install", log: "npm warn deprecated fixture@1.0.0: use the maintained API", annotations: [] }]), {
    "install: npm warn deprecated fixture@1.0.0: use the maintained API": 1,
  })
})

test("the CI entrypoint fails a new-warning fixture and passes unchanged main evidence", () => {
  const directory = mkdtempSync(join(tmpdir(), "warning-budget-"))
  const command = fileURLToPath(new URL("./warning-budget.mjs", import.meta.url))
  try {
    writeFileSync(join(directory, "gh"), `#!/usr/bin/env node
const endpoint = process.argv[3]
const scenario = process.env.BUDGET_FIXTURE
const skipped = scenario.startsWith('skipped-baseline') || scenario === 'paged-baseline' || scenario === 'missing-job'
const renamed = scenario.startsWith('renamed-')
let result
if (endpoint.endsWith('/actions/runs/2')) result = {id:2,workflow_id:3,created_at:'2026-10-27T05:00:00Z',head_sha:'candidate',html_url:'candidate-run'}
else if (endpoint.includes('/actions/workflows/3/runs?')) {
  const page = Number(new URLSearchParams(endpoint.split('?')[1]).get('page') || 1)
  const latest = {id:1,event:'push',head_branch:'main',created_at:'2026-10-26T05:00:00Z',head_sha:'main',html_url:'main-run'}
  const older = {id:3,event:'schedule',head_branch:'main',created_at:'2026-10-25T05:00:00Z',head_sha:'older-main',html_url:'older-main-run'}
  const untrusted = {id:4,event:'pull_request',head_branch:'main',created_at:'2026-10-27T04:00:00Z',head_sha:'fork',html_url:'fork-run'}
  if (scenario === 'missing-baseline') result = {workflow_runs:[]}
  else if (scenario === 'paged-baseline') result = {workflow_runs:page === 1 ? [latest, ...Array.from({length:99}, (entry, index) => ({...untrusted,id:100+index}))] : page === 2 ? [older] : []}
  else result = {workflow_runs:page > 1 ? [] : [latest, ...(skipped && scenario !== 'missing-job' ? [untrusted, {...older,id:5,event:'workflow_dispatch',head_branch:'feature'}, {...older,id:6,created_at:'2026-10-28T05:00:00Z'}, older] : [])]}
}
else if (endpoint.includes('/jobs?')) {
  const baseline = endpoint.includes('/runs/1/')
  const older = endpoint.includes('/runs/3/')
  const name = renamed ? (scenario.includes('cursor') ? 'cursor-background-lifecycle' : 'installer') + ' (' + (baseline ? 'ubuntu-latest' : 'ubuntu-24.04') + ')' : !baseline&&!older&&scenario.startsWith('pr-only')?'image-budget':'frontend-build'
  result = {jobs:[{id:older?13:baseline?11:12,name,status:scenario==='incomplete'?'in_progress':'completed',conclusion:baseline&&skipped?'skipped':'success',check_run_url:'https://api.github.com/check-runs/'+(older?23:baseline?21:22)}]}
  if (scenario.startsWith('cache-warm') && !baseline && !older) result.jobs.push(...['push-go-cache-warm', 'push-turbo-cache-warm'].map(name => ({name,status:'in_progress',conclusion:null})))
} else if (endpoint.includes('/annotations?')) result = []
else if (endpoint.endsWith('/logs')) {
  if (scenario === 'ansi-warning' && !process.argv.includes('--allow-escape-sequences')) {
    process.stderr.write('the response contains terminal escape sequences; pass --allow-escape-sequences to output it anyway')
    process.exit(1)
  }
  const current = endpoint.includes('/jobs/12/')
  const diagnostic = scenario === 'ansi-warning' ? '\\u001b[2KWar\\u001b[33mning: fixture diagnostic\\u001b[0m' : 'Warning: fixture diagnostic'
  const warning = renamed || (current && ['new-warning', 'ansi-warning', 'pr-only-warning', 'cache-warm-new-warning'].includes(scenario)) || (skipped && !endpoint.includes('/jobs/11/'))
  process.stdout.write(warning ? diagnostic + (current && (scenario === 'skipped-baseline-new-warning' || scenario.endsWith('-extra-warning')) ? '\\n'+diagnostic : '') : '')
  process.exit(0)
} else throw new Error('Unexpected API request '+endpoint)
process.stdout.write(JSON.stringify(result))
`, { mode: 0o700 })
    for (const [scenario, expected] of [["cache-warm", 0], ["cache-warm-new-warning", 1], ["new-warning", 1], ["unchanged", 0], ["missing-baseline", 1], ["incomplete", 1], ["ansi-warning", 1], ["skipped-baseline", 0], ["skipped-baseline-new-warning", 1], ["paged-baseline", 0], ["missing-job", 1], ["pr-only-warning", 1], ["pr-only-clean", 0], ["renamed-cursor", 0], ["renamed-cursor-extra-warning", 1], ["renamed-installer", 0], ["renamed-installer-extra-warning", 1]]) {
      const result = spawnSync(process.execPath, [command], { cwd: directory, encoding: "utf8", env: { ...process.env, PATH: `${directory}:${process.env.PATH}`, GITHUB_REPOSITORY: "cheese-work/multica", GITHUB_RUN_ID: "2", BUDGET_FIXTURE: scenario, GITHUB_STEP_SUMMARY: "" } })
      assert.equal(result.status, expected, `${scenario}: ${result.stderr}`)
      if (["new-warning", "ansi-warning", "cache-warm-new-warning"].includes(scenario)) {
        const report = JSON.parse(readFileSync(join(directory, "warning-budget-report.json")))
        assert.deepEqual(report.regressions, [{ warning: "frontend-build: Warning: fixture diagnostic", baseline: 0, current: 1 }])
      }
      if (scenario === "cache-warm") {
        const report = JSON.parse(readFileSync(join(directory, "warning-budget-report.json")))
        assert.deepEqual(Object.keys(report.job_baselines), ["frontend-build"])
      }
      if (["skipped-baseline", "skipped-baseline-new-warning", "paged-baseline"].includes(scenario)) {
        const report = JSON.parse(readFileSync(join(directory, "warning-budget-report.json")))
        assert.deepEqual(report.job_baselines["frontend-build"], { run: "older-main-run", sha: "older-main", job: "frontend-build" })
        assert.deepEqual(report.regressions, scenario === "skipped-baseline-new-warning" ? [{ warning: "frontend-build: Warning: fixture diagnostic", baseline: 1, current: 2 }] : [])
      }
      if (scenario.startsWith("pr-only")) {
        const report = JSON.parse(readFileSync(join(directory, "warning-budget-report.json")))
        assert.deepEqual(report.zero_baseline_jobs, ["image-budget"])
        assert.deepEqual(report.baseline, {})
      }
      if (scenario.startsWith("renamed-")) {
        const report = JSON.parse(readFileSync(join(directory, "warning-budget-report.json")))
        const job = scenario.includes("cursor") ? "cursor-background-lifecycle" : "installer"
        assert.deepEqual(report.job_baselines[`${job} (ubuntu-24.04)`], { run: "main-run", sha: "main", job: `${job} (ubuntu-latest)` })
        assert.deepEqual(report.regressions, scenario.endsWith("-extra-warning") ? [{ warning: `${job} (ubuntu-24.04): Warning: fixture diagnostic`, baseline: 1, current: 2 }] : [])
      }
    }
  } finally {
    rmSync(directory, { recursive: true, force: true })
  }
})
