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
  assert.match(aggregate, /^    needs: \[changes, migration-prefix-check, backend-test-shards\]$/m);
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
