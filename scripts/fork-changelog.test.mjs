import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, writeFileSync, existsSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

// CHE-1631 line contract and generator rules. The module is imported inside each
// test so a missing or partial implementation fails per behaviour.
const load = () => import("./fork-changelog.mjs");
const SCRIPT = new URL("./fork-changelog.mjs", import.meta.url).pathname;
const GOLDEN = JSON.parse(
  readFileSync(new URL("./fixtures/fork-changelog-golden.json", import.meta.url), "utf8"),
);

const sha = (n) => String(n).padStart(40, "0");
function pr(n, over = {}) {
  return {
    number: n,
    title: `fix(x): change ${n}`,
    body: `Changelog: Change ${n} now works.`,
    headRef: `agent/test/${n}`,
    mergeSha: sha(n),
    mergedAt: "2026-10-13T08:00:00Z",
    ...over,
  };
}
const run = async (prs, firstParent, options) => {
  const { generateEntries } = await load();
  return generateEntries({ prs, firstParent: firstParent ?? prs.map((p) => p.mergeSha) }, options);
};
const reasonOf = (result, n) => result.hidden.find((h) => h.number === n)?.reason;

// --- line grammar -----------------------------------------------------------

test("parses the first Changelog line and keeps the sentence", async () => {
  const { parseChangelogLine } = await load();
  const got = parseChangelogLine("intro\n\nChangelog: Issue lists now open instantly.\nmore");
  assert.equal(got.status, "text");
  assert.equal(got.text, "Issue lists now open instantly.");
  assert.equal(got.override, null);
  assert.equal(parseChangelogLine("Changelog:No space is fine.").text, "No space is fine.");
});

test("the section override is read from Changelog(feat|fix|perf)", async () => {
  const { parseChangelogLine } = await load();
  for (const kind of ["feat", "fix", "perf"]) {
    assert.equal(parseChangelogLine(`Changelog(${kind}): Something.`).override, kind);
  }
  assert.equal(parseChangelogLine("Changelog(docs): Something.").status, "missing");
  assert.equal(parseChangelogLine("changelog: lower case is not a line").status, "missing");
  assert.equal(parseChangelogLine("  Changelog: indented is not at line start").status, "missing");
});

test("CRLF and lone CR bodies parse like LF bodies", async () => {
  const { parseChangelogLine } = await load();
  assert.equal(parseChangelogLine("a\r\nChangelog: Works.\r\nb").text, "Works.");
  assert.equal(parseChangelogLine("a\rChangelog: Works.\rb").text, "Works.");
});

test("the first match wins and extra matches warn", async () => {
  const { parseChangelogLine } = await load();
  const got = parseChangelogLine("Changelog: First.\nChangelog: Second.");
  assert.equal(got.text, "First.");
  assert.ok(got.warnings.some((w) => /more than one|multiple/i.test(w)), got.warnings.join("|"));
});

test("lines inside HTML comments (closed or unclosed) and code fences are ignored", async () => {
  const { parseChangelogLine } = await load();
  assert.equal(parseChangelogLine("<!--\nChangelog: Hidden.\n-->").status, "missing");
  assert.equal(parseChangelogLine("<!-- open\nChangelog: Hidden.").status, "missing");
  assert.equal(parseChangelogLine("```\nChangelog: Hidden.\n```").status, "missing");
  assert.equal(parseChangelogLine("~~~md\nChangelog: Hidden.\n~~~").status, "missing");
  assert.equal(parseChangelogLine("<!-- c -->\nChangelog: Shown.").text, "Shown.");
});

test("blank values and <placeholders> count as missing, none is explicit", async () => {
  const { parseChangelogLine } = await load();
  assert.equal(parseChangelogLine("Changelog:").status, "missing");
  assert.equal(parseChangelogLine("Changelog:   ").status, "missing");
  const placeholder = parseChangelogLine("Changelog: <one user-facing sentence, or none>");
  assert.equal(placeholder.status, "missing");
  assert.ok(placeholder.warnings.some((w) => /placeholder/i.test(w)), placeholder.warnings.join("|"));
  assert.equal(parseChangelogLine("Changelog: <anything>").status, "missing");
  assert.equal(parseChangelogLine("Changelog: none").status, "none");
  assert.equal(parseChangelogLine("Changelog: None").status, "none");
  assert.equal(parseChangelogLine(null).status, "missing");
  assert.equal(parseChangelogLine(undefined).status, "missing");
});

