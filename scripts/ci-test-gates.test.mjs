import assert from "node:assert/strict"
import { readFileSync, readdirSync } from "node:fs"
import { test } from "node:test"

const dir = new URL("../.github/workflows/", import.meta.url)

// Workflow steps as raw text blocks. Steps are list items at 6 spaces of indent.
const steps = (source) => source.split(/^      - /m).slice(1)

// Actions' default `run` shell is `bash -e {0}`: no pipefail, so `cmd | tee log` exits with tee's
// status and a red test run (a failed test, a reported data race) leaves the job green. `shell: bash`
// is `bash -eo pipefail {0}`.
test("a CI step that pipes a Go test run into another command keeps the test's exit status", () => {
  const offenders = []
  for (const name of readdirSync(dir).filter((n) => n.endsWith(".yml"))) {
    for (const step of steps(readFileSync(new URL(name, dir), "utf8"))) {
      const run = step.match(/^        run: (.*(?:\n          .*)*)/m)?.[1] ?? ""
      const runsGoTests = /test-go\.sh|\bgo test\b|gotestsum/.test(run)
      const piped = /[^|]\|[^|]/.test(run.replace(/\|\|/g, ""))
      const keepsStatus = /^        shell: bash$/m.test(step) || /pipefail/.test(run)
      if (runsGoTests && piped && !keepsStatus) offenders.push(`${name}: ${step.split("\n")[0]}`)
    }
  }
  assert.deepEqual(offenders, [])
})
