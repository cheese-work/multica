import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import {
  READ_ONLY_COMMANDS,
  SourceError,
  fixtureSources,
  liveSources,
  main,
  measure,
  parseFindings,
  render,
} from "./mwg-baseline.mjs";

// CHE-9348 (S3a of CHE-7574): the baseline measurement script for the Modern Web Guidance pilot readout.
// The fixture suite under scripts/fixtures/mwg-baseline/ holds recorded source responses for two agents and exact
// expected results. Each expected count was worked out by hand from the fixtures, not produced by the script.
const DIR = fileURLToPath(new URL("./fixtures/mwg-baseline/", import.meta.url));
const expected = JSON.parse(readFileSync(join(DIR, "expected.json"), "utf8"));
const AGENTS = ["x99-claude-sonnet-200k", "x99-claude-sonnet-design"];
const W = {
  start: new Date(expected.window.start),
  end: new Date(expected.window.end),
  reviewCutoff: new Date(expected.window.reviewCutoff),
  mwgEnd: new Date(expected.window.mwgEnd),
};
const base = (sources, extra = {}) => ({
  sources,
  agents: AGENTS,
  ...W,
  transcriptRoot: join(DIR, "transcripts"),
  repos: ["cheese-work/multica", "cheese-work/doop"],
  ...extra,
});

test("fixture suite: every unit, review state and metric equals the hand-counted expectation", async () => {
  const result = await measure(base(fixtureSources(DIR)));
  assert.deepEqual(result.window, expected.window);
  assert.deepEqual(result.agents, expected.agents);
  assert.equal(result.exitCode, 0);
});

test("fixture suite: exclusions are exact (non-web, outside window, other agent, no unit)", async () => {
  const result = await measure(base(fixtureSources(DIR)));
  const listed = Object.values(result.agents).flatMap((a) => a.units.list.map((u) => `${u.repo}#${u.number}`));
  for (const excluded of ["cheese-work/multica#105", "cheese-work/multica#106", "cheese-work/multica#107", "cheese-work/doop#202"]) {
    assert.ok(!listed.includes(excluded), `${excluded} must not be a unit`);
  }
  assert.equal(listed.length, 8);
});

test("a PR list that hits the page limit is split by date, never truncated", async () => {
  const sources = fixtureSources(DIR, { prLimit: 2 });
  const result = await measure(base(sources, { prLimit: 2 }));
  assert.deepEqual(result.agents, expected.agents);
  assert.ok(result.apiCalls.github > 2, "the window was split into several searches");
});

test("run pages are followed until the lookback ends", async () => {
  const sources = fixtureSources(DIR, { taskPageSize: 3 });
  const result = await measure(base(sources));
  assert.deepEqual(result.agents, expected.agents);
});

test("a slice that cannot shrink below the limit is unavailable, not a low count", async () => {
  const sources = fixtureSources(DIR);
  const stuck = { ...sources, prList: async (_repo, _s, _e, limit) => Array.from({ length: limit }, (_, i) => ({ number: i + 1 })) };
  const result = await measure(base(stuck, { prLimit: 3 }));
  assert.equal(result.sources.github.ok, false);
  assert.match(result.sources.github.error, /incomplete|limit/i);
  assert.equal(result.exitCode, 2);
});

// Missing-source controls (S3a AC 3), here against the fixture window; the live controls are in the receipt.
const failing = (source) => async () => {
  throw new SourceError(source, `${source} control: simulated failure`);
};
const UNAVAILABLE_BY_SOURCE = {
  github: { units: true, M1: true, M1s: true, M2: true, M3: true, M3p: false, M4: true },
  multica: { units: true, M1: true, M1s: true, M2: true, M3: true, M3p: true, M4: true },
  transcripts: { units: false, M1: true, M1s: false, M2: false, M3: false, M3p: true, M4: true },
};
for (const source of ["github", "multica", "transcripts"]) {
  test(`control: ${source} failure makes every metric that needs it unavailable, prints no count and exits non-zero`, async () => {
    let sources = fixtureSources(DIR);
    let opts = {};
    if (source === "github") sources = { ...sources, prList: failing("github"), prFiles: failing("github") };
    if (source === "multica") sources = { ...sources, agents: failing("multica") };
    if (source === "transcripts") opts = { transcriptRoot: join(DIR, "does-not-exist") };
    const result = await measure(base(sources, opts));
    assert.equal(result.sources[source].ok, false);
    assert.notEqual(result.exitCode, 0);
    for (const name of AGENTS) {
      const agent = result.agents[name];
      for (const [metric, needs] of Object.entries(UNAVAILABLE_BY_SOURCE[source])) {
        const node = metric === "units" ? agent.units : agent.metrics[metric];
        if (needs) {
          assert.equal(node.state, "unavailable", `${name} ${metric}`);
          assert.deepEqual(Object.keys(node).sort(), ["failed", "needs", "state"], `${name} ${metric} prints no count`);
          assert.ok(node.failed.includes(source));
        } else {
          assert.notEqual(node.state, "unavailable", `${name} ${metric} does not need ${source}`);
        }
      }
    }
    const text = render(result);
    assert.match(text, /unavailable/);
    for (const line of text.split("\n").filter((l) => /unavailable/.test(l))) assert.doesNotMatch(line, /\d+\s*(hits|findings|units|%)|n=\d/);
  });
}

