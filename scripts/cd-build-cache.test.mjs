import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import test from "node:test"

const read = (path) => readFileSync(new URL(`../${path}`, import.meta.url), "utf8")
const cd = read(".github/workflows/cd-qualification.yml")
const backend = read("Dockerfile")
const web = read("Dockerfile.web")
const runLines = (dockerfile) => dockerfile.split("\n").filter((line) => line.startsWith("RUN "))

test("backend Go steps share module and build caches and stamp versions last", () => {
  const compiles = runLines(backend).filter((line) => /go (build|mod download)|build-daemon-binaries/.test(line))
  assert.ok(compiles.length >= 6)
  for (const line of compiles) {
    assert.match(line, /--mount=type=cache,target=\/go\/pkg\/mod/, line)
    assert.match(line, /--mount=type=cache,target=\/root\/\.cache\/go-build/, line)
  }
  const firstArg = backend.indexOf("ARG VERSION")
  for (const bin of ["migrate", "maintenance", "backfill_task_usage_hourly", "backfill_codex_usage_cache"]) {
    assert.ok(backend.indexOf(`bin/${bin}`) < firstArg, `${bin} must build before the version ARGs`)
  }
})

test("web build keeps the Next.js cache and sets build args after dependencies", () => {
  assert.match(web, /RUN --mount=type=cache,target=\/app\/apps\/web\/\.next\/cache pnpm --filter @multica\/web build/)
  assert.ok(web.indexOf("ARG NEXT_PUBLIC_APP_VERSION") > web.indexOf("pnpm install --frozen-lockfile --offline"))
})

test("qualification reuses one named builder on X99 and builds both images in parallel", () => {
  assert.doesNotMatch(cd, /docker\/setup-buildx-action/)
  assert.doesNotMatch(cd, /Remove this run's builder/)
  assert.match(cd, /BUILDER: cheese-x99-cd/)
  assert.match(cd, /docker buildx inspect "\$BUILDER" >\/dev\/null 2>&1 \|\| docker buildx create --name "\$BUILDER" --driver docker-container --bootstrap/)
  assert.equal(cd.match(/docker buildx build \\\n\s+--builder "\$BUILDER"/g)?.length, 2)
  assert.match(cd, /--provenance=mode=max/)
  assert.match(cd, /--sbom=true/)
  assert.match(cd, /^          wait "\$backend_pid" \|\| status=1$/m)
  assert.match(cd, /^          wait "\$web_pid" \|\| status=1$/m)
})

test("the persistent builder cache is bounded after every run", () => {
  assert.match(cd, /if: \$\{\{ always\(\) \}\}\n\s+timeout-minutes: 4\n\s+run: docker buildx prune --builder "\$BUILDER" --force --keep-storage 20GB/)
})