test("text over 140 code points is cut without splitting a grapheme and warns", async () => {
  const { parseChangelogLine } = await load();
  const family = "\u{1F468}\u200D\u{1F469}\u200D\u{1F467}"; // one grapheme, 5 code points
  const cut = parseChangelogLine(`Changelog: ${"a".repeat(138)}${family}`);
  assert.equal(cut.text, "a".repeat(138), "the grapheme that crosses 140 is dropped whole");
  assert.ok(cut.warnings.some((w) => /truncat/i.test(w)), cut.warnings.join("|"));
  assert.ok([...cut.text].length <= 140);

  const exact = parseChangelogLine(`Changelog: ${"b".repeat(140)}`);
  assert.equal(exact.text.length, 140);
  assert.ok(!exact.warnings.some((w) => /truncat/i.test(w)));

  const emoji = parseChangelogLine(`Changelog: ${"\u{1F600}".repeat(150)}`);
  assert.equal([...emoji.text].length, 140);
});

// --- sanitizing ------------------------------------------------------------

test("control, bidi and zero-width characters are stripped with a warning", async () => {
  const { parseChangelogLine } = await load();
  const dirty = "Fixed\u202E evil\u2066 text\u200B here\u0007\uFEFF\u2060.";
  const got = parseChangelogLine(`Changelog: ${dirty}`);
  assert.equal(got.status, "text");
  assert.equal(got.text, "Fixed evil text here.");
  assert.ok(got.warnings.some((w) => /strip/i.test(w)), got.warnings.join("|"));
  assert.doesNotMatch(got.text, /[\u0000-\u001f\u007f-\u009f\u200b-\u200f\u202a-\u202e\u2060\u2066-\u2069\ufeff]/);
});

test("a zero-width joiner stays inside an emoji sequence and is not reported as stripped", async () => {
  const { parseChangelogLine } = await load();
  const family = "\u{1F468}\u200D\u{1F469}\u200D\u{1F467}";
  const got = parseChangelogLine(`Changelog: Families ${family} are supported.`);
  assert.equal(got.text, `Families ${family} are supported.`);
  assert.ok(!got.warnings.some((w) => /strip/i.test(w)), got.warnings.join("|"));
});

test("token-shaped sentences are hidden as parse-error, even with zero-width evasion", async () => {
  const { parseChangelogLine } = await load();
  const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r";
  for (const secret of [
    "ghp_" + "a1B2c3D4e5F6g7H8i9J0k1L2m3N4o5P6q7R8",
    "github_pat_" + "11ABCDEFG0abcdefghijkl_mnopqrstuvwx",
    "sk-" + "proj1234567890abcdefghijkl",
    "AKIA" + "IOSFODNN7EXAMPLE",
    jwt,
  ]) {
    assert.equal(parseChangelogLine(`Changelog: Rotated ${secret} today.`).status, "parse-error", secret);
  }
  for (const joiner of ["\u200B", "\u200D", "\u2060"]) {
    const split = `ghp_a1B2c3D4${joiner}e5F6g7H8i9J0k1L2m3N4o5P6`;
    assert.equal(parseChangelogLine(`Changelog: Rotated ${split} today.`).status, "parse-error");
  }
  assert.equal(parseChangelogLine("Changelog: Task-based risk-free flows are faster.").status, "text");
});

// --- sections --------------------------------------------------------------

test("section follows the title type, the override wins, other types go to Improvements", async () => {
  const { sectionFor } = await load();
  assert.equal(sectionFor("feat(x): y", null), "New Features");
  assert.equal(sectionFor("fix: y", null), "Bug Fixes");
  assert.equal(sectionFor("perf(x)!: y", null), "Improvements");
  assert.equal(sectionFor("refactor(x): y", null), "Improvements");
  assert.equal(sectionFor("no type here", null), "Improvements");
  assert.equal(sectionFor("feat(x): y", "fix"), "Bug Fixes");
  assert.equal(sectionFor("fix(x): y", "perf"), "Improvements");
  assert.equal(sectionFor("docs: y", "feat"), "New Features");
});

// --- single PR evaluation and the check text --------------------------------

