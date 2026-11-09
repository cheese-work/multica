import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

// CHE-4270: go-test-flakes.mjs turns the merge_group rerun output into flakes.json plus a job summary.
const script = new URL("./go-test-flakes.mjs", import.meta.url).pathname;
const ev = (Action, Package, Test) => JSON.stringify({ Action, Package, ...(Test ? { Test } : {}) });
const run = (files) => {
  const dir = mkdtempSync(join(tmpdir(), "flakes-"));
  for (const [name, text] of Object.entries(files)) writeFileSync(join(dir, name), text);
  let summary;
  let status = 0;
  try {
    summary = execFileSync("node", [script, dir], { encoding: "utf8" });
  } catch (error) {
    ({ status } = error);
    summary = error.stdout;
  }
  return { summary, status, flakes: JSON.parse(readFileSync(join(dir, "flakes.json"), "utf8")) };
};

test("a test that fails then passes is one flake; steady tests are not", () => {
  const { flakes, summary } = run({
    "go-test-regular.json": [
      ev("fail", "pkg/a", "TestFlaky"), ev("fail", "pkg/a"),
      ev("pass", "pkg/a", "TestFlaky"), ev("pass", "pkg/a"),
      ev("pass", "pkg/a", "TestSteady"),
    ].join("\n"),
  });
  assert.deepEqual(flakes, { flakes: [{ package: "pkg/a", test: "TestFlaky" }] });
  assert.match(summary, /1 flaky test/);
  assert.match(summary, /pkg\/a.*TestFlaky/);
});

test("a test that never passes is not a flake", () => {
  const { flakes } = run({ "go-test-regular.json": [ev("fail", "pkg/a", "TestBroken"), ev("fail", "pkg/a", "TestBroken")].join("\n") });
  assert.deepEqual(flakes.flakes, []);
});

test("a parent of a flaky subtest is listed once, as the subtest", () => {
  const { flakes } = run({
    "go-test-regular.json": [
      ev("fail", "p", "TestP/sub"), ev("fail", "p", "TestP"),
      ev("pass", "p", "TestP/sub"), ev("pass", "p", "TestP"),
    ].join("\n"),
  });
  assert.deepEqual(flakes.flakes, [{ package: "p", test: "TestP/sub" }]);
});

test("handler shard flakes merge with gotestsum flakes, sorted and deduplicated", () => {
  const { flakes } = run({
    "go-test-agent.json": [ev("fail", "z", "TestZ"), ev("pass", "z", "TestZ")].join("\n"),
    "handler-flakes.txt": "internal/handler\tTestH\ninternal/handler\tTestH\n",
  });
  assert.deepEqual(flakes.flakes, [
    { package: "internal/handler", test: "TestH" },
    { package: "z", test: "TestZ" },
  ]);
});

test("no flakes: empty list and a clear summary", () => {
  const { flakes, summary } = run({ "go-test-regular.json": ev("pass", "p", "TestOk") });
  assert.deepEqual(flakes.flakes, []);
  assert.match(summary, /No flaky tests/);
});

// Reruns are for flakes, not for a real break: more than 3 tests failing in one run fails the run even
// when every rerun passed (the shards and the other packages rerun independently, so only this
// job-wide count can see it). flakes.json and the summary are still written.
const flakyRun = (n) => ({
  "go-test-regular.json": Array.from({ length: n }, (_, i) => [ev("fail", "p", `TestF${i}`), ev("pass", "p", `TestF${i}`)]).flat().join("\n"),
});

test("3 flakes in one run pass; 4 fail the run but are still recorded", () => {
  const ok = run(flakyRun(3));
  assert.equal(ok.status, 0);
  const bad = run(flakyRun(4));
  assert.equal(bad.status, 1);
  assert.equal(bad.flakes.flakes.length, 4);
  assert.match(bad.summary, /more than 3/i);
});

test("flakes from the shards and the other packages count together", () => {
  const { status } = run({
    "go-test-regular.json": [ev("fail", "a", "TestA"), ev("pass", "a", "TestA"), ev("fail", "a", "TestB"), ev("pass", "a", "TestB")].join("\n"),
    "handler-flakes.txt": "internal/handler\tTestH1\ninternal/handler\tTestH2\n",
  });
  assert.equal(status, 1);
});

// A rerun is its own gotestsum run: first-pass file, then go-test-<name>-rerun-<n>.json.
test("a fail in the first pass and a pass in the rerun file is one flake", () => {
  const { flakes } = run({
    "go-test-regular.json": ev("fail", "p", "TestX/sub") + "\n" + ev("fail", "p", "TestX"),
    "go-test-regular-rerun-1.json": ev("pass", "p", "TestX/sub") + "\n" + ev("pass", "p", "TestX"),
  });
  assert.deepEqual(flakes.flakes, [{ package: "p", test: "TestX/sub" }]);
});

// --plan: the job-wide rerun decision of test-go.sh reads the first-pass failures of each run.
const plan = (events) => {
  const dir = mkdtempSync(join(tmpdir(), "plan-"));
  const file = join(dir, "go-test-regular.json");
  writeFileSync(file, events.join("\n"));
  return execFileSync("node", [script, "--plan", file], { encoding: "utf8" });
};
const out = (Output) => JSON.stringify({ Action: "output", Package: "p", Output });

test("--plan lists every failed leaf test with its root test", () => {
  assert.equal(
    plan([ev("fail", "p", "TestB/sub"), ev("fail", "p", "TestB"), ev("fail", "p", "TestA"), ev("fail", "p")]),
    "p\tTestA\tTestA\np\tTestB\tTestB/sub\n",
  );
});

test("--plan counts four sibling subtests of one root as four failures", () => {
  const subs = [1, 2, 3, 4].map((i) => ev("fail", "p", `TestA/s${i}`));
  const lines = plan([...subs, ev("fail", "p", "TestA"), ev("fail", "p")]).trim().split("\n");
  assert.deepEqual(lines, [1, 2, 3, 4].map((i) => `p\tTestA\tTestA/s${i}`));
});

test("--plan prints ! for a panic, a race, a timeout, a package-level failure or a missing file", () => {
  for (const events of [
    [out("panic: boom\n"), ev("fail", "p", "TestA"), ev("fail", "p")],
    [out("WARNING: DATA RACE\n"), ev("fail", "p", "TestA"), ev("fail", "p")],
    [out("panic: test timed out after 10m0s\n"), ev("fail", "p")],
    [ev("fail", "p")], // build or TestMain failure: no failing test
  ]) {
    assert.equal(plan(events), "!\n");
  }
  assert.equal(execFileSync("node", [script, "--plan", "/nonexistent/x.json"], { encoding: "utf8" }), "!\n");
});

test("--plan prints nothing for a passing run", () => {
  assert.equal(plan([ev("pass", "p", "TestA"), ev("pass", "p")]), "");
});

test("--unproven lists the plan leaves with no pass event in the rerun logs", () => {
  const dir = mkdtempSync(join(tmpdir(), "unproven-"));
  const planFile = join(dir, "plan.txt");
  const rerun = join(dir, "rerun.json");
  writeFileSync(planFile, "p\tTestA\tTestA/s1\np\tTestA\tTestA/s2\n");
  writeFileSync(rerun, [ev("pass", "p", "TestA/s1"), ev("skip", "p", "TestA/s2"), ev("pass", "p", "TestA")].join("\n"));
  assert.equal(execFileSync("node", [script, "--unproven", planFile, rerun], { encoding: "utf8" }), "p\tTestA/s2\n");
});
