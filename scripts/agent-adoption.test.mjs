import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  buildDaily,
  checkAgentFacing,
  daily,
  lastAdoption,
  main,
  newCliFlags,
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
  assert.match(out.body, /- #12 feat\(cli\): property flag — agents change: pass --property on issue create — owner: @alice/);
  assert.match(out.body, /- #15 feat\(daemon\): guard shim — agents change: no raw sleep — owner: @bob/);
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

test("a PR with a missing field is judged by the path heuristic and flagged", () => {
  const out = buildDaily({ prev, next, prs: [missingFieldPr] });
  assert.match(out.body, /#14.*field missing, path heuristic; author to confirm.*owner: @dan/);
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

const issue = (sha, prs, created_at) => ({
  identifier: `CHE-${prs[0]}`,
  created_at,
  description: `<!-- agent-adoption-daily: sha:${sha} -->\n${prs.map((n) => `- #${n} x`).join("\n")}`,
});
const search = (issues) => JSON.stringify({ issues });

test("lastAdoption takes the newest marker's SHA and every PR already listed", () => {
  const out = lastAdoption(search([
    issue("1".repeat(40), [3, 4], "2026-11-01T00:00:00Z"),
    issue("2".repeat(40), [9], "2026-11-05T00:00:00Z"),
    { identifier: "CHE-1", created_at: "2026-11-09T00:00:00Z", description: "mentions agent-adoption-daily only in prose" },
  ]));
  assert.equal(out.sha, "2".repeat(40));
  assert.deepEqual([...out.prs].sort(), [3, 4, 9]);
  assert.equal(lastAdoption(search([])).sha, null);
  assert.throws(() => lastAdoption("not json"));
  assert.throws(() => lastAdoption(JSON.stringify({ error: "x" })));
});

function deps(over = {}) {
  const created = [];
  return {
    created,
    liveShort: () => "bbbbbbb",
    resolve: () => next,
    search: () => search([]),
    bootstrap: () => prev,
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
  const d = deps({ search: () => search([issue(next, [12], "2026-11-14T00:00:00Z")]) });
  const out = daily(d);
  assert.equal(out.created, null);
  assert.equal(d.created.length, 0);
});

test("daily: the range starts at the last adoption's SHA and skips PRs it listed", () => {
  const seenCalls = [];
  const d = deps({
    search: () => search([issue(prev, [12], "2026-11-13T00:00:00Z")]),
    collectPrs: (p, n) => (seenCalls.push([p, n]), [agentPr, otherAgentPr]),
  });
  daily(d);
  assert.deepEqual(seenCalls, [[prev, next]]);
  assert.deepEqual(d.created.length, 1);
  assert.doesNotMatch(d.created[0].body, /- #12 /);
  assert.match(d.created[0].body, /- #15 /);
});

test("daily: a no-op day opens nothing and says so", () => {
  const d = deps({ collectPrs: () => [plainPr] });
  const out = daily(d);
  assert.equal(out.created, null);
  assert.match(out.log, /no agent-facing PR/);
});

test("daily: nothing deployed since the last adoption opens nothing and collects nothing", () => {
  let collected = 0;
  const d = deps({ search: () => search([issue(next, [12], "2026-11-14T00:00:00Z")]), collectPrs: () => (collected++, []) });
  assert.equal(daily(d).created, null);
  assert.equal(collected, 0);
});

test("daily: an unreadable live revision or search fails instead of creating a possible duplicate", () => {
  assert.throws(() => daily(deps({ liveShort: () => { throw new Error("C00 unreachable"); } })), /C00 unreachable/);
  assert.throws(() => daily(deps({ search: () => "{" })));
  const d = deps({ search: () => "{" });
  assert.throws(() => daily(d));
  assert.equal(d.created.length, 0);
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