test("evaluatePr gives an entry or a hide reason, never a throw", async () => {
  const { evaluatePr } = await load();
  const entry = evaluatePr(pr(1, { title: "feat(a): b", body: "Changelog: It works." }));
  assert.deepEqual([entry.kind, entry.section, entry.text], ["entry", "New Features", "It works."]);

  const cases = [
    [{ body: "" }, "missing"],
    [{ body: "Changelog: none" }, "none"],
    [{ body: "Changelog: leaked ghp_" + "a".repeat(30) }, "parse-error"],
    [{ headRef: "upstream-sync/2026-10-13", body: "Changelog: Shown." }, "upstream-sync"],
    [{ title: 'Revert "feat(a): b (#9)"', body: "Changelog: Shown." }, "reverted"],
    [{ body: "Reverts cheese-work/multica#9\n\nChangelog: Shown." }, "reverted"],
  ];
  for (const [over, reason] of cases) {
    const got = evaluatePr(pr(2, over));
    assert.deepEqual([got.kind, got.reason], ["hidden", reason], JSON.stringify(over));
  }
  assert.equal(evaluatePr(pr(3, { body: null })).reason, "missing");
});

test("formatCheck prints the exact Entry and Hidden lines", async () => {
  const { evaluatePr, formatCheck } = await load();
  const entry = formatCheck(evaluatePr(pr(1, { title: "fix: a", body: "Changelog: Works now." })));
  assert.equal(entry.line, 'Entry: [Bug Fixes] "Works now."');
  assert.equal(entry.failing, false);

  const hidden = formatCheck(evaluatePr(pr(1, { title: "chore: a", body: "" })));
  assert.equal(hidden.line, 'Hidden: missing — add "Changelog: <sentence>" to publish');
  assert.equal(hidden.failing, false, "a chore PR may stay hidden");
});

test("feat, fix and perf PRs without a valid line fail the check; none and syncs pass", async () => {
  const { evaluatePr, formatCheck } = await load();
  const check = (over) => formatCheck(evaluatePr(pr(1, over)));
  for (const type of ["feat", "fix", "perf"]) {
    assert.equal(check({ title: `${type}(a): b`, body: "" }).failing, true, `${type} missing`);
    assert.equal(check({ title: `${type}(a): b`, body: "Changelog: <placeholder>" }).failing, true);
    assert.equal(check({ title: `${type}(a): b`, body: "Changelog: tok ghp_" + "a".repeat(30) }).failing, true);
    assert.equal(check({ title: `${type}(a): b`, body: "Changelog: none" }).failing, false);
    assert.equal(check({ title: `${type}(a): b`, body: "Changelog: Works." }).failing, false);
  }
  assert.equal(check({ title: "feat: b", body: "", headRef: "upstream-sync/x" }).failing, false);
  assert.equal(check({ title: "docs: b", body: "" }).failing, false);
});

test("warnings reach the check output", async () => {
  const { evaluatePr, formatCheck } = await load();
  const got = formatCheck(evaluatePr(pr(1, { body: `Changelog: ${"x".repeat(200)}` })));
  assert.ok(got.warnings.some((w) => /truncat/i.test(w)), got.warnings.join("|"));
});

// --- generator --------------------------------------------------------------

test("an entry carries id, ISO merge time, ICT date, section and text", async () => {
  const result = await run([pr(7, { title: "feat(a): b", mergedAt: "2026-10-13T08:00:00Z" })]);
  assert.deepEqual(result.entries, [
    {
      id: "fork-pr-7",
      number: 7,
      mergedAt: "2026-10-13T08:00:00.000Z",
      date: "2026-10-13",
      section: "New Features",
      text: "Change 7 now works.",
    },
  ]);
  assert.deepEqual(result.hidden, []);
});

test("the ICT day rolls over at 17:00 UTC, including at month and year end", async () => {
  const result = await run([
    pr(1, { mergedAt: "2026-10-31T23:30:00Z" }),
    pr(2, { mergedAt: "2026-10-31T16:59:59Z" }),
    pr(3, { mergedAt: "2026-12-31T17:00:00Z" }),
  ]);
  const date = (n) => result.entries.find((e) => e.number === n).date;
  assert.equal(date(1), "2026-11-01");
  assert.equal(date(2), "2026-10-31");
  assert.equal(date(3), "2027-01-01");
});

