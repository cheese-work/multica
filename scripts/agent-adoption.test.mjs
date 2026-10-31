import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import {
  buildAdoption,
  checkAgentFacing,
  newCliFlags,
  parseAgentFacing,
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
