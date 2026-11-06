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
  const summary = execFileSync("node", [script, dir], { encoding: "utf8" });
  return { summary, flakes: JSON.parse(readFileSync(join(dir, "flakes.json"), "utf8")) };
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
