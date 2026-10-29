import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { addedMigrations, checkMigrationPrefixes, collectMigrations } from "./migration-prefix-check.mjs";

const pair = (number, name) => [
  `server/migrations/${number}_${name}.up.sql`,
  `server/migrations/${number}_${name}.down.sql`,
];

test("colliding fixture fails with the other PR and exact pair renames", () => {
  const result = checkMigrationPrefixes({
    files: pair(691, "candidate"),
    main: pair(690, "main"),
    pulls: [{ number: 424, files: pair(691, "reserved") }],
  });
  assert.deepEqual(result.collisions, ["691_candidate collides with PR #424 (691_reserved)"]);
  assert.equal(result.nextFree, 692);
  assert.deepEqual(result.commands, [
    "git mv -- 'server/migrations/691_candidate.up.sql' 'server/migrations/692_candidate.up.sql'",
    "git mv -- 'server/migrations/691_candidate.down.sql' 'server/migrations/692_candidate.down.sql'",
  ]);
});

test("renumbered fixture passes", () => {
  assert.deepEqual(checkMigrationPrefixes({
    files: pair(692, "candidate"),
    main: pair(690, "main"),
    pulls: [{ number: 424, files: pair(691, "reserved") }],
  }).collisions, []);
});

for (const owner of ["main", "another open PR"]) {
  test(`same-path candidate and ${owner} reservation pass without rename suggestions`, () => {
    const files = pair(631, "wakeup_scoped_event");
    const result = checkMigrationPrefixes({
      files,
      main: owner === "main" ? files : [],
      pulls: owner === "another open PR" ? [{ number: 424, files }] : [],
    });
    assert.deepEqual(result.collisions, []);
    assert.deepEqual(result.commands, []);
  });
}

test("main collisions and multiple candidate collisions get distinct safe slots", () => {
  const result = checkMigrationPrefixes({
    files: [...pair("0691", "first"), ...pair(691, "second"), ...pair(700, "free")],
    main: pair(691, "taken"),
    pulls: [{ number: 1, files: pair(710, "reserved") }],
  });
  assert.match(result.collisions.join("\n"), /origin\/main \(691_taken\)/);
  assert.match(result.collisions.join("\n"), /this PR/);
  assert.equal(result.nextFree, 711);
  assert.match(result.commands[0], /0711_first.up.sql/);
  assert.match(result.commands[2], /712_second.up.sql/);
  assert.equal(result.commands.length, 4);
});

test("up/down pairs do not collide and historic main collisions are ignored", () => {
  assert.equal(checkMigrationPrefixes({
    files: pair(692, "candidate"), main: [...pair(500, "one"), ...pair(500, "two")], pulls: [],
  }).collisions.length, 0);
});

test("suggestions shell-quote filenames instead of executing PR input", () => {
  const result = checkMigrationPrefixes({ files: pair(691, "it's $(touch injected)"), main: pair(691, "taken"), pulls: [] });
  const command = result.commands[0].replace("git mv --", "printf '%s\\n'");
  const output = spawnSync("bash", ["-c", command], { encoding: "utf8" });
  assert.equal(output.status, 0);
  assert.equal(output.stdout, `${pair(691, "it's $(touch injected)")[0]}\n${pair(692, "it's $(touch injected)")[0]}\n`);
});

test("only added, copied and renamed migration paths reserve prefixes", () => {
  const filename = pair(691, "candidate")[0];
  assert.deepEqual(addedMigrations([
    ...["added", "copied", "renamed"].map((status) => ({ status, filename })),
    { status: "modified", filename }, { status: "removed", filename },
    { status: "added", filename: "server/migrations/README.md" },
  ]), [filename, filename, filename]);
});

const fileEntry = (number, name = "candidate") => ({ status: "added", filename: pair(number, name)[0] });
const options = { repository: "cheese-work/multica", number: 10 };
function mockAPI(routes, calls = []) {
  return async (url, init) => {
    const endpoint = url.split("/repos/cheese-work/multica/")[1];
    calls.push(endpoint);
    assert.ok(init.signal);
    assert.ok(Object.hasOwn(routes, endpoint), `Unexpected request: ${endpoint}`);
    return { ok: true, json: async () => routes[endpoint] };
  };
}

test("non-migration PRs avoid main and open-PR scans", async () => {
  const calls = [];
  const input = await collectMigrations({ ...options, fetcher: mockAPI({ "pulls/10/files?per_page=100&page=1": [] }, calls) });
  assert.deepEqual(input, { files: [], main: [], pulls: [] });
  assert.equal(calls.length, 1);
});