test("a transcript file that cannot be read is a source failure; a missing run transcript is only coverage", async () => {
  const result = await measure(base(fixtureSources(DIR)));
  assert.equal(result.sources.transcripts.ok, true);
  assert.equal(result.agents["x99-claude-sonnet-200k"].metrics.M3p.runsWithTranscript, 6);
});

test("an unconfirmed M3p hit is reported, and a confirmation source failure is unavailable", async () => {
  const sources = fixtureSources(DIR);
  const noMessages = { ...sources, runMessages: async () => [] };
  const result = await measure(base(noMessages));
  assert.equal(result.agents["x99-claude-sonnet-200k"].metrics.M3p.unconfirmed, 1);
  const broken = { ...sources, runMessages: failing("multica") };
  const failed = await measure(base(broken));
  assert.equal(failed.agents["x99-claude-sonnet-200k"].metrics.M3p.state, "unavailable");
  assert.equal(failed.exitCode, 2);
});

test("parseFindings reads Class and Rule per finding and counts web findings", () => {
  const text = [
    "Verdict: CHANGES",
    "1. **P1: a** `x`",
    "   Class: a11y",
    "   Rule: Controls need a name.",
    "2. **P2: b**",
    "   Class: failure-path",
    "   Rule: A permanent 404 must not retry.",
    "3. **P2: c**",
    "   Class: other",
    "   Rule: Layout must not shift when the font loads.",
    "4. **P2: d**",
    "   Class: other",
    "   Rule: Uses an outdated web API (document.execCommand).",
    "5. **P3: e**",
    "   Class: docs-drift",
    "   Rule: Browser support table is stale.",
  ].join("\n");
  const f = parseFindings(text);
  assert.deepEqual(f.map((x) => [x.severity, x.class, x.web]), [
    ["P1", "a11y", true],
    ["P2", "failure-path", false],
    ["P2", "other", true],
    ["P2", "other", true],
    ["P3", "docs-drift", true],
  ]);
});

test("parseFindings handles Class lines without a P header and ignores prose", () => {
  const f = parseFindings("Class: a11y\nRule: x\nClass: other\nRule: Retry policy for sync.\nThe word css in prose does not count.");
  assert.deepEqual(f.map((x) => x.web), [true, false]);
});

test("the script calls only read-only subcommands (S3a AC 7)", async () => {
  const calls = [];
  const exec = async (cmd, args) => {
    calls.push([cmd, ...args]);
    throw new Error("stub: no network in tests");
  };
  const sources = liveSources({ exec, env: {} });
  await sources.agents().catch(() => {});
  await sources.prList("cheese-work/multica", new Date(0), new Date(1), 10).catch(() => {});
  await sources.prFiles("cheese-work/multica", 1).catch(() => {});
  await sources.issue("CHE-1").catch(() => {});
  await sources.comments("CHE-1").catch(() => {});
  await sources.tasks("agent-id", null).catch(() => {});
  await sources.runMessages("run-id").catch(() => {});
  const allowed = READ_ONLY_COMMANDS.map((c) => c.join(" "));
  for (const call of calls) {
    const joined = call.join(" ");
    assert.ok(allowed.some((a) => joined.startsWith(a)), `unexpected command: ${joined}`);
  }
  const used = new Set(calls.map((c) => c.slice(0, c[0] === "gh" ? 3 : c[0] === "multica" && c[1] === "issue" ? 4 : 3).join(" ")));
  assert.ok(used.size >= 6, "every source call maps to a listed read-only subcommand");
  for (const forbidden of ["create", "update", "delete", "add", "set", "status", "assign", "import", "merge", "edit", "close"]) {
    assert.ok(!READ_ONLY_COMMANDS.flat().includes(forbidden), `${forbidden} is not in the allowlist`);
  }
});

test("main: fixture mode prints JSON and exits 0; a failing source exits 2 and prints no count", async () => {
  const out = [];
  const code = await main(
    ["--fixture-dir", DIR, "--agent", AGENTS[0], "--agent", AGENTS[1], "--start", expected.window.start, "--end", expected.window.end,
      "--review-cutoff", expected.window.reviewCutoff, "--mwg-end", expected.window.mwgEnd, "--transcript-root", join(DIR, "transcripts"), "--json"],
    {},
    { write: (s) => out.push(s) },
  );
  assert.equal(code, 0);
  assert.deepEqual(JSON.parse(out.join("")).agents, expected.agents);

  const bad = [];
  const code2 = await main(
    ["--fixture-dir", DIR, "--agent", AGENTS[0], "--start", expected.window.start, "--end", expected.window.end,
      "--review-cutoff", expected.window.reviewCutoff, "--transcript-root", join(DIR, "no-such-root"), "--json"],
    {},
    { write: (s) => bad.push(s) },
  );
  assert.equal(code2, 2);
  const parsed = JSON.parse(bad.join(""));
  assert.equal(parsed.agents[AGENTS[0]].metrics.M1.state, "unavailable");
  assert.equal("hits" in parsed.agents[AGENTS[0]].metrics.M1, false);
});
