import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  buildDaily,
  checkAgentFacing,
  daily,
  diffFromFiles,
  lastAdoption,
  main,
  newCliFlags,
  parseAdoptionSpec,
  parseAgentFacing,
  touchesAgentSurface,
} from "./agent-adoption.mjs";

const template = readFileSync(new URL("../.github/PULL_REQUEST_TEMPLATE.md", import.meta.url), "utf8");

// PR template field (CHE-2794 carry-over).
test("the template carries an unfilled Agent-facing field and an Adoption item line", () => {
  assert.match(template, /^Agent-facing: yes \| no$/m);
  assert.match(template, /^Adoption item:/m);
  assert.equal(parseAgentFacing(template).agentFacing, null, "the placeholder counts as missing");
});

test("parseAgentFacing reads the answer and the adoption item, ignoring comments and fences", () => {
  assert.deepEqual(parseAgentFacing("Agent-facing: yes\nAdoption item: use --property on issue create"), {
    agentFacing: "yes",
    adoptionItem: "use --property on issue create",
  });
  assert.deepEqual(parseAgentFacing("Agent-facing: No\nAdoption item:"), { agentFacing: "no", adoptionItem: null });
  assert.equal(parseAgentFacing("<!--\nAgent-facing: yes\n-->").agentFacing, null);
  assert.equal(parseAgentFacing("```\nAgent-facing: yes\n```").agentFacing, null);
  assert.equal(parseAgentFacing("Agent-facing: maybe").agentFacing, null);
  assert.equal(parseAgentFacing(undefined).agentFacing, null);
});

test("touchesAgentSurface matches CLI commands, the daemon and the agent brief, not tests or docs", () => {
  for (const path of [
    "server/cmd/multica/cmd_issue.go",
    "server/internal/daemon/daemon.go",
    "server/internal/handler/agent_decision_brief.go",
    "server/internal/handler/agent.go",
    "server/internal/issueworkflow/issueworkflow.go",
  ]) assert.equal(touchesAgentSurface([path]), true, path);
  for (const path of [
    "server/cmd/multica/cmd_issue_test.go",
    "server/internal/issueworkflow/rules_test.go",
    "apps/web/page.tsx",
    "README.md",
  ]) assert.equal(touchesAgentSurface([path]), false, path);
});

test("checkAgentFacing requires the field only when the PR touches an agent surface", () => {
  const cli = ["server/cmd/multica/cmd_issue.go"];
  assert.match(checkAgentFacing("no field here", cli)[0], /Agent-facing/);
  assert.deepEqual(checkAgentFacing("Agent-facing: no", cli), []);
  assert.match(checkAgentFacing("Agent-facing: yes", cli)[0], /Adoption item/);
  assert.deepEqual(checkAgentFacing("Agent-facing: yes\nAdoption item: add --x", cli), []);
  assert.deepEqual(checkAgentFacing("no field here", ["apps/web/page.tsx"]), []);
});

test("newCliFlags lists added flags with their derived command path", () => {
  const diff = [
    "+++ b/server/cmd/multica/main.go",
    '+\trootCmd.PersistentFlags().String("profile", "", "x")',
    "+++ b/server/cmd/multica/cmd_issue_label.go",
    '+\t\tcmd.Flags().Bool("full-id", false, "x")',
    '+\tissueListCmd.Flags().Bool("resolved", false, "x")',
    '-\tissueListCmd.Flags().String("old", "", "removed")',
    '+\t\tc.Flags().StringVar(&out, "output", "table", "x")',
  ].join("\n");
  assert.deepEqual(newCliFlags(diff), [
    { command: "", flag: "profile" },
    { command: "issue label", flag: "full-id" },
    { command: "issue list", flag: "resolved" },
    { command: "issue label", flag: "output" },
  ]);
});

