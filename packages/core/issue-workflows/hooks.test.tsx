/**
 * @vitest-environment jsdom
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import type { IssueWorkflow, Project } from "../types";
import { useProjectWithWorkflow } from "./hooks";

// A consequential write waits for `resolved`. A successful answer that cannot
// say which workflow the issue's project follows reads as "no handoff" exactly
// like a plain project does, while the server still resolves a handler. So
// `resolved` is true only when the answer is authoritative.

const NAMED = { id: "wf-1", name: "Delivery", is_default: false, steps: [] } as unknown as IssueWorkflow;
const DEFAULT = { id: "wf-default", name: "Default", is_default: true, steps: [] } as unknown as IssueWorkflow;
const project = (id: string, workflow_id: string | null) => ({ id, workflow_id }) as unknown as Project;

const listIssueWorkflows = vi.fn();
const listProjects = vi.fn();
let qc: QueryClient;

function wrapper({ children }: { children: ReactNode }) {
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

function resolveFor(projectId: string | null) {
  return renderHook(() => useProjectWithWorkflow("ws-1", projectId), { wrapper });
}

beforeEach(() => {
  qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setApiInstance({ listIssueWorkflows, listProjects } as unknown as ApiClient);
});

afterEach(() => {
  qc.clear();
  listIssueWorkflows.mockReset();
  listProjects.mockReset();
  setApiInstance(undefined as never);
});

describe("useProjectWithWorkflow resolution", () => {
  it("is unresolved when the project list does not contain the issue's project", async () => {
    listIssueWorkflows.mockResolvedValue({ workflows: [NAMED, DEFAULT], total: 2 });
    listProjects.mockResolvedValue({ projects: [project("other", null)], total: 1 });
    const { result } = resolveFor("p-1");

    await waitFor(() => expect(listProjects).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 50));

    expect(result.current.resolved).toBe(false);
    expect(result.current.workflow).toBeNull();
  });

  it("is unresolved when the project names a workflow the list does not contain", async () => {
    listIssueWorkflows.mockResolvedValue({ workflows: [DEFAULT], total: 1 });
    listProjects.mockResolvedValue({ projects: [project("p-1", "wf-1")], total: 1 });
    const { result } = resolveFor("p-1");

    await waitFor(() => expect(listProjects).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 50));

    expect(result.current.resolved).toBe(false);
  });

  it("is unresolved when a successful workflow list is empty but the project names a workflow", async () => {
    listIssueWorkflows.mockResolvedValue({ workflows: [], total: 0 });
    listProjects.mockResolvedValue({ projects: [project("p-1", "wf-1")], total: 1 });
    const { result } = resolveFor("p-1");

    await waitFor(() => expect(listProjects).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 50));

    expect(result.current.resolved).toBe(false);
  });

  it("is unresolved after the workflow list answers with a malformed body", async () => {
    // The api client turns a malformed body into a failure, not an empty list.
    listIssueWorkflows.mockRejectedValue(new Error("malformed GET /api/issue-workflows response"));
    listProjects.mockResolvedValue({ projects: [project("p-1", "wf-1")], total: 1 });
    const { result } = resolveFor("p-1");

    await waitFor(() => expect(listProjects).toHaveBeenCalled());
    await new Promise((resolve) => setTimeout(resolve, 50));

    expect(result.current.resolved).toBe(false);
  });

  it("resolves the named workflow once both lists contain it", async () => {
    listIssueWorkflows.mockResolvedValue({ workflows: [NAMED, DEFAULT], total: 2 });
    listProjects.mockResolvedValue({ projects: [project("p-1", "wf-1")], total: 1 });
    const { result } = resolveFor("p-1");

    await waitFor(() => expect(result.current.resolved).toBe(true));
    expect(result.current.workflow?.id).toBe("wf-1");
  });

  it("resolves to the Default workflow for a project that names none", async () => {
    listIssueWorkflows.mockResolvedValue({ workflows: [NAMED, DEFAULT], total: 2 });
    listProjects.mockResolvedValue({ projects: [project("p-1", null)], total: 1 });
    const { result } = resolveFor("p-1");

    await waitFor(() => expect(result.current.resolved).toBe(true));
    expect(result.current.workflow?.id).toBe("wf-default");
  });

  it("resolves to no workflow when none is stored and the project names none", async () => {
    listIssueWorkflows.mockResolvedValue({ workflows: [], total: 0 });
    listProjects.mockResolvedValue({ projects: [project("p-1", null)], total: 1 });
    const { result } = resolveFor("p-1");

    await waitFor(() => expect(result.current.resolved).toBe(true));
    expect(result.current.workflow).toBeNull();
  });

  it("resolves at once for an issue outside any project", () => {
    const { result } = resolveFor(null);

    expect(result.current.resolved).toBe(true);
    expect(result.current.workflow).toBeNull();
  });
});
