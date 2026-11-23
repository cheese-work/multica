import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

import {
  READ_ONLY_COMMANDS,
  SourceError,
  classifyCall,
  fixtureSources,
  liveSources,
  main,
  measure,
  namesPr,
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
  assert.equal(listed.length, 12);
});

test("a PR list that hits the page limit is split by date, never truncated", async () => {
  const sources = fixtureSources(DIR, { prLimit: 2 });
  const result = await measure(base(sources, { prLimit: 2 }));
  assert.deepEqual(result.agents, expected.agents);
  assert.ok(result.apiCalls.github > 2, "the window was split into several searches");
});

test("run pages are followed until the window start", async () => {
  const sources = fixtureSources(DIR, { taskPageSize: 3 });
  const result = await measure(base(sources));
  assert.deepEqual(result.agents, expected.agents);
});

// Review round 1, finding 1: a verdict counts only for the PR (or head) it names.
test("two PRs on one issue: only the PR the verdict names is reviewed", async () => {
  const result = await measure(base(fixtureSources(DIR)));
  const list = result.agents["x99-claude-sonnet-200k"].units.list;
  const by = (n) => list.find((u) => u.repo === "cheese-work/multica" && u.number === n);
  assert.equal(by(111).reviewed, true, "the verdict names PR #111 and its head");
  assert.equal(by(112).reviewed, false, "same issue, no verdict for this PR");
  assert.equal(by(113).reviewed, false, "a verdict that names no PR and no head signs nothing");
  assert.equal(by(114).reviewed, false);
});

test("namesPr matches the PR number or a head SHA prefix, not a longer number or another SHA", () => {
  const unit = { number: 111, headOid: "5".repeat(40) };
  assert.equal(namesPr("Verdict: PASS\nPR #111", unit), true);
  assert.equal(namesPr("Verdict: PASS\nhttps://github.com/o/r/pull/111", unit), true);
  assert.equal(namesPr("Verdict: PASS\nHead SHA: 5555555", unit), true, "a 7-digit prefix of the head");
  assert.equal(namesPr("Verdict: PASS\nPR #1111 and #11", unit), false);
  assert.equal(namesPr("Verdict: PASS\nHead SHA: " + "6".repeat(40), unit), false);
  assert.equal(namesPr("Verdict: PASS\nissue CHE-111", unit), false);
});

// Review round 1, finding 2: every run of the issue counts; a missing older transcript is partial coverage.
test("a unit's older run without a transcript makes the unit partial, never full", async () => {
  const result = await measure(base(fixtureSources(DIR)));
  const list = result.agents["x99-claude-sonnet-200k"].units.list;
  assert.equal(list.find((u) => u.number === 102).transcript, "partial");
  assert.equal(list.find((u) => u.number === 101).transcript, "full", "the older run of CHE-9001 has a transcript and is counted");
  assert.equal(result.agents["x99-claude-sonnet-200k"].metrics.M1.n, 4);
});

// Review round 1, finding 3: direct entrypoint calls are M3p hits, reading or listing the files is not.
test("classifyCall flags every way to run the package outside mwg.sh, and nothing else", () => {
  const kinds = (command) => classifyCall({ name: "Bash", input: { command } }).kinds;
  for (const command of [
    "npx modern-web-guidance search x",
    "pnpx modern-web-guidance@1.0.1 search x",
    "npm exec modern-web-guidance -- search x",
    "node /tmp/modern-web-guidance/modern-web.mjs search x",
    "node modern-web.mjs search x",
    "DISABLE_TELEMETRY=1 node --no-warnings ./pkg/skills/modern-web-guidance/modern-web.mjs search x",
    'node "/tmp/p/modern-web.mjs" search x',
    "cd /tmp/p && node modern-web.mjs search x",
    "./modern-web.mjs search x",
    "./node_modules/.bin/modern-web-guidance search x",
  ]) {
    assert.deepEqual(kinds(command), ["unsanctioned"], command);
  }
  for (const command of [
    'echo "HS  $SKILL_DIR/scripts/mwg.sh" | sha256sum -c --status && sh "$SKILL_DIR/scripts/mwg.sh" search "x" --skill-version v',
    "cat /tmp/modern-web-guidance/modern-web.mjs",
    "ls /tmp/modern-web-guidance",
    "cd /tmp/modern-web-guidance",
    "grep -rn modern-web.mjs .",
    "git log --oneline",
  ]) {
    assert.deepEqual(kinds(command), [], command);
  }
});