test("entries sort newest first, ties by PR number descending", async () => {
  const result = await run([
    pr(1, { mergedAt: "2026-10-12T01:00:00Z" }),
    pr(2, { mergedAt: "2026-10-14T01:00:00Z" }),
    pr(4, { mergedAt: "2026-10-13T01:00:00Z" }),
    pr(3, { mergedAt: "2026-10-13T01:00:00Z" }),
  ]);
  assert.deepEqual(result.entries.map((e) => e.number), [2, 4, 3, 1]);
});

test("the start date is 2026-10-11 00:00 ICT and can be overridden by option or env", async () => {
  const { optionsFromEnv } = await load();
  const before = pr(1, { mergedAt: "2026-10-10T16:59:59Z" });
  const at = pr(2, { mergedAt: "2026-10-10T17:00:00Z" });
  const result = await run([before, at]);
  assert.deepEqual(result.entries.map((e) => e.number), [2]);
  assert.equal(reasonOf(result, 1), "before-start-date");

  const later = await run([before, at], undefined, { startDate: "2026-10-12" });
  assert.deepEqual(later.entries, []);
  assert.deepEqual(later.hidden.map((h) => h.reason).sort(), ["before-start-date", "before-start-date"]);

  const fromEnv = optionsFromEnv({ FORK_CHANGELOG_START: "2026-10-12", FORK_CHANGELOG_SYNC_PREFIX: "sync/" });
  assert.deepEqual(fromEnv, { startDate: "2026-10-12", syncPrefix: "sync/" });
  assert.deepEqual(optionsFromEnv({}), {});
});

test("upstream-sync heads are hidden; the prefix is overridable", async () => {
  const sync = pr(1, { headRef: "upstream-sync/2026-10-13" });
  const other = pr(2, { headRef: "sync/2026-10-13" });
  const result = await run([sync, other]);
  assert.equal(reasonOf(result, 1), "upstream-sync");
  assert.deepEqual(result.entries.map((e) => e.number), [2]);
  const custom = await run([sync, other], undefined, { syncPrefix: "sync/" });
  assert.equal(reasonOf(custom, 2), "upstream-sync");
  assert.deepEqual(custom.entries.map((e) => e.number), [1]);
});

test("only PRs whose merge commit is on the first-parent list are considered", async () => {
  const onMain = pr(1);
  const later = pr(2);
  const result = await run([onMain, later], [onMain.mergeSha]);
  assert.deepEqual(result.entries.map((e) => e.number), [1]);
  assert.deepEqual(result.hidden, [], "a PR merged after the qualified SHA is not counted at all");
  const objects = await run([onMain, later], [{ sha: later.mergeSha, message: "Merge (#2)" }]);
  assert.deepEqual(objects.entries.map((e) => e.number), [2]);
});

test("hide reasons: none, missing and parse-error", async () => {
  const result = await run([
    pr(1, { body: "Changelog: none" }),
    pr(2, { body: "no line" }),
    pr(3, { body: "Changelog: key ghp_" + "a".repeat(30) }),
    pr(4),
  ]);
  assert.equal(reasonOf(result, 1), "none");
  assert.equal(reasonOf(result, 2), "missing");
  assert.equal(reasonOf(result, 3), "parse-error");
  assert.deepEqual(result.entries.map((e) => e.number), [4]);
  assert.deepEqual(result.counts, { shown: 1, hidden: 3, byReason: { none: 1, missing: 1, "parse-error": 1 } });
});

test("a body-declared revert hides the original and the revert PR itself", async () => {
  const result = await run([
    pr(10),
    pr(11, { title: "revert(x): undo 10", body: "Reverts cheese-work/multica#10\n\nChangelog: Undone." }),
    pr(12),
  ]);
  assert.equal(reasonOf(result, 10), "reverted");
  assert.equal(reasonOf(result, 11), "reverted");
  assert.deepEqual(result.entries.map((e) => e.number), [12]);
});

test('a title-declared revert (Revert "…") hides the original by number or by title', async () => {
  const byNumber = await run([pr(10), pr(11, { title: 'Revert "fix(x): change 10 (#10)"' })]);
  assert.equal(reasonOf(byNumber, 10), "reverted");
  const byTitle = await run([pr(10), pr(11, { title: 'Revert "fix(x): change 10"' })]);
  assert.equal(reasonOf(byTitle, 10), "reverted");
  assert.equal(reasonOf(byTitle, 11), "reverted");
});

