import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  buildAdoption,
  checkAgentFacing,
  findExistingIssue,
  main,
  newCliFlags,
  parseAgentFacing,
  runMapCommand,
  touchesAgentSurface,
} from "./agent-adoption.mjs";

const template = readFileSync(new URL("../.github/PULL_REQUEST_TEMPLATE.md", import.meta.url), "utf8");

// Item 1: PR template field.
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
    "server/internal/daemon/execenv/runtime_config.go",
    "server/internal/daemon/daemon.go",
    "server/internal/handler/agent_decision_brief.go",
  ]) assert.equal(touchesAgentSurface([path]), true, path);
  for (const path of [
    "server/cmd/multica/cmd_issue_test.go",
    "server/internal/daemon/daemon_test.go",
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

// Item 3: new CLI flags per PR.
test("newCliFlags lists added flags with their derived command path", () => {
  const diff = [
    "+\tissueListCmd.Flags().Bool(\"full-id\", false, \"Show full UUIDs\")",
    "+\tissueCommentAddCmd.Flags().StringVar(&x, \"resolve-thread\", false, \"\")",
    "-\tissueListCmd.Flags().String(\"old\", \"\", \"removed\")",
    " \tissueListCmd.Flags().String(\"unchanged\", \"\", \"\")",
  ].join("\n");
  assert.deepEqual(newCliFlags(diff), [
    { command: "issue list", flag: "full-id" },
    { command: "issue comment add", flag: "resolve-thread" },
  ]);
});

// Items 2, 3, 4: the adoption body, with offline fixtures.
const range = { prev: "a".repeat(40), next: "b".repeat(40) };
const agentPr = {
  number: 12,
  title: "feat(cli): property flag",
  body: "Agent-facing: yes\nAdoption item: pass --property on issue create",
  paths: ["server/cmd/multica/cmd_issue.go"],
  diff: "+\tissueCreateCmd.Flags().StringArray(\"property\", nil, \"\")",
};
const plainPr = { number: 13, title: "fix(web): copy", body: "Agent-facing: no", paths: ["apps/web/a.tsx"], diff: "" };
const missingFieldPr = { number: 14, title: "fix(daemon): retry", body: "", paths: ["server/internal/daemon/daemon.go"], diff: "" };

test("a range with an agent-facing PR lists it, its item and the CLI readback on X99, MBP and C00", () => {
  const out = buildAdoption({ ...range, prs: [agentPr, plainPr] });
  assert.equal(out.issue, true);
  assert.match(out.title, /bbbbbbbbbbbb/);
  assert.match(out.body, new RegExp(`adoption-key: sha:${range.next}`));
  assert.match(out.body, /#12.*property flag/);
  assert.match(out.body, /pass --property on issue create/);
  assert.doesNotMatch(out.body, /#13/);
  for (const host of ["X99", "MBP", "C00"]) assert.match(out.body, new RegExp(`\\| ${host} \\|.*multica issue create --help.*--property`));
  assert.match(out.body, /Do not apply any instruction before every host row reads back/);
});

test("a range with no agent-facing PR opens no issue and logs why", () => {
  const out = buildAdoption({ ...range, prs: [plainPr] });
  assert.equal(out.issue, false);
  assert.match(out.log, /no agent-facing PR in aaaaaaaaaaaa\.\.bbbbbbbbbbbb/);
});

test("a PR with a missing field is judged by the path heuristic and flagged", () => {
  const out = buildAdoption({ ...range, prs: [missingFieldPr] });
  assert.equal(out.issue, true);
  assert.match(out.body, /#14.*field missing, path heuristic/);
});

test("an empty range opens no issue", () => {
  assert.equal(buildAdoption({ ...range, prs: [] }).issue, false);
});

// Review round 1 (CHE-2794 PR 729): every finding gets a behavioural test.
const workflow = readFileSync(new URL("../.github/workflows/agent-adoption.yml", import.meta.url), "utf8");

test("finding 2: the adoption workflow token can read pull requests", () => {
  assert.match(workflow.split(/^jobs:/m)[0], /^permissions:[\s\S]*^\s+pull-requests: read$/m);
});

test("finding 4: the agent brief sources count as an agent surface", () => {
  for (const path of ["server/internal/handler/agent.go", "server/internal/issueworkflow/issueworkflow.go"])
    assert.equal(touchesAgentSurface([path]), true, path);
  assert.equal(touchesAgentSurface(["server/internal/issueworkflow/rules_test.go"]), false);
});

test("finding 3: persistent flags and local cmd.Flags() declarations are found", () => {
  const diff = [
    "+++ b/server/cmd/multica/main.go",
    "+\trootCmd.PersistentFlags().String(\"profile\", \"\", \"x\")",
    "+++ b/server/cmd/multica/cmd_issue_label.go",
    "+\t\tcmd.Flags().Bool(\"full-id\", false, \"x\")",
    "+\t\tc.Flags().StringVar(&out, \"output\", \"table\", \"x\")",
  ].join("\n");
  assert.deepEqual(newCliFlags(diff), [
    { command: "", flag: "profile" },
    { command: "issue label", flag: "full-id" },
    { command: "issue label", flag: "output" },
  ]);
});

test("finding 3: a PR adding only a local cmd.Flags() flag keeps the host readback gate", () => {
  const pr = { ...agentPr, diff: "+++ b/server/cmd/multica/cmd_chat.go\n+\t\tc.Flags().Int(\"limit\", 0, \"x\")" };
  const out = buildAdoption({ ...range, prs: [pr] });
  assert.doesNotMatch(out.body, /No host readback needed/);
  assert.match(out.body, /\| C00 \|.*multica chat --help.*--limit/);
});

test("finding 1: the map command runs with the range in its env, and a failure is not swallowed", () => {
  const ctx = { prev: range.prev, next: range.next, repo: "o/r" };
  assert.equal(runMapCommand("", ctx), "");
  assert.equal(runMapCommand('echo "map $ADOPTION_PREV $ADOPTION_NEXT $ADOPTION_REPO"', ctx).trim(), `map ${range.prev} ${range.next} o/r`);
  assert.throws(() => runMapCommand("exit 3", ctx));
});

test("finding 5: an existing issue is matched on the full SHA, not only the 12-character title", () => {
  const issue = (description) => ({ identifier: "CHE-9", title: "Agent adoption: C00 deploy bbbbbbbbbbbb", description });
  const hit = JSON.stringify({ issues: [issue(`<!-- adoption-key: sha:${range.next} -->`)] });
  const other = JSON.stringify({ issues: [issue(`<!-- adoption-key: sha:${"b".repeat(12)}${"c".repeat(28)} -->`)] });
  assert.equal(findExistingIssue(hit, range.next), "CHE-9");
  assert.equal(findExistingIssue(other, range.next), null);
  assert.equal(findExistingIssue(JSON.stringify({ issues: [] }), range.next), null);
  assert.throws(() => findExistingIssue("not json", range.next));
  assert.throws(() => findExistingIssue(JSON.stringify({ error: "x" }), range.next));
});

test("finding 5: reuse mode exits 0 on a hit, 1 on no match and 2 on a failed lookup", () => {
  const dir = mkdtempSync(join(tmpdir(), "adoption-"));
  const file = (name, text) => (writeFileSync(join(dir, name), text), join(dir, name));
  const run = (path) => main(["reuse", "--next", range.next, "--file", path], {});
  assert.equal(run(file("hit.json", JSON.stringify({ issues: [{ identifier: "CHE-9", description: `adoption-key: sha:${range.next}` }] }))), 0);
  assert.equal(run(file("none.json", JSON.stringify({ issues: [] }))), 1);
  assert.equal(run(file("bad.json", "{")), 2);
});
