import assert from "node:assert/strict"
import { spawnSync } from "node:child_process"
import { existsSync, mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { test } from "node:test"
import { fileURLToPath } from "node:url"

for (const scenario of ["pinned", "install-failure", "invalid-pin"]) {
  test(`pnpm setup: ${scenario}`, () => {
    const directory = mkdtempSync(join(tmpdir(), "setup-pnpm-"))
    try {
      mkdirSync(join(directory, "bin"))
      writeFileSync(join(directory, "package.json"), JSON.stringify({ packageManager: scenario === "invalid-pin" ? "other-package@latest" : "pnpm@10.28.2" }))
      writeFileSync(join(directory, "bin/npm"), '#!/usr/bin/env node\nrequire("fs").writeFileSync(process.env.NPM_TRACE, JSON.stringify(process.argv.slice(2))); process.exit(Number(process.env.NPM_STATUS))\n', { mode: 0o700 })
      const pathFile = join(directory, "github-path")
      const trace = join(directory, "npm-trace")
      const result = spawnSync("bash", [fileURLToPath(new URL("./setup-pnpm.sh", import.meta.url))], { cwd: directory, encoding: "utf8", env: { ...process.env, PATH: `${directory}/bin:${process.env.PATH}`, RUNNER_TEMP: join(directory, "runner"), GITHUB_PATH: pathFile, NPM_TRACE: trace, NPM_STATUS: scenario === "install-failure" ? "23" : "0" } })
      assert.equal(result.status, scenario === "pinned" ? 0 : scenario === "install-failure" ? 23 : 1, result.stderr)
      if (scenario === "invalid-pin") assert.equal(existsSync(trace), false)
      else assert.deepEqual(JSON.parse(readFileSync(trace)), ["install", "--prefix", join(directory, "runner/ci-pnpm"), "pnpm@10.28.2"])
      if (scenario === "pinned") assert.equal(readFileSync(pathFile, "utf8"), `${directory}/runner/ci-pnpm/node_modules/.bin\n`)
      else assert.equal(existsSync(pathFile), false)
    } finally {
      rmSync(directory, { recursive: true, force: true })
    }
  })
}
