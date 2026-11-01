// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { IssueWorkflow, Project } from "@multica/core/types";
import type { StatusOption } from "./issue-status";
import { buildIssueStatusCatalog } from "./issue-status";
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
const emptyCatalog = buildIssueStatusCatalog(undefined);
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
  it("is unknown (null) while the project list is not loaded, never Default", () => {
    expect(resolveIssueWorkflow(workflows, undefined, "p-dev", null)).toBeNull();
    expect(resolveIssueWorkflow(workflows, undefined, "p-plain", null)).toBeNull();
  });
  it("is unknown (null) for a project the list lacks", () => {
    expect(resolveIssueWorkflow(workflows, projects, "p-gone", null)).toBeNull();
  });
  it("follows Default outside any project without the project list", () => {
    expect(resolveIssueWorkflow(workflows, undefined, null, null)).toBe(DEFAULT);
  });
  it("resolves an override without the project list", () => {
    expect(resolveIssueWorkflow(workflows, undefined, "p-dev", "qa")).toBe(QA);
  });
});

describe("issueWorkflowStatusOptions", () => {
  it("lists the override's statuses, not the project's", () => {
    const issue = { project_id: "p-dev", workflow_id: "qa", status: "todo" };
    expect(keys(issueWorkflowStatusOptions(catalog, emptyCatalog, workflows, projects, issue))).toEqual(["todo", "qa", "done"]);
  });
  it("lists the project's statuses after the override is reset", () => {
    const issue = { project_id: "p-dev", workflow_id: null, status: "todo" };
    expect(keys(issueWorkflowStatusOptions(catalog, emptyCatalog, workflows, projects, issue))).toEqual(["todo", "in_progress", "in_review", "done"]);
  });
  it("keeps the current status listed when its workflow omits it", () => {
    const issue = { project_id: null, workflow_id: "qa", status: "in_review" };
    expect(keys(issueWorkflowStatusOptions(catalog, emptyCatalog, workflows, projects, issue))).toContain("in_review");
  });
  it("offers the whole catalog while the workflow is unknown", () => {
    const issue = { project_id: "p-dev", workflow_id: "gone", status: "todo" };
    expect(issueWorkflowStatusOptions(catalog, emptyCatalog, workflows, projects, issue)).toBe(catalog);
  });
  it("keeps workflow steps the status options lack, with fallback labels", () => {
    const issue = { project_id: null, workflow_id: "qa", status: "todo" };
    const partial = catalog.filter((o) => o.key !== "qa");
    const listed = issueWorkflowStatusOptions(partial, emptyCatalog, workflows, projects, issue);
    expect(keys(listed)).toEqual(["todo", "qa", "done"]);
    expect(listed[1]).toMatchObject({ key: "qa", label: "qa", category: "unstarted" });
  });
  it("lists every workflow step while the status options are empty", () => {
    const issue = { project_id: null, workflow_id: "qa", status: "todo" };
    expect(keys(issueWorkflowStatusOptions([], emptyCatalog, workflows, projects, issue))).toEqual(["todo", "qa", "done"]);
  });
  it("offers the whole catalog while the project list is unknown", () => {
    const issue = { project_id: "p-dev", workflow_id: null, status: "todo" };
    expect(issueWorkflowStatusOptions(catalog, emptyCatalog, workflows, undefined, issue)).toBe(catalog);
  });
});