test("a first-parent commit saying 'This reverts commit <merge sha>' hides that PR", async () => {
  const result = await run(
    [pr(10), pr(12)],
    [
      { sha: pr(10).mergeSha, message: "fix(x): change 10 (#10)" },
      { sha: pr(12).mergeSha, message: "fix(x): change 12 (#12)" },
      { sha: sha(99), message: `Revert "change 10"\n\nThis reverts commit ${pr(10).mergeSha}.` },
    ],
  );
  assert.equal(reasonOf(result, 10), "reverted");
  assert.deepEqual(result.entries.map((e) => e.number), [12]);
});

test("a revert of a revert shows the original again; the revert PRs stay hidden", async () => {
  const result = await run([
    pr(10),
    pr(11, { title: 'Revert "fix(x): change 10 (#10)"', body: "Reverts cheese-work/multica#10" }),
    pr(12, { title: 'Revert "Revert "fix(x): change 10 (#10)""', body: "Reverts cheese-work/multica#11" }),
  ]);
  assert.deepEqual(result.entries.map((e) => e.number), [10]);
  assert.equal(reasonOf(result, 11), "reverted");
  assert.equal(reasonOf(result, 12), "reverted");
});

test("a commit-level revert of a revert PR restores the original", async () => {
  const result = await run(
    [pr(10), pr(11, { title: 'Revert "fix(x): change 10 (#10)"', body: "Reverts cheese-work/multica#10" })],
    [
      { sha: pr(10).mergeSha, message: "change 10" },
      { sha: pr(11).mergeSha, message: "revert 10" },
      { sha: sha(98), message: `This reverts commit ${pr(11).mergeSha}.` },
    ],
  );
  assert.deepEqual(result.entries.map((e) => e.number), [10]);
});

test("revert cycles and unknown targets do not throw", async () => {
  const result = await run([
    pr(10, { body: "Reverts cheese-work/multica#11" }),
    pr(11, { body: "Reverts cheese-work/multica#10" }),
    pr(12, { body: "Reverts cheese-work/multica#9999\nChangelog: Odd." }),
  ]);
  assert.equal(result.entries.length + result.hidden.length, 3);
});

test("warnings are collected per PR", async () => {
  const result = await run([pr(1, { body: `Changelog: ${"x".repeat(200)}` })]);
  assert.equal(result.warnings.length, 1);
  assert.equal(result.warnings[0].number, 1);
  assert.match(result.warnings[0].message, /truncat/i);
});

test("serializeEntries is stable JSON with a trailing newline", async () => {
  const { serializeEntries } = await load();
  const result = await run([pr(1)]);
  const text = serializeEntries(result);
  assert.ok(text.endsWith("\n"));
  assert.deepEqual(JSON.parse(text), { entries: result.entries });
  assert.equal(serializeEntries(result), text);
});

// --- golden: real PR bodies merged since 11 Oct 2026 -------------------------

test("golden: real merged PR bodies publish nothing until they carry a line", async () => {
  const result = await run(GOLDEN.prs, GOLDEN.firstParent);
  assert.deepEqual(result.entries, []);
  assert.deepEqual(
    result.hidden.map((h) => [h.number, h.reason]).sort((a, b) => a[0] - b[0]),
    [[213, "missing"], [215, "missing"], [220, "missing"], [221, "missing"]],
  );
});

test("golden: the same real bodies publish the exact sentence once a line is appended", async () => {
  const sentences = {
    213: "A direct message can now be renamed without starting an agent turn.",
    220: "Direct messages can be forked with a full copy and an optional first message.",
    221: "Fixed a duplicate database migration number that blocked startup.",
  };
  const prs = GOLDEN.prs
    .filter((p) => sentences[p.number])
    .map((p) => ({ ...p, body: `${p.body}\n\nChangelog: ${sentences[p.number]}\n` }));
  const result = await run(prs, GOLDEN.firstParent);
  assert.deepEqual(
    result.entries.map((e) => [e.number, e.section, e.date, e.text]),
    [
      [220, "New Features", "2026-10-14", sentences[220]],
      [221, "Bug Fixes", "2026-10-13", sentences[221]],
      [213, "New Features", "2026-10-13", sentences[213]],
    ],
  );
});

// --- CLI: local preview and PR check -----------------------------------------