// CHE-5717: the daily collection step.
const prev = "a".repeat(40);
const next = "b".repeat(40);
const agentPr = {
  number: 12,
  title: "feat(cli): property flag",
  author: "alice",
  body: "Agent-facing: yes\nAdoption item: pass --property on issue create",
  paths: ["server/cmd/multica/cmd_issue.go"],
  diff: '+\tissueCreateCmd.Flags().StringArray("property", nil, "")',
};
const otherAgentPr = { ...agentPr, number: 15, title: "feat(daemon): guard shim", author: "bob", body: "Agent-facing: yes\nAdoption item: no raw sleep", diff: "" };
const plainPr = { number: 13, title: "fix(web): copy", author: "carol", body: "Agent-facing: no", paths: ["apps/web/a.tsx"], diff: "" };
const missingFieldPr = { number: 14, title: "fix(daemon): retry", author: "dan", body: "", paths: ["server/internal/daemon/daemon.go"], diff: "" };

test("each agent-facing PR gets one line: feature, what agents change, owner", () => {
  const out = buildDaily({ prev, next, prs: [agentPr, plainPr, otherAgentPr] });
  assert.equal(out.issue, true);
  assert.match(out.body, /- #12 feat\(cli\): property flag — agents change: pass --property on issue create — owner: alice/);
  assert.match(out.body, /- #15 feat\(daemon\): guard shim — agents change: no raw sleep — owner: bob/);
  assert.doesNotMatch(out.body, /#13/);
  assert.deepEqual(out.prs, [12, 15]);
});

test("a PR is listed once, and never again when an earlier adoption issue listed it", () => {
  const twice = buildDaily({ prev, next, prs: [agentPr, agentPr] });
  assert.equal(twice.body.match(/- #12 /g).length, 1);
  const out = buildDaily({ prev, next, prs: [agentPr, otherAgentPr], seen: new Set([12]) });
  assert.deepEqual(out.prs, [15]);
  assert.equal(buildDaily({ prev, next, prs: [agentPr], seen: new Set([12]) }).issue, false);
});

test("a day with no agent-facing PR opens nothing and logs why", () => {
  const out = buildDaily({ prev, next, prs: [plainPr] });
  assert.equal(out.issue, false);
  assert.match(out.log, /no agent-facing PR in aaaaaaaaaaaa\.\.bbbbbbbbbbbb/);
  assert.equal(buildDaily({ prev, next, prs: [] }).issue, false);
});

test("only an explicit Agent-facing: yes lists a PR; a missing field is the PR check's job, not the daily issue's", () => {
  const out = buildDaily({ prev, next, prs: [missingFieldPr] });
  assert.equal(out.issue, false);
  assert.deepEqual(buildDaily({ prev, next, prs: [missingFieldPr, agentPr] }).prs, [12]);
});

test("the body is ready: marker, Estimate first line, 3 numbered acceptance criteria, host readback per new flag", () => {
  const { body, title } = buildDaily({ prev, next, prs: [agentPr] });
  assert.match(title, /^Agent adoption: C00 deploy bbbbbbbbbbbb$/);
  assert.match(body, /^Estimate: [0-9.]+ h\n/);
  assert.ok(Number(/^Estimate: ([0-9.]+) h/.exec(body)[1]) <= 2);
  assert.match(body, new RegExp(`<!-- agent-adoption-daily: sha:${next} -->`));
  const criteria = /## Acceptance criteria\n([\s\S]*?)(?=\n## |$)/.exec(body)[1];
  assert.ok((criteria.match(/^\d+\. /gm) ?? []).length >= 3);
  for (const host of ["X99", "MBP", "C00"]) assert.match(body, new RegExp(`\\| ${host} \\|.*multica issue create --help.*--property`));
});

const issue = (sha, prs, created_at, title = `Agent adoption: C00 deploy ${sha.slice(0, 12)}`) => ({
  identifier: `CHE-${prs[0]}`,
  title,
  created_at,
  description: `<!-- agent-adoption-daily: sha:${sha} -->\n${prs.map((n) => `- #${n} x`).join("\n")}`,
});
const filler = (n) => ({ identifier: `CHE-${n}`, title: `other ${n}`, created_at: "2026-11-01T00:00:00Z", description: "mentions agent-adoption-daily only in prose" });
// One list page: {issues, has_more}, as `multica issue list --output json` prints it.
const pageOf = (issues, has_more = false) => JSON.stringify({ issues, has_more });
const pages = (...all) => (offset) => all[offset] === undefined ? pageOf([]) : all[offset];

test("lastAdoption takes the newest adoption issue's SHA and the PRs it listed, walking pages until it finds it", () => {
  const seenOffsets = [];
  const list = (offset) => {
    seenOffsets.push(offset);
    return offset === 0 ? pageOf([filler(1), filler(2)], true) : pageOf([filler(3), issue("2".repeat(40), [9], "2026-11-05T00:00:00Z"), issue("1".repeat(40), [3, 4], "2026-11-01T00:00:00Z")]);
  };
  const out = lastAdoption(list);
  assert.equal(out.sha, "2".repeat(40));
  assert.deepEqual([...out.prs], [9]);
  assert.deepEqual(seenOffsets, [0, 2], "pages advance by the number of issues returned");
});

test("lastAdoption ignores a marker in an issue that is not an adoption issue", () => {
  const forged = issue("3".repeat(40), [1], "2026-11-09T00:00:00Z", "quotes the marker in prose");
  assert.equal(lastAdoption(pages(pageOf([forged, issue("2".repeat(40), [9], "2026-11-05T00:00:00Z")]))).sha, "2".repeat(40));
});

test("lastAdoption returns no SHA when every page is read and none is an adoption issue", () => {
  assert.equal(lastAdoption(pages(pageOf([filler(1)]))).sha, null);
  assert.equal(lastAdoption(pages()).sha, null);
});

test("lastAdoption fails closed on unreadable, malformed or never-ending history", () => {
  assert.throws(() => lastAdoption(() => "not json"));
  assert.throws(() => lastAdoption(() => JSON.stringify({ error: "x" })), /issues array/);
  assert.throws(() => lastAdoption(() => pageOf([filler(1)], true), { maxPages: 3 }), /page limit/);
  assert.throws(() => lastAdoption(() => pageOf([], true)), /empty page/);
});

function deps(over = {}) {
  const created = [];
  return {
    created,
    liveShort: () => "bbbbbbb",
    resolve: () => next,
    list: pages(pageOf([issue(prev, [3], "2026-11-01T00:00:00Z")])),
    initial: () => null,
    relation: () => "ahead",
    ready: () => {},
    collectPrs: () => [agentPr, plainPr],
    create: (t) => (created.push(t), "CHE-900"),
    ...over,
  };
}

test("daily: an agent-facing PR since the last adoption opens exactly one issue", () => {
  const d = deps();
  const out = daily(d);
  assert.equal(out.created, "CHE-900");
  assert.equal(d.created.length, 1);
  assert.match(d.created[0].body, /#12/);
});

test("daily: the next run, with that issue now present, opens nothing", () => {
  const d = deps({ list: pages(pageOf([issue(next, [12], "2026-11-14T00:00:00Z")])) });
  const out = daily(d);
  assert.equal(out.created, null);
  assert.equal(d.created.length, 0);
});

test("daily: the range starts at the last adoption's SHA and skips PRs it listed", () => {
  const seenCalls = [];
  const d = deps({
    list: pages(pageOf([issue(prev, [12], "2026-11-13T00:00:00Z")])),
    collectPrs: (p, n) => (seenCalls.push([p, n]), [agentPr, otherAgentPr]),
  });
  daily(d);
  assert.deepEqual(seenCalls, [[prev, next]]);
  assert.deepEqual(d.created.length, 1);
  assert.doesNotMatch(d.created[0].body, /- #12 /);
  assert.match(d.created[0].body, /- #15 /);
});

test("daily: a PR merged days ago and deployed today is in range, because the range is by SHA, not by time", () => {
  const calls = [];
  const d = deps({ collectPrs: (p, n) => (calls.push([p, n]), [{ ...agentPr, merged_at: "2026-10-01T00:00:00Z" }]) });
  assert.equal(daily(d).created, "CHE-900");
  assert.deepEqual(calls, [[prev, next]]);
});

test("daily: with no adoption issue yet, the explicit initial boundary starts the range", () => {
  const calls = [];
  const start = "c".repeat(40);
  const d = deps({ list: pages(), initial: () => start, collectPrs: (p, n) => (calls.push([p, n]), [agentPr]) });
  assert.equal(daily(d).created, "CHE-900");
  assert.deepEqual(calls, [[start, next]]);
});

test("daily: with no adoption issue and no initial boundary it fails closed and opens nothing", () => {
  const d = deps({ list: pages() });
  assert.throws(() => daily(d), /initial/);
  assert.equal(d.created.length, 0);
});

test("daily: a truncated or unreadable history fails closed and opens nothing", () => {
  const d = deps({ list: () => pageOf([filler(1)], true) });
  assert.throws(() => daily(d), /page limit/);
  assert.equal(d.created.length, 0);
  const bad = deps({ list: () => "{" });
  assert.throws(() => daily(bad));
  assert.equal(bad.created.length, 0);
});

test("daily: a no-op day opens nothing and says so", () => {
  const d = deps({ collectPrs: () => [plainPr] });
  const out = daily(d);
  assert.equal(out.created, null);
  assert.match(out.log, /no agent-facing PR/);
});

test("daily: nothing deployed since the last adoption opens nothing and collects nothing", () => {
  let collected = 0;
  const d = deps({ list: pages(pageOf([issue(next, [12], "2026-11-14T00:00:00Z")])), collectPrs: () => (collected++, []) });
  assert.equal(daily(d).created, null);
  assert.equal(collected, 0);
});

test("daily: an unreadable live revision fails instead of creating a possible duplicate", () => {
  const d = deps({ liveShort: () => { throw new Error("C00 unreachable"); } });
  assert.throws(() => daily(d), /C00 unreachable/);
  assert.equal(d.created.length, 0);
});

test("daily: a deploy that fails its health readback opens nothing", () => {
  const d = deps({ ready: () => { throw new Error("C00 /readyz returned 503"); } });
  assert.throws(() => daily(d), /503/);
  assert.equal(d.created.length, 0);
});

test("daily: a rollback or an unrelated revision opens nothing", () => {
  for (const relation of ["behind", "diverged"]) {
    const d = deps({ relation: () => relation });
    assert.throws(() => daily(d), /not a descendant|rollback/i, relation);
    assert.equal(d.created.length, 0, relation);
  }
});

test("daily: the health readback is for the exact revision the range ends at", () => {
  const asked = [];
  daily(deps({ ready: (sha) => asked.push(sha) }));
  assert.deepEqual(asked, [next]);
});

test("main usage errors exit 2", () => {
  assert.equal(main(["nope"], {}), 2);
});

test("check mode reads the PR event and the changed-file list, never the PR checkout", () => {
  const dir = mkdtempSync(join(tmpdir(), "adoption-check-"));
  const file = (name, text) => (writeFileSync(join(dir, name), text), join(dir, name));
  const files = file("files.txt", "server/cmd/multica/cmd_issue.go\nREADME.md\n");
  const run = (body, list = files) =>
    main(["check", "--event", file("event.json", JSON.stringify({ pull_request: { body } })), "--files", list], {});
  assert.equal(run("Agent-facing: no"), 0);
  assert.equal(run("Agent-facing: yes\nAdoption item: use --x"), 0);
  assert.equal(run("nothing"), 1);
  assert.equal(run("Agent-facing: yes"), 1);
  assert.equal(run("nothing", file("docs.txt", "README.md\n")), 0);
  assert.equal(run("Agent-facing: no", join(dir, "missing.txt")), 1, "an unreadable file list fails closed");
});

// CHE-10123 (CHE-9922 W5): per-deploy adoption issue lists features, signals, owners and result lines.
const specPr = {
  ...agentPr,
  body: [
    "Refs CHE-9561",
    "Agent-facing: yes",
    "Adoption item: pass --property on issue create",
    "Adoption signal: `issue create --property` used",
    "Adoption base: issues created by agents in 7 days, at least 20",
    "Adoption target: 3 or more issues by 2 or more agents in 7 days",
  ].join("\n"),
};
// The result-line parser of adoption-reconcile (multica-dotfiles, CHE-10121), reduced to its field rules.
const parsesAsResult = (line) => {
  const t = line.trim().replace(/^- /, "");
  const kv = Object.fromEntries(t.split(" ").slice(1).map((p) => p.split("=")));
  return t.startsWith("ADOPTION-RESULT ") && /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(kv.feature ?? "") && /^[A-Z]+-\d+$/.test(kv.issue ?? "") &&
    /^[0-9a-f]{40}$/.test(kv.deployed_sha ?? "") && /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/.test(kv.window_start ?? "") &&
    /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/.test(kv.window_end ?? "") && ["PASS", "FAIL", "unknown"].includes(kv.verdict);
};
const rowOf = (body, n) => new RegExp(`^- #${n} [^\\n]*(?:\\n  [^\\n]*)*`, "m").exec(body)[0];

test("the template carries Adoption signal, base, target and old-path lines, all unfilled", () => {
  for (const key of ["signal", "base", "target", "old-path"]) assert.match(template, new RegExp(`^Adoption ${key}:`, "m"), key);
  assert.deepEqual(parseAdoptionSpec(template), { signal: null, base: null, target: null, oldPath: null });
});

test("parseAdoptionSpec reads the lines, ignoring placeholders, comments and fences", () => {
  assert.deepEqual(parseAdoptionSpec(specPr.body), {
    signal: "`issue create --property` used",
    base: "issues created by agents in 7 days, at least 20",
    target: "3 or more issues by 2 or more agents in 7 days",
    oldPath: null,
  });
  assert.equal(parseAdoptionSpec("Adoption signal: <one positive-use signal>").signal, null);
  assert.equal(parseAdoptionSpec("<!--\nAdoption target: x\n-->").target, null);
  assert.equal(parseAdoptionSpec("```\nAdoption target: x\n```").target, null);
  assert.equal(parseAdoptionSpec("Adoption old-path: 0 calls of `issue create --field`").oldPath, "0 calls of `issue create --field`");
  assert.equal(parseAdoptionSpec(undefined).signal, null);
});

test("a PR with signal, base and target is listed with them, its owner and one pending result slot", () => {
  const row = rowOf(buildDaily({ prev, next, prs: [specPr] }).body, 12);
  assert.match(row, /^- #12 feat\(cli\): property flag — agents change: pass --property on issue create — owner: alice/);
  assert.match(row, /\n  - signal: `issue create --property` used/);
  assert.match(row, /\n  - base: issues created by agents in 7 days, at least 20/);
  assert.match(row, /\n  - target: 3 or more issues by 2 or more agents in 7 days/);
  assert.match(row, /\n  - result: pending/);
  assert.doesNotMatch(row, /result: unknown/);
  const slot = /\n  - result line: (ADOPTION-RESULT .*)/.exec(row)[1];
  assert.match(slot, new RegExp(`^ADOPTION-RESULT feature=pr-12 issue=CHE-9561 deployed_sha=${next} `));
  assert.match(slot, /verdict=<PASS\|FAIL\|unknown>$/);
  assert.equal(parsesAsResult(slot), false, "a slot is never a result the reconciler reads");
});

test("a PR without a signal, base or target gets result unknown with each missing line named", () => {
  for (const [drop, reason] of [["signal", "signal"], ["base", "base"], ["target", "target"]]) {
    const body = specPr.body.split("\n").filter((l) => !l.startsWith(`Adoption ${drop}:`)).join("\n");
    const row = rowOf(buildDaily({ prev, next, prs: [{ ...specPr, body }] }).body, 12);
    assert.match(row, new RegExp(`\\n  - result: unknown — reason: no ${reason} line`), drop);
    assert.doesNotMatch(row, /result: pending/, drop);
  }
  const none = rowOf(buildDaily({ prev, next, prs: [agentPr] }).body, 12);
  assert.match(none, /result: unknown — reason: no signal line, no base line, no target line/);
  assert.match(none, /issue=<KEY-N>/, "no Closes or Refs line: the issue key stays a placeholder");
});

test("an old-path count alone never gives PASS: it is shown as optional and the result stays unknown", () => {
  const row = rowOf(buildDaily({ prev, next, prs: [{ ...agentPr, body: `${agentPr.body}\nAdoption old-path: 0 calls of the old flag` }] }).body, 12);
  assert.match(row, /\n  - old path \(optional, never enough alone\): 0 calls of the old flag/);
  assert.match(row, /result: unknown — reason: no signal line/);
  assert.doesNotMatch(row, /result: pending/);
});

test("the issue body states evidence rules A1 to A4 and the 7-day window with one extension", () => {
  const { body } = buildDaily({ prev, next, prs: [specPr] });
  for (const rule of ["A1", "A2", "A3", "A4"]) assert.match(body, new RegExp(`^- ${rule}\\. `, "m"), rule);
  assert.match(body, /all four hold/i);
  assert.match(body, /7 days from the `deployed` write/);
  assert.match(body, /extends once by 7 days/);
  assert.match(body, /then report the gap/);
  assert.match(body, /below its minimum/i);
  assert.match(body, /one ADOPTION-RESULT line per feature/);
  assert.match(body, /adoption-reconcile\.md/);
});

test("the result-line format in the body is one the reconciler's parser accepts once filled", () => {
  const filled = `ADOPTION-RESULT feature=pr-12 issue=CHE-9561 deployed_sha=${next} window_start=2026-11-27T06:00:00Z window_end=2026-12-04T06:00:00Z verdict=PASS`;
  assert.equal(parsesAsResult(filled), true);
  const { body } = buildDaily({ prev, next, prs: [specPr, otherAgentPr] });
  for (const line of body.split("\n")) assert.equal(parsesAsResult(line), false, `line must not parse as a result: ${line}`);
});

test("a new-format issue still gives lastAdoption its SHA and only the PR rows, not the sub-lines", () => {
  const { body } = buildDaily({ prev, next, prs: [specPr, otherAgentPr] });
  const found = lastAdoption(pages(pageOf([{ identifier: "CHE-1", title: "Agent adoption: C00 deploy bbbbbbbbbbbb", description: body }])));
  assert.equal(found.sha, next);
  assert.deepEqual([...found.prs].sort((a, b) => a - b), [12, 15]);
});

test("diffFromFiles rebuilds the CLI diff from the PR files API when the PR diff is too large (PR 1834, 2913 files)", () => {
  const files = [
    { filename: "server/cmd/multica/cmd_issue.go", patch: '@@ -1 +1,2 @@\n+\tissueListCmd.Flags().Bool("resolved", false, "x")' },
    { filename: "apps/web/a.tsx", patch: "@@ -1 +1 @@\n+x" },
    { filename: "server/cmd/multica/binary.png" },
  ];
  const diff = diffFromFiles(files);
  assert.deepEqual(newCliFlags(diff), [{ command: "issue list", flag: "resolved" }]);
  assert.doesNotMatch(diff, /apps\/web/);
});
