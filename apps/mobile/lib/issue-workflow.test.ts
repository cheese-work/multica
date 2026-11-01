// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { IssueWorkflow, Project } from "@multica/core/types";
import type { StatusOption } from "./issue-status";
import { issueWorkflowStatusOptions, resolveIssueWorkflow } from "./issue-workflow";

const step = (status_key: string) => ({ status_key }) as IssueWorkflow["steps"][number];
const wf = (id: string, keys: string[], is_default = false) =>
  ({ id, name: id, is_default, steps: keys.map(step) }) as IssueWorkflow;

const DEFAULT = wf("default", ["backlog", "todo", "in_progress", "done"], true);
const DEV = wf("dev", ["todo", "in_progress", "in_review", "done"]);
const QA = wf("qa", ["todo", "qa", "done"]);
const workflows = [DEFAULT, DEV, QA];
const projects = [{ id: "p-dev", workflow_id: "dev" }, { id: "p-plain", workflow_id: null }] as Project[];

const opt = (key: string): StatusOption =>
  ({ key, category: "started", label: key, color: null, icon: null }) as StatusOption;
const catalog = ["backlog", "todo", "in_progress", "in_review", "qa", "done"].map(opt);
const keys = (options: StatusOption[]) => options.map((o) => o.key);

describe("resolveIssueWorkflow", () => {
  it("prefers the issue override over its project's workflow", () => {
    expect(resolveIssueWorkflow(workflows, projects, "p-dev", "qa")).toBe(QA);
  });
  it("follows the project's named workflow without an override", () => {
    expect(resolveIssueWorkflow(workflows, projects, "p-dev", null)).toBe(DEV);
  });
  it("follows Default for a project with none, and outside any project", () => {
    expect(resolveIssueWorkflow(workflows, projects, "p-plain", null)).toBe(DEFAULT);
    expect(resolveIssueWorkflow(workflows, projects, null, null)).toBe(DEFAULT);
  });
  it("is unknown (null) while workflows are not loaded or the override is missing", () => {
    expect(resolveIssueWorkflow(undefined, projects, "p-dev", "qa")).toBeNull();
    expect(resolveIssueWorkflow(workflows, projects, "p-dev", "gone")).toBeNull();
  });
});

describe("issueWorkflowStatusOptions", () => {
  it("lists the override's statuses, not the project's", () => {
    const issue = { project_id: "p-dev", workflow_id: "qa", status: "todo" };
    expect(keys(issueWorkflowStatusOptions(catalog, workflows, projects, issue))).toEqual(["todo", "qa", "done"]);
  });
  it("lists the project's statuses after the override is reset", () => {
    const issue = { project_id: "p-dev", workflow_id: null, status: "todo" };
    expect(keys(issueWorkflowStatusOptions(catalog, workflows, projects, issue))).toEqual(["todo", "in_progress", "in_review", "done"]);
  });
  it("keeps the current status listed when its workflow omits it", () => {
    const issue = { project_id: null, workflow_id: "qa", status: "in_review" };
    expect(keys(issueWorkflowStatusOptions(catalog, workflows, projects, issue))).toContain("in_review");
  });
  it("offers the whole catalog while the workflow is unknown", () => {
    const issue = { project_id: "p-dev", workflow_id: "gone", status: "todo" };
    expect(issueWorkflowStatusOptions(catalog, workflows, projects, issue)).toBe(catalog);
  });
});