function cli(args, env = {}) {
  return spawnSync(process.execPath, [SCRIPT, ...args], {
    encoding: "utf8",
    env: { PATH: process.env.PATH, ...env },
  });
}
const tmp = () => mkdtempSync(join(tmpdir(), "fork-changelog-"));

test("preview: --fixture and --out write one known entry with no credentials", () => {
  const out = join(tmp(), "preview.json");
  const fixture = new URL("./fixtures/fork-changelog-preview.json", import.meta.url).pathname;
  const res = cli(["--fixture", fixture, "--out", out]);
  assert.equal(res.status, 0, res.stderr);
  const written = JSON.parse(readFileSync(out, "utf8"));
  assert.equal(written.entries.length, 1);
  assert.equal(written.entries[0].id, "fork-pr-1");
  assert.equal(written.entries[0].text, "Issue lists now open instantly in large workspaces.");
});

test("preview: a bad fixture exits non-zero and leaves no output file", () => {
  const dir = tmp();
  const bad = join(dir, "bad.json");
  writeFileSync(bad, "{not json");
  const out = join(dir, "out.json");
  const res = cli(["--fixture", bad, "--out", out]);
  assert.notEqual(res.status, 0);
  assert.equal(existsSync(out), false);
});

test("the script header documents both modes", () => {
  const head = readFileSync(SCRIPT, "utf8").split("\n").slice(0, 30).join("\n");
  assert.match(head, /--fixture/);
  assert.match(head, /--out/);
  assert.match(head, /--event/);
});

function checkEvent(prOver, state = "open") {
  const dir = tmp();
  const event = join(dir, "event.json");
  const summary = join(dir, "summary.md");
  writeFileSync(
    event,
    JSON.stringify({
      pull_request: {
        number: 5,
        state,
        title: prOver.title,
        body: prOver.body,
        head: { ref: prOver.headRef ?? "agent/x/5" },
      },
    }),
  );
  writeFileSync(summary, "");
  const res = cli(["--event", event], { GITHUB_STEP_SUMMARY: summary });
  return { res, summary: readFileSync(summary, "utf8") };
}

test("PR check: writes the Entry line to the job summary and passes", () => {
  const { res, summary } = checkEvent({ title: "fix(a): b", body: "Changelog: It works." });
  assert.equal(res.status, 0, res.stderr);
  assert.match(summary, /Entry: \[Bug Fixes\] "It works\."/);
});

test("PR check: a feat PR without a line writes Hidden and fails", () => {
  const { res, summary } = checkEvent({ title: "feat(a): b", body: "nothing" });
  assert.equal(res.status, 1);
  assert.match(summary, /Hidden: missing — add "Changelog: <sentence>" to publish/);
});

test("PR check: warnings are annotated, a closed PR is skipped, a sync head passes", () => {
  const warn = checkEvent({ title: "fix(a): b", body: `Changelog: ${"y".repeat(200)}` });
  assert.equal(warn.res.status, 0);
  assert.match(warn.res.stdout, /::warning::.*truncat/i);

  const closed = checkEvent({ title: "feat(a): b", body: "" }, "closed");
  assert.equal(closed.res.status, 0);
  assert.equal(closed.summary.includes("Hidden"), false);

  const sync = checkEvent({ title: "feat: merge upstream", body: null, headRef: "upstream-sync/1" });
  assert.equal(sync.res.status, 0);
  assert.match(sync.summary, /Hidden: upstream-sync/);
});

test("PR check workflow: GitHub-hosted, permissions: {}, no secrets, skips closed PRs", () => {
  const raw = readFileSync(new URL("../.github/workflows/pr-changelog-check.yml", import.meta.url), "utf8");
  const wf = raw.replace(/^\s*#.*$/gm, "");
  assert.match(wf, /^permissions: \{\}$/m);
  assert.match(wf, /runs-on: ubuntu-latest/);
  assert.doesNotMatch(wf, /self-hosted|secrets\./);
  for (const type of ["opened", "edited", "synchronize", "reopened"]) assert.match(wf, new RegExp(type));
  assert.match(wf, /pull_request:/);
  assert.doesNotMatch(wf, /pull_request_target/);
  assert.match(wf, /state != 'closed'/);
  assert.match(raw, /upstream-sync\//, "the header explains why sync heads stay off X99");
  assert.match(wf, /fork-changelog\.mjs --event/);
});
