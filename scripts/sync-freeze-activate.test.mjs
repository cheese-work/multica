import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

// CHE-9542 activation barrier. Merge-queue checks that started while SYNC_FREEZE was off keep their old result, so
// turning the variable on does not stop an entry already in the queue. The activation script therefore refuses to
// turn the freeze on while the queue holds any entry other than PR 1834, and reads the queue again after the variable
// is set: an entry that slipped in before the set (its check ran with the old value) fails the activation loudly.
const script = new URL("./sync-freeze-activate.sh", import.meta.url).pathname;

// A fake gh. FAKE_QUEUE_1 / FAKE_QUEUE_2 are the PR numbers (space separated) returned by the first and later queue
// reads; FAKE_QUEUE_FAIL makes the queue read fail; FAKE_VAR_FAIL makes `gh variable set` fail.
function run(env) {
  const dir = mkdtempSync(join(tmpdir(), "sfa-"));
  const log = join(dir, "gh.log");
  const count = join(dir, "reads");
  writeFileSync(
    join(dir, "gh"),
    `#!/bin/sh
echo "$*" >> "${log}"
case "$1 $2" in
  "api graphql")
    if [ -n "$FAKE_QUEUE_FAIL" ]; then echo "boom" >&2; exit 1; fi
    n=$(cat "${count}" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "${count}"
    if [ "$n" = 1 ]; then set -- $FAKE_QUEUE_1; else set -- $FAKE_QUEUE_2; fi
    printf '{"data":{"repository":{"mergeQueue":{"entries":{"totalCount":%s,"nodes":[' "$#"
    sep=""; for p in "$@"; do printf '%s{"pullRequest":{"number":%s}}' "$sep" "$p"; sep=","; done
    printf ']}}}}}'
    ;;
  "variable set") if [ -n "$FAKE_VAR_FAIL" ]; then echo "403" >&2; exit 1; fi ;;
esac
exit 0
`,
  );
  chmodSync(join(dir, "gh"), 0o755);
  const res = spawnSync("bash", [script], {
    env: { PATH: `${dir}:${process.env.PATH}`, REPO: "o/r", FAKE_QUEUE_1: "", FAKE_QUEUE_2: "", ...env },
    encoding: "utf8",
  });
  const calls = readFileSync(log, "utf8");
  return { ...res, calls, out: res.stdout + res.stderr, set: /variable set SYNC_FREEZE --body on/.test(calls) };
}

test("empty queue: sets the variable on and re-reads the queue after", () => {
  const r = run({});
  assert.equal(r.status, 0, r.out);
  assert.ok(r.set);
  assert.equal((r.calls.match(/api graphql/g) || []).length, 2);
  assert.ok(r.calls.indexOf("variable set") > r.calls.indexOf("api graphql"), "queue read before the set");
  assert.ok(r.calls.lastIndexOf("api graphql") > r.calls.indexOf("variable set"), "queue read after the set");
});

test("queue holds only PR 1834: allowed, variable set", () => {
  const r = run({ FAKE_QUEUE_1: "1834", FAKE_QUEUE_2: "1834" });
  assert.equal(r.status, 0, r.out);
  assert.ok(r.set);
});

test("another PR is queued: refuses, names it, does not set the variable", () => {
  const r = run({ FAKE_QUEUE_1: "1834 2100 2101" });
  assert.equal(r.status, 1, r.out);
  assert.equal(r.set, false);
  assert.match(r.out, /2100/);
  assert.match(r.out, /2101/);
  assert.match(r.out, /drain|dequeue|empty/i);
});

test("PR 1834 is not a prefix match: 18340 blocks activation", () => {
  assert.equal(run({ FAKE_QUEUE_1: "18340" }).status, 1);
});

test("an entry that joined before the set is caught by the re-read: exit 2, variable already on, rollback printed", () => {
  const r = run({ FAKE_QUEUE_1: "", FAKE_QUEUE_2: "2100" });
  assert.equal(r.status, 2, r.out);
  assert.ok(r.set);
  assert.match(r.out, /2100/);
  assert.match(r.out, /SYNC_FREEZE --body off/);
});

test("queue unreadable: fail closed, variable not set", () => {
  const r = run({ FAKE_QUEUE_FAIL: "1" });
  assert.equal(r.status, 1, r.out);
  assert.equal(r.set, false);
});

test("variable set fails: exit non-zero, no success message", () => {
  const r = run({ FAKE_VAR_FAIL: "1" });
  assert.notEqual(r.status, 0, r.out);
  assert.doesNotMatch(r.out, /SYNC_FREEZE is on/);
});

test("success output tells the operator the lift command", () => {
  const r = run({});
  assert.match(r.out, /SYNC_FREEZE is on/);
  assert.match(r.out, /SYNC_FREEZE --body off/);
});

test("the CI node test list includes both freeze suites", () => {
  const ci = readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8");
  assert.match(ci, /scripts\/sync-freeze-check\.test\.mjs/);
  assert.match(ci, /scripts\/sync-freeze-activate\.test\.mjs/);
});