// Text that only mentions a command (a written file, a grep pattern, a commit message) is not a call; a wrapped call is.
test("classifyCall reads statements: quoted text and heredoc bodies are not calls, wrapped calls are", () => {
  const kinds = (command, at) => classifyCall({ name: "Bash", input: { command }, at }, Date.parse("2026-11-14T00:00:00Z")).kinds;
  for (const command of [
    "python3 - <<'E'\nopen('t').write('node /tmp/modern-web-guidance/modern-web.mjs search x')\nnode modern-web.mjs search y\nE",
    'cmd="node /tmp/modern-web-guidance/modern-web.mjs search x"',
    'grep -n "foo\\|modern-web-guidance" file',
    'git commit -m "docs: run node modern-web.mjs search; never npx modern-web-guidance"',
    "echo 'cd /tmp && node modern-web.mjs search x' > note.txt",
    "# node modern-web.mjs search x",
  ]) {
    assert.deepEqual(kinds(command), [], command);
  }
  for (const command of [
    'bash -lc "node /tmp/p/modern-web.mjs search x"',
    "sh -c 'npx modern-web-guidance search x'",
    "bash <<'EOF'\nnode modern-web.mjs search x\nEOF",
    "timeout 60 node /tmp/p/modern-web.mjs search x",
    "exec node /tmp/p/modern-web.mjs search x",
    "ls && npx -y modern-web-guidance@1.0.1 search x | head",
  ]) {
    assert.deepEqual(kinds(command), ["unsanctioned"], command);
  }
  const late = "2026-11-15T00:00:00Z";
  assert.deepEqual(kinds('sh "$SKILL_DIR/scripts/mwg.sh" search "x"', late), ["late"]);
  assert.deepEqual(kinds('sh "$SKILL_DIR/scripts/mwg.sh" search "x"', "2026-11-13T00:00:00Z"), []);
  assert.deepEqual(kinds("git add pilots/modern-web-guidance/scripts/mwg.sh SKILL.md", late), [], "naming the file is not running it");
  assert.equal(classifyCall({ name: "Bash", input: { command: 'sh "$SKILL_DIR/scripts/mwg.sh" retrieve id' } }).use, true);
  assert.equal(classifyCall({ name: "Bash", input: { command: 'git add scripts/mwg.sh search' } }).use, false);
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

test("a metric that does not need the failed source keeps its exact fixture value", async () => {
  const sources = fixtureSources(DIR);
  const noGithub = await measure(base({ ...sources, prList: failing("github"), prFiles: failing("github") }));
  const noTranscripts = await measure(base(sources, { transcriptRoot: join(DIR, "does-not-exist") }));
  for (const name of AGENTS) {
    assert.deepEqual(noGithub.agents[name].metrics.M3p, expected.agents[name].metrics.M3p, `${name} M3p without github`);
    for (const metric of ["M1s", "M2", "M3"]) {
      assert.deepEqual(noTranscripts.agents[name].metrics[metric], expected.agents[name].metrics[metric], `${name} ${metric} without transcripts`);
    }
  }
});

test("a transcript file that cannot be read is a source failure; a missing run transcript is only coverage", async () => {
  const result = await measure(base(fixtureSources(DIR)));
  assert.equal(result.sources.transcripts.ok, true);
  assert.equal(result.agents["x99-claude-sonnet-200k"].metrics.M3p.runsWithTranscript, 6);
});

test("an unconfirmed M3p hit is reported, and a confirmation source failure is unavailable", async () => {
  const sources = fixtureSources(DIR);
  const noMessages = { ...sources, runMessages: async () => [] };
  const result = await measure(base(noMessages));
  assert.equal(result.agents["x99-claude-sonnet-200k"].metrics.M3p.unconfirmed, 2);
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
  await sources.prBody("cheese-work/multica", 1).catch(() => {});
  await sources.issue("CHE-1").catch(() => {});
  await sources.comments("CHE-1").catch(() => {});
  await sources.tasks("agent-id", null).catch(() => {});
  await sources.issueRuns("CHE-1").catch(() => {});
  await sources.runMessages("run-id").catch(() => {});
  const allowed = READ_ONLY_COMMANDS.map((c) => c.join(" "));
  for (const call of calls) {
    const joined = call.join(" ");
    assert.ok(allowed.some((a) => joined.startsWith(a)), `unexpected command: ${joined}`);
  }
  const used = new Set(calls.map((c) => c.slice(0, c[0] === "gh" ? 3 : c[0] === "multica" && c[1] === "issue" ? 4 : 3).join(" ")));
  assert.ok(used.size >= 8, "every source call maps to a listed read-only subcommand");
  for (const forbidden of ["create", "update", "delete", "add", "set", "status", "assign", "import", "merge", "edit", "close"]) {
    assert.ok(!READ_ONLY_COMMANDS.flat().includes(forbidden), `${forbidden} is not in the allowlist`);
  }
});

test("a transient 5xx or timeout is retried; an auth failure is not; a source error is one line", async () => {
  const fail = (stderr) => Object.assign(new Error("exit 1"), { stderr });
  let calls = 0;
  const flaky = liveSources({
    exec: async () => {
      calls += 1;
      if (calls < 3) throw fail("HTTP 502: 502 Bad Gateway (https://api.github.com/graphql)");
      return { stdout: "[]", stderr: "" };
    },
    env: {},
    retryDelayMs: 0,
  });
  assert.deepEqual(await flaky.prList("cheese-work/multica", new Date(0), new Date(1000), 10), []);
  assert.equal(calls, 3);

  let authCalls = 0;
  const denied = liveSources({
    exec: async () => {
      authCalls += 1;
      throw fail('non-200 OK status code: 401 Unauthorized body: "{\r\n  \"message\": \"Bad credentials\"\r\n}"');
    },
    env: {},
    retryDelayMs: 0,
  });
  const err = await denied.prList("cheese-work/multica", new Date(0), new Date(1000), 10).catch((e) => e);
  assert.ok(err instanceof SourceError);
  assert.equal(authCalls, 1, "a 401 is not retried");
  assert.doesNotMatch(err.message, /[\r\n]/);

  let down = 0;
  const dead = liveSources({ exec: async () => { down += 1; throw fail("HTTP 503"); }, env: {}, retryDelayMs: 0 });
  await assert.rejects(() => dead.agents(), SourceError);
  assert.equal(down, 3, "gives up after three attempts");
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