test("all PR and file pages are scanned, including drafts and other base branches", async () => {
  const filler = Array.from({ length: 100 }, () => ({ status: "modified", filename: "README.md" }));
  const pulls = Array.from({ length: 100 }, (_, index) => ({ number: index + 10 }));
  const routes = {
    "pulls/10/files?per_page=100&page=1": filler,
    "pulls/10/files?per_page=100&page=2": [fileEntry(691)],
    "git/trees/main?recursive=1": { truncated: false, tree: [{ type: "blob", path: pair(690, "main")[0] }] },
    "pulls?state=open&per_page=100&page=1": pulls,
    "pulls?state=open&per_page=100&page=2": [{ number: 424, draft: true, base: { ref: "stack" } }],
    ...Object.fromEntries(pulls.slice(1).map((pull) => [`pulls/${pull.number}/files?per_page=100&page=1`, []])),
    "pulls/424/files?per_page=100&page=1": filler,
    "pulls/424/files?per_page=100&page=2": [fileEntry(691, "reserved")],
  };
  const result = checkMigrationPrefixes(await collectMigrations({ ...options, fetcher: mockAPI(routes) }));
  assert.match(result.collisions.join("\n"), /PR #424/);
  assert.equal(result.nextFree, 692);
});

test("API failures, truncated trees, changed heads and file caps fail closed", async () => {
  await assert.rejects(collectMigrations({ ...options, fetcher: async () => ({ ok: false, status: 403 }) }), /HTTP 403/);
  const routes = {
    "pulls/10/files?per_page=100&page=1": [fileEntry(691)],
    "git/trees/main?recursive=1": { truncated: true, tree: [] },
    "pulls?state=open&per_page=100&page=1": [{ number: 10, head: { sha: "new" } }],
  };
  await assert.rejects(collectMigrations({ ...options, fetcher: mockAPI(routes) }), /Incomplete origin\/main/);
  routes["git/trees/main?recursive=1"].truncated = false;
  await assert.rejects(collectMigrations({ ...options, expectedHead: "old", fetcher: mockAPI(routes) }), /head changed/);
  await assert.rejects(collectMigrations({ ...options, fetcher: async () => ({ ok: true, json: async () => Array(100).fill(fileEntry(691)) }) }), /3000-file limit/);
});

test("one deadline bounds the entire API scan", async () => {
  await assert.rejects(collectMigrations({ ...options, signal: AbortSignal.timeout(5), fetcher: async (_url, { signal }) => {
    await new Promise((resolve, reject) => {
      const timer = setTimeout(resolve, 100);
      signal.addEventListener("abort", () => { clearTimeout(timer); reject(signal.reason); }, { once: true });
    });
  } }), /timeout/);
});

const workflow = readFileSync(new URL("../.github/workflows/ci.yml", import.meta.url), "utf8");
const job = (name) => workflow.match(new RegExp(`^  ${name}:\\n[\\s\\S]*?(?=^  [\\w-]+:|$(?![\\s\\S]))`, "m"))?.[0] ?? "";

test("PR-only scanner runs in parallel and required backend-tests fails closed", () => {
  assert.match(job("migration-prefix-check"), /^    if: github.event_name == 'pull_request'$/m);
  assert.doesNotMatch(job("migration-prefix-check"), /^    needs:/m);
  assert.match(job("migration-prefix-check"), /persist-credentials: false/);
  assert.match(job("migration-prefix-check"), /pull-requests: read/);
  assert.match(job("migration-prefix-check"), /timeout 25s node scripts\/migration-prefix-check.mjs/);
  assert.match(job("backend-tests"), /^    needs: \[changes, migration-prefix-check\]$/m);
  assert.match(job("backend-tests"), /^    if: \$\{\{ !cancelled\(\) && needs\.changes\.outputs\.reuse != 'true' \}\}$/m);
  const guard = job("backend-tests").match(/      - name: Require migration prefix result[\s\S]*?        run: \|\n((?:          .*\n)+)/)?.[1];
  assert.ok(guard, "required check must validate the scanner result before expensive steps");
  const command = guard.replace(/^          /gm, "");
  for (const expected of ["success", "skipped"]) {
    for (const actual of ["success", "failure", "cancelled", "skipped", ""]) {
      const result = spawnSync("bash", ["-c", command], { env: { ...process.env, EXPECTED_RESULT: expected, MIGRATION_RESULT: actual } });
      assert.equal(result.status === 0, actual === expected, `${expected}/${actual}`);
    }
  }
});
