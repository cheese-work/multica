import assert from "node:assert/strict";
import { existsSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fetchChangelog, FETCH_DEADLINE_MS } from "./fork-changelog-fetch.mjs";

const sha = "a".repeat(40);
const pr = (number, extra = {}) => ({
  number, title: "feat: Notes", body: "Changelog: Notes now show shipped changes.",
  head: { ref: "feature/notes" }, merge_commit_sha: sha,
  merged_at: "2026-10-12T18:00:00Z", updated_at: "2026-10-13T00:00:00Z", ...extra,
});
const page = (prs, next = false) => ({
  ok: true, status: 200, json: async () => prs,
  headers: new Headers(next ? { link: '<https://api.github.com/next>; rel="next"' } : {}),
});

function fixture(context, extra = {}) {
  const directory = mkdtempSync(join(tmpdir(), "fork-fetch-"));
  context.after(() => rmSync(directory, { recursive: true, force: true }));
  const out = join(directory, "entries.json");
  const summaryPath = join(directory, "summary.md");
  writeFileSync(out, "stale entries");
  const warnings = [];
  return { directory, warnings, out, summaryPath, options: {
    token: "test-token", out, summaryPath, firstParent: [sha],
    warning: (message) => warnings.push(message), fetchImpl: async () => page([pr(1)]), ...extra,
  } };
}

test("fetches closed main PRs ordered by update, writes atomically and summarizes counts", async (context) => {
  const fixtureData = fixture(context, { fetchImpl: async (url, options) => {
    assert.equal(existsSync(fixtureData.out), false);
    const query = new URL(url).searchParams;
    for (const [key, value] of Object.entries({ state: "closed", base: "main", sort: "updated", direction: "desc", per_page: "100" })) {
      assert.equal(query.get(key), value);
    }
    assert.equal(options.headers.Authorization, "Bearer test-token");
    assert.ok(options.signal instanceof AbortSignal);
    return page([pr(1), pr(2, { body: "Changelog: none" }), pr(3, { merged_at: null })]);
  } });
  const result = await fetchChangelog(fixtureData.options);
  assert.equal(result.entries.length, 1);
  assert.deepEqual(JSON.parse(readFileSync(fixtureData.out, "utf8")), { entries: result.entries });
  assert.deepEqual(readdirSync(fixtureData.directory).sort(), ["entries.json", "summary.md"]);
  assert.match(readFileSync(fixtureData.summaryPath, "utf8"), /Shown: 1[\s\S]*Hidden: 1[\s\S]*none: 1/);
  assert.deepEqual(fixtureData.warnings, []);
  assert.equal(FETCH_DEADLINE_MS, 180_000);
});

test("paginates until updated_at is strictly before the ICT start", async (context) => {
  const pages = [page([pr(1)], true), page([
    pr(2, { updated_at: "2026-10-10T17:00:00Z" }),
    pr(3, { updated_at: "2026-10-10T16:59:59Z" }),
  ], true)];
  const urls = [];
  const data = fixture(context, { fetchImpl: async (url) => { urls.push(url); return pages.shift(); } });
  const result = await fetchChangelog(data.options);
  assert.equal(urls.length, 2);
  assert.equal(new URL(urls[1]).searchParams.get("page"), "2");
  assert.deepEqual(result.entries.map((entry) => entry.number), [2, 1]);
});

test("excludes unmerged PRs and PRs merged after the qualified SHA", async (context) => {
  const data = fixture(context, { fetchImpl: async () => page([
    pr(1), pr(2, { merge_commit_sha: "b".repeat(40) }), pr(3, { merged_at: null }),
  ]) });
  assert.deepEqual((await fetchChangelog(data.options)).entries.map((entry) => entry.number), [1]);
});

for (const [name, extra] of [
  ["no token", { token: "", fetchImpl: async () => { throw new Error("must not fetch"); } }],
  ["403", { fetchImpl: async () => ({ ok: false, status: 403 }) }],
  ["5xx", { fetchImpl: async () => ({ ok: false, status: 503 }) }],
  ["invalid response", { fetchImpl: async () => page({ error: "not a list" }) }],
  ["failure reading a page", { fetchImpl: async () => ({ ok: true, json: async () => { throw new Error("broken page"); } }) }],
  ["generator throws", { generator: () => { throw new Error("generator failed"); } }],
  ["timeout", { deadlineMs: 10, fetchImpl: async () => new Promise(() => {}) }],
]) {
  test(`${name} leaves no file and adds a warning`, async (context) => {
    const data = fixture(context, extra);
    assert.equal(await fetchChangelog(data.options), null);
    assert.equal(existsSync(data.out), false);
    assert.equal(data.warnings.length, 1);
    assert.match(readFileSync(data.summaryPath, "utf8"), /unavailable/);
    assert.deepEqual(readdirSync(data.directory), ["summary.md"]);
  });
}

test("failure after a complete first page never publishes partial entries", async (context) => {
  let requests = 0;
  const data = fixture(context, { fetchImpl: async () => {
    requests += 1;
    if (requests === 1) return page([pr(1)], true);
    throw new Error("network failed on page 2");
  } });
  assert.equal(await fetchChangelog(data.options), null);
  assert.equal(requests, 2);
  assert.equal(existsSync(data.out), false);
  assert.equal(data.warnings.length, 1);
});

test("the deadline covers all pages rather than restarting per request", async (context) => {
  let requests = 0;
  const data = fixture(context, { deadlineMs: 60, fetchImpl: async () => {
    requests += 1;
    if (requests === 1) {
      await new Promise((resolve) => setTimeout(resolve, 35));
      return page([pr(1)], true);
    }
    await new Promise((resolve) => setTimeout(resolve, 35));
    return page([pr(2)]);
  } });
  assert.equal(await fetchChangelog(data.options), null);
  assert.equal(existsSync(data.out), false);
  assert.match(data.warnings[0], /deadline/);
  await new Promise((resolve) => setTimeout(resolve, 40));
  assert.equal(existsSync(data.out), false);
});

test("a slow JSON response cannot write after the deadline", async (context) => {
  let resolvePage;
  const data = fixture(context, { deadlineMs: 10, fetchImpl: async () => ({
    ok: true, json: () => new Promise((resolve) => { resolvePage = resolve; }),
  }) });
  assert.equal(await fetchChangelog(data.options), null);
  resolvePage([pr(1)]);
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(existsSync(data.out), false);
});

test("write failure removes the temp file and reports unavailability", async (context) => {
  const data = fixture(context);
  writeFileSync(join(data.directory, "parent-is-a-file"), "not a directory");
  data.options.out = join(data.directory, "parent-is-a-file", "entries.json");
  assert.equal(await fetchChangelog(data.options), null);
  assert.equal(data.warnings.length, 1);
  assert.equal(existsSync(data.options.out), false);
});

test("per-PR content warnings are logged as notes, not annotations", async (context) => {
  const long = `Changelog: ${"Notes now show shipped changes. ".repeat(8)}`;
  const notes = [];
  const data = fixture(context, { note: (message) => notes.push(message), fetchImpl: async () => page([pr(1, { body: long })]) });
  const result = await fetchChangelog(data.options);
  assert.equal(result.entries.length, 1);
  assert.deepEqual(data.warnings, []);
  assert.equal(notes.length, 1);
  assert.match(notes[0], /#1: Truncated to 140 characters/);
});
