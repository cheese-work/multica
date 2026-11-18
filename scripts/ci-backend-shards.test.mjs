import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import test from "node:test";

// CHE-4268: the merge-queue critical path was one 15 min `backend-tests` job. It is now a matrix of
// shard jobs (handler x3, service, rest) behind an aggregate that keeps the required check name.
const ci = readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8");
const policy = readFileSync(new URL("../.github/ci-policy.yml", import.meta.url), "utf8");
const job = (id) => (ci.match(new RegExp(`^  ${id}:\\n[\\s\\S]*?(?=^  [\\w-]+:\\n|$(?![\\s\\S]))`, "m"))?.[0] ?? "")
  .replace(/^ *#.*\n/gm, "");
const shards = job("backend-test-shards");
const aggregate = job("backend-tests");

test("the shard matrix covers handler shards, service and the rest, one runner each", () => {
  assert.ok(shards, "missing backend-test-shards job");
  const partitions = shards.match(/^        partition: \[(.*)\]$/m)?.[1].split(/,\s*/);
  assert.deepEqual(partitions, ["handler-0", "handler-1", "handler-2", "handler-3", "service", "rest"]);
  assert.match(shards, /^      fail-fast: false$/m);
  assert.match(shards, /^    runs-on: \$\{\{ vars\.RUNNER_BACKEND_TESTS \|\| 'ubuntu-24\.04' \}\}$/m);
  const handlerShards = partitions.filter((name) => name.startsWith("handler-")).length;
  assert.match(shards, new RegExp(`^      MULTICA_HANDLER_SHARDS: "${handlerShards}"$`, "m"));
  assert.match(shards, /bash scripts\/test-go\.sh --race --only regular --partition \$\{\{ matrix\.partition \}\}/);
});

test("each shard uploads its timing JSON", () => {
  assert.match(shards, /node scripts\/go-test-timing\.mjs "\$\{\{ matrix\.partition \}\}"/);
  assert.match(shards, /actions\/upload-artifact@v7/);
  assert.match(shards, /name: go-test-timing-\$\{\{ matrix\.partition \}\}/);
});

test("required check names are unchanged and the aggregate backend-tests gates on every shard", () => {
  assert.ok(aggregate, "missing backend-tests job");
  assert.match(policy, /required_checks:\n  - backend-tests\n  - security-gate\n/);
  assert.match(ci, /^  backend:\n/m);
  assert.match(aggregate, /^    needs: \[changes, migration-prefix-check, migration-checks, backend-test-shards\]$/m);
  assert.doesNotMatch(aggregate, /services:|setup-go|test-go\.sh/, "the aggregate runs no tests");
  const guard = aggregate.match(/      - name: Require shard results[\s\S]*?        run: \|\n((?:          .*\n)+)/)?.[1];
  assert.ok(guard, "aggregate must check the shard result");
  const command = guard.replace(/^          /gm, "");
  for (const actual of ["success", "failure", "cancelled", "skipped", ""]) {
    const result = spawnSync("bash", ["-c", command], { env: { ...process.env, SHARDS_RESULT: actual } });
    assert.equal(result.status === 0, actual === "success", `shards ${actual}`);
  }
});

test("the old single-job layout is gone: only shards run the Go test step", () => {
  assert.equal(ci.match(/scripts\/test-go\.sh --race --only regular/g)?.length, 1);
  assert.match(shards, /scripts\/test-go\.sh --race --only regular/);
});

// CHE-6847: the Decision card E2E (CHE-6247) ran in the `rest` shard, after the Go tests, with the
// Go server, `next dev` and Chromium beside the Postgres service. In 15 of 36 runs the runner was
// lost there and GitHub failed the job only after ~57 min, so `backend-tests` missed the merge
// queue's 30 min limit and the PR was dropped. The check now has its own job on a fresh runner,
// serves a production build and is a dependency of the required `backend` aggregate.
const e2e = job("decision-card-e2e");
const e2eStep = (name) => e2e.match(new RegExp(`      - name: ${name}\\n[\\s\\S]*?(?=^      - name: |$(?![\\s\\S]))`, "m"))?.[0] ?? "";

test("the rest shard no longer runs the decision card E2E", () => {
  assert.doesNotMatch(shards, /Decision card E2E|decision card E2E|playwright|pnpm dev:web/);
});

test("the decision card E2E is its own bounded job with its own database", () => {
  assert.ok(e2e, "missing decision-card-e2e job");
  assert.match(e2e, /^    name: decision-card-e2e$/m);
  assert.match(e2e, /^    needs: \[changes\]$/m);
  assert.match(e2e, /^    if: \$\{\{ !cancelled\(\) && needs\.changes\.outputs\.push_backend == 'true' \}\}$/m);
  const minutes = Number(e2e.match(/^    timeout-minutes: (\d+)$/m)?.[1]);
  assert.ok(minutes > 0 && minutes <= 20, `job timeout-minutes must be 1..20, got ${minutes}`);
  assert.match(e2e, /^    services:\n      postgres:/m);
  assert.match(e2e, /go run \.\/cmd\/migrate up/);
});

test("the decision card E2E serves a production build and every wait in it is bounded", () => {
  const step = e2eStep("Decision card E2E");
  assert.ok(step, "missing Decision card E2E step");
  assert.doesNotMatch(step, /dev:web|next dev/);
  assert.match(e2e, /next build/);
  assert.match(step, /next start|pnpm --filter @multica\/web start/);
  const minutes = Number(step.match(/^        timeout-minutes: (\d+)$/m)?.[1]);
  assert.ok(minutes > 0 && minutes <= 12, `step timeout-minutes must be 1..12, got ${minutes}`);
  const globalTimeout = Number(step.match(/playwright test [^\n]*--global-timeout=(\d+)/)?.[1]);
  assert.ok(globalTimeout > 0 && globalTimeout < minutes * 60_000, "playwright --global-timeout must be set and end before the step timeout");
});

test("a red decision card E2E uploads the API and web logs, and a memory trace", () => {
  const upload = e2eStep("Upload decision card E2E logs");
  assert.match(upload, /^        if: \$\{\{ !cancelled\(\) \}\}$/m);
  assert.match(upload, /actions\/upload-artifact@v7/);
  for (const log of ["e2e-api.log", "e2e-web.log", "e2e-mem.log"]) assert.match(upload, new RegExp(log));
  assert.match(e2eStep("Decision card E2E"), /free -m/);
});

test("the required backend aggregate waits on the decision card E2E and expects its result", () => {
  const backend = job("backend");
  assert.match(backend, /^    needs: \[[^\]]*\bdecision-card-e2e\b[^\]]*\]$/m);
  const scopes = JSON.parse(backend.match(/JOB_SCOPES: '(\{.*\})'/)[1]);
  assert.equal(scopes["decision-card-e2e"], "push_backend");
});
