/**
 * @vitest-environment jsdom
 *
 * The editor keeps unsaved edits when the saved workflow refreshes under it.
 * A refresh that changes only bookkeeping (updated_at, the status count) leaves
 * the edits alone. A refresh that changes the workflow's content, because
 * another admin saved it, keeps the edits too, says so, and holds Save until
 * the editor chooses between the two versions.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError } from "@multica/core/api";
import { issueStatusKeys } from "@multica/core/issue-statuses/queries";
import { issueWorkflowKeys } from "@multica/core/issue-workflows";
import { projectListOptions } from "@multica/core/projects/queries";
import type { IssueStatusEntry, IssueWorkflow } from "@multica/core/types";
import { renderWithI18n } from "../test/i18n";
import { WorkflowEditorPage } from "./workflow-editor-page";

const apiMock = vi.hoisted(() => ({
  updateIssueWorkflow: vi.fn(),
  listIssueWorkflows: vi.fn(),
  createIssueWorkflow: vi.fn(),
}));
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: apiMock,
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useCurrentWorkspace: () => ({ id: "ws-1", slug: "acme" }),
}));
vi.mock("@multica/core/workspace/hooks", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/workspace/hooks")>()),
  useActorName: () => ({ getActorName: () => "Someone" }),
}));

function workflow(overrides: Partial<IssueWorkflow> = {}): IssueWorkflow {
  const step = (status_key: string) => ({
    status_key,
    handler: { type: "none" as const },
    instructions: "",
  });
  return {
    id: "wf-1",
    workspace_id: "ws-1",
    name: "Delivery",
    description: "Ship it",
    is_default: false,
    steps: ["backlog", "todo", "in_progress", "blocked", "done", "cancelled"].map(step),
    project_ids: [],
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    version: "v-2026-01-01",
    ...overrides,
  } as IssueWorkflow;
}

function statusEntry(key: string): IssueStatusEntry {
  return {
    id: key, workspace_id: "ws-1", key, name: key, description: "",
    category: "started", color: "#888888", is_system: true, archived_at: null,
  } as IssueStatusEntry;
}

describe("workflow editor baseline refresh", () => {
  let queryClient: QueryClient;
  const statuses = ["backlog", "todo", "in_progress", "blocked", "done", "cancelled"].map(statusEntry);

  // Query observers are notified on a later tick, so flush it inside act.
  const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
  const publish = (next: IssueWorkflow) =>
    act(async () => {
      queryClient.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [next], total: 1 });
      await settle();
    });

  beforeEach(() => {
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses });
    queryClient.setQueryData(projectListOptions("ws-1").queryKey, { projects: [], total: 0 });
    queryClient.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [workflow()], total: 1 });
    renderWithI18n(
      <QueryClientProvider client={queryClient}>
        <WorkflowEditorPage workflowId="wf-1" seed={null} canEdit onBack={() => {}} onOpen={() => {}} />
      </QueryClientProvider>,
    );
  });

  afterEach(cleanup);

  const nameField = () => screen.getByDisplayValue(/Delivery/) as HTMLInputElement;
  const saveButton = () => screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;

  it("keeps dirty edits when only updated_at changes", async () => {
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    expect(saveButton().disabled).toBe(false);

    await publish(workflow({ updated_at: "2026-01-02T00:00:00Z" }));

    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
    expect(saveButton().disabled).toBe(false);
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps dirty edits when a status is added to the catalog", async () => {
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });

    await act(async () => {
      queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses: [...statuses, statusEntry("in_review")] });
      await settle();
    });

    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
    expect(saveButton().disabled).toBe(false);
  });

  it("keeps dirty edits and holds Save when another admin changes the workflow", async () => {
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });

    await publish(workflow({ description: "Changed elsewhere", updated_at: "2026-01-02T00:00:00Z", version: "v-2026-01-02" }));

    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(saveButton().disabled).toBe(true);
  });

  it("loads the latest version on request and drops the edits", async () => {
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    await publish(workflow({ name: "Delivery by admin", updated_at: "2026-01-02T00:00:00Z" }));

    fireEvent.click(screen.getByRole("button", { name: "Load latest" }));

    expect(screen.getByDisplayValue("Delivery by admin")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps the edits on request and re-enables Save", async () => {
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    await publish(workflow({ description: "Changed elsewhere", updated_at: "2026-01-02T00:00:00Z", version: "v-2026-01-02" }));

    fireEvent.click(screen.getByRole("button", { name: "Keep my edits" }));

    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(saveButton().disabled).toBe(false);
  });

  it("refreshes a clean editor to the new version", async () => {
    await publish(workflow({ name: "Delivery by admin", updated_at: "2026-01-02T00:00:00Z" }));

    expect(screen.getByDisplayValue("Delivery by admin")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  // The server serializes workflow writes but a whole-workflow payload still
  // replaces whatever another admin saved first, unless the save names the
  // version it edited.
  describe("saving against a version", () => {
    afterEach(() => {
      apiMock.updateIssueWorkflow.mockReset();
      apiMock.listIssueWorkflows.mockReset();
    });

    it("sends the version of the workflow the editor started from", async () => {
      const update = apiMock.updateIssueWorkflow.mockResolvedValue(workflow());
      fireEvent.change(nameField(), { target: { value: "Delivery v2" } });

      await act(async () => {
        fireEvent.click(saveButton());
        await settle();
      });

      expect(update).toHaveBeenCalledTimes(1);
      expect(update.mock.calls[0]![0]).toBe("wf-1");
      expect(update.mock.calls[0]![1]).toMatchObject({ expected_version: "v-2026-01-01" });
    });

    it("keeps the edits and offers the choice when the server says another admin saved first", async () => {
      apiMock.updateIssueWorkflow.mockRejectedValue(
        new ApiError("conflict", 409, "Conflict", { code: "workflow_changed_elsewhere" }),
      );
      apiMock.listIssueWorkflows.mockResolvedValue({
        workflows: [workflow({ description: "Changed elsewhere", updated_at: "2026-01-02T00:00:00Z", version: "v-2026-01-02" })],
        total: 1,
      });
      fireEvent.change(nameField(), { target: { value: "Delivery v2" } });

      await act(async () => {
        fireEvent.click(saveButton());
        await settle();
      });

      await waitFor(() => expect(screen.getByRole("alert")).toBeTruthy());
      expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
      expect(saveButton().disabled).toBe(true);
    });

    it("saves the kept edits against the latest version after the editor chooses to keep them", async () => {
      const update = apiMock.updateIssueWorkflow
        .mockRejectedValueOnce(new ApiError("conflict", 409, "Conflict", { code: "workflow_changed_elsewhere" }))
        .mockResolvedValue(workflow());
      apiMock.listIssueWorkflows.mockResolvedValue({
        workflows: [workflow({ description: "Changed elsewhere", updated_at: "2026-01-02T00:00:00Z", version: "v-2026-01-02" })],
        total: 1,
      });
      fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
      await act(async () => {
        fireEvent.click(saveButton());
        await settle();
      });
      await waitFor(() => expect(screen.getByRole("alert")).toBeTruthy());

      fireEvent.click(screen.getByRole("button", { name: "Keep my edits" }));
      await act(async () => {
        fireEvent.click(saveButton());
        await settle();
      });

      expect(update).toHaveBeenCalledTimes(2);
      expect(update.mock.calls[1]![1]).toMatchObject({ expected_version: "v-2026-01-02" });
    });
  });
});

// updated_at has one-second resolution and version has full precision, so two
// saves inside one second share updated_at and differ in version. The token the
// editor submits must belong to the same accepted revision as the steps it
// submits.
describe("workflow editor version binding", () => {
  let queryClient: QueryClient;
  const statuses = ["backlog", "todo", "in_progress", "blocked", "done", "cancelled"].map(statusEntry);
  const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
  const publish = (next: IssueWorkflow) =>
    act(async () => {
      queryClient.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [next], total: 1 });
      await settle();
    });
  const sameSecond = { updated_at: "2026-01-01T00:00:05Z" };
  const nameField = () => screen.getByDisplayValue(/Delivery/) as HTMLInputElement;
  const saveButton = () => screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
  const save = () =>
    act(async () => {
      fireEvent.click(saveButton());
      await settle();
    });

  beforeEach(() => {
    apiMock.updateIssueWorkflow.mockReset();
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses });
    queryClient.setQueryData(projectListOptions("ws-1").queryKey, { projects: [], total: 0 });
    queryClient.setQueryData(issueWorkflowKeys.list("ws-1"), {
      workflows: [workflow({ ...sameSecond, version: "2026-01-01T00:00:05.100000Z" })],
      total: 1,
    });
    renderWithI18n(
      <QueryClientProvider client={queryClient}>
        <WorkflowEditorPage workflowId="wf-1" seed={null} canEdit onBack={() => {}} onOpen={() => {}} />
      </QueryClientProvider>,
    );
  });

  afterEach(cleanup);

  it("holds Save when another admin saves within the same second with different content", async () => {
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });

    await publish(
      workflow({ ...sameSecond, version: "2026-01-01T00:00:05.700000Z", description: "Changed in the same second" }),
    );

    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
    expect(saveButton().disabled).toBe(true);
    expect(apiMock.updateIssueWorkflow).not.toHaveBeenCalled();
  });

  it("saves kept edits against the version of the content the editor kept them over", async () => {
    apiMock.updateIssueWorkflow.mockResolvedValue(workflow());
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    await publish(
      workflow({ ...sameSecond, version: "2026-01-01T00:00:05.700000Z", description: "Changed in the same second" }),
    );

    fireEvent.click(screen.getByRole("button", { name: "Keep my edits" }));
    await save();

    expect(apiMock.updateIssueWorkflow).toHaveBeenCalledTimes(1);
    expect(apiMock.updateIssueWorkflow.mock.calls[0]![1]).toMatchObject({
      expected_version: "2026-01-01T00:00:05.700000Z",
    });
  });

  it("reseeds a clean editor from a same-second save", async () => {
    await publish(
      workflow({ ...sameSecond, version: "2026-01-01T00:00:05.700000Z", name: "Delivery by admin" }),
    );

    expect(screen.getByDisplayValue("Delivery by admin")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("follows a same-second refresh that changed nothing the editor shows, and saves against it", async () => {
    apiMock.updateIssueWorkflow.mockResolvedValue(workflow());
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });

    await publish(workflow({ ...sameSecond, version: "2026-01-01T00:00:05.700000Z" }));
    expect(screen.queryByRole("alert")).toBeNull();
    await save();

    expect(apiMock.updateIssueWorkflow.mock.calls[0]![1]).toMatchObject({
      expected_version: "2026-01-01T00:00:05.700000Z",
    });
  });

  it("names the new version on a second save after the first succeeds", async () => {
    apiMock.updateIssueWorkflow.mockImplementation(async (_id: string, body: { name?: string }) =>
      workflow({ ...sameSecond, name: body.name ?? "Delivery", version: "2026-01-01T00:00:05.900000Z" }),
    );
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    await save();
    fireEvent.change(screen.getByDisplayValue("Delivery v2"), { target: { value: "Delivery v3" } });
    await save();

    expect(apiMock.updateIssueWorkflow).toHaveBeenCalledTimes(2);
    expect(apiMock.updateIssueWorkflow.mock.calls[1]![1]).toMatchObject({
      expected_version: "2026-01-01T00:00:05.900000Z",
    });
  });
});

// An existing workflow's row is not there until its query loads. Until then the
// editor holds defaults, and saving them by workflow id would replace the saved
// columns and handoffs the editor never saw.
describe("workflow editor before the row is available", () => {
  const statuses = ["backlog", "todo", "in_progress", "blocked", "done", "cancelled"].map(statusEntry);
  const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
  let queryClient: QueryClient;

  function mount(workflowId: string | null) {
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses });
    queryClient.setQueryData(projectListOptions("ws-1").queryKey, { projects: [], total: 0 });
    renderWithI18n(
      <QueryClientProvider client={queryClient}>
        <WorkflowEditorPage workflowId={workflowId} seed={null} canEdit onBack={() => {}} onOpen={() => {}} />
      </QueryClientProvider>,
    );
  }

  beforeEach(() => {
    apiMock.updateIssueWorkflow.mockReset();
    apiMock.listIssueWorkflows.mockReset();
    apiMock.createIssueWorkflow.mockReset();
  });
  afterEach(cleanup);

  it("offers no editable fields and no Save while the workflow is loading", async () => {
    apiMock.listIssueWorkflows.mockReturnValue(new Promise(() => {}));
    mount("wf-1");
    await act(async () => settle());

    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(apiMock.updateIssueWorkflow).not.toHaveBeenCalled();
  });

  it("becomes editable and saves against the loaded row once it arrives", async () => {
    apiMock.listIssueWorkflows.mockResolvedValue({ workflows: [workflow()], total: 1 });
    apiMock.updateIssueWorkflow.mockResolvedValue(workflow());
    mount("wf-1");
    await waitFor(() => expect(screen.getByDisplayValue("Delivery")).toBeTruthy());

    fireEvent.change(screen.getByDisplayValue("Delivery"), { target: { value: "Delivery v2" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
      await settle();
    });

    expect(apiMock.updateIssueWorkflow).toHaveBeenCalledTimes(1);
    expect(apiMock.updateIssueWorkflow.mock.calls[0]![1]).toMatchObject({
      name: "Delivery v2",
      expected_version: "v-2026-01-01",
    });
    expect((apiMock.updateIssueWorkflow.mock.calls[0]![1] as { steps: unknown[] }).steps.length).toBeGreaterThan(0);
  });

  it("shows an error with a retry, and no Save, when the workflow list fails to load", async () => {
    apiMock.listIssueWorkflows.mockRejectedValue(new Error("network down"));
    mount("wf-1");

    await waitFor(() => expect(screen.getByRole("alert")).toBeTruthy());
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(apiMock.updateIssueWorkflow).not.toHaveBeenCalled();

    apiMock.listIssueWorkflows.mockResolvedValue({ workflows: [workflow()], total: 1 });
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.getByDisplayValue("Delivery")).toBeTruthy());
  });

  it("shows not-found, with no Save, when the workflow does not exist", async () => {
    apiMock.listIssueWorkflows.mockResolvedValue({ workflows: [], total: 0 });
    mount("wf-missing");

    await waitFor(() => expect(screen.getByText("This workflow no longer exists.")).toBeTruthy());
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
    expect(apiMock.updateIssueWorkflow).not.toHaveBeenCalled();
  });

  it("still creates a new workflow without waiting for any row", async () => {
    apiMock.listIssueWorkflows.mockReturnValue(new Promise(() => {}));
    apiMock.createIssueWorkflow.mockResolvedValue(workflow({ id: "wf-new" }));
    mount(null);

    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Brand new" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Create" }));
      await settle();
    });

    expect(apiMock.createIssueWorkflow).toHaveBeenCalledTimes(1);
    expect(apiMock.updateIssueWorkflow).not.toHaveBeenCalled();
  });
});

// A save is in flight. Refreshes and further edits arrive before its response,
// and the response can be a conflict, a mapping request or a plain failure.
// Nothing here may replace unaccepted edits without the editor choosing.
describe("workflow editor draft preservation during a save", () => {
  let queryClient: QueryClient;
  const statuses = ["backlog", "todo", "in_progress", "blocked", "done", "cancelled"].map(statusEntry);
  const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
  const publish = (next: IssueWorkflow) =>
    act(async () => {
      queryClient.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [next], total: 1 });
      await settle();
    });
  const nameField = () => screen.getByDisplayValue(/Delivery/) as HTMLInputElement;
  const saveButton = () => screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
  // A refresh that changes nothing the editor shows: another version, same content.
  const unrelated = (version: string) =>
    workflow({ version, updated_at: "2026-01-01T00:00:09Z", project_ids: ["project-9"] });

  function deferredSave() {
    let resolve!: (value: IssueWorkflow) => void;
    let reject!: (reason: unknown) => void;
    apiMock.updateIssueWorkflow.mockReturnValue(
      new Promise<IssueWorkflow>((res, rej) => {
        resolve = res;
        reject = rej;
      }),
    );
    return { resolve, reject };
  }
  const startSave = () =>
    act(async () => {
      fireEvent.click(saveButton());
      await settle();
    });

  beforeEach(() => {
    apiMock.updateIssueWorkflow.mockReset();
    apiMock.listIssueWorkflows.mockReset();
    apiMock.listIssueWorkflows.mockImplementation(async () =>
      queryClient.getQueryData(issueWorkflowKeys.list("ws-1")),
    );
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses });
    queryClient.setQueryData(projectListOptions("ws-1").queryKey, { projects: [], total: 0 });
    queryClient.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [workflow()], total: 1 });
    renderWithI18n(
      <QueryClientProvider client={queryClient}>
        <WorkflowEditorPage workflowId="wf-1" seed={null} canEdit onBack={() => {}} onOpen={() => {}} />
      </QueryClientProvider>,
    );
  });
  afterEach(cleanup);

  it("keeps the draft when an unrelated refresh lands during a save that is then refused as stale", async () => {
    const save = deferredSave();
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    await startSave();

    await publish(unrelated("v-2026-01-01-b"));
    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();

    await act(async () => {
      save.reject(new ApiError("conflict", 409, "Conflict", { code: "workflow_changed_elsewhere" }));
      await settle();
    });

    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(saveButton().disabled).toBe(true);
  });

  it("keeps the draft through a refresh after the server asks for a status mapping", async () => {
    const save = deferredSave();
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    await startSave();
    await act(async () => {
      save.reject(
        new ApiError("mapping", 409, "Conflict", {
          code: "workflow_status_mapping_required",
          plan: { required: [{ status_key: "in_progress", issue_count: 2, suggested_status_key: "todo" }], unchanged: [] },
        }),
      );
      await settle();
    });

    await publish(unrelated("v-2026-01-01-b"));

    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
  });

  it("keeps the draft through a refresh after a plain save failure", async () => {
    const save = deferredSave();
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    await startSave();
    await act(async () => {
      save.reject(new ApiError("boom", 500, "Server Error", {}));
      await settle();
    });

    await publish(unrelated("v-2026-01-01-b"));

    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
    expect(saveButton().disabled).toBe(false);
  });

  it("keeps edits made while the save is pending, and saves them against the response's version", async () => {
    const save = deferredSave();
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    await startSave();
    fireEvent.change(screen.getByDisplayValue("Delivery v2"), { target: { value: "Delivery v3" } });

    await act(async () => {
      save.resolve(workflow({ name: "Delivery v2", version: "v-2026-01-01-saved", updated_at: "2026-01-01T00:00:09Z" }));
      await settle();
    });

    expect(screen.getByDisplayValue("Delivery v3")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
    await waitFor(() => expect(saveButton().disabled).toBe(false));

    apiMock.updateIssueWorkflow.mockResolvedValue(workflow());
    await startSave();
    expect(apiMock.updateIssueWorkflow.mock.calls[1]![1]).toMatchObject({
      name: "Delivery v3",
      expected_version: "v-2026-01-01-saved",
    });
  });

  it("adopts the response as the baseline when nothing was edited during the save", async () => {
    const save = deferredSave();
    fireEvent.change(nameField(), { target: { value: "Delivery v2" } });
    await startSave();

    await act(async () => {
      save.resolve(workflow({ name: "Delivery v2", version: "v-2026-01-01-saved", updated_at: "2026-01-01T00:00:09Z" }));
      await settle();
    });

    expect(screen.getByDisplayValue("Delivery v2")).toBeTruthy();
    expect(saveButton().disabled).toBe(true);
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

// A save that needs a status mapping leaves the mapping dialog open on the
// whole-workflow draft. Another admin then changes the workflow, and the editor
// holds Save behind its conflict choice. The still-open dialog is another way
// to save, and a later unrelated refresh must not slide the accepted version
// forward to the other admin's token.
describe("workflow editor conflict with the mapping dialog open", () => {
  let queryClient: QueryClient;
  const statuses = ["backlog", "todo", "in_progress", "blocked", "done", "cancelled"].map(statusEntry);
  const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
  const publish = (next: IssueWorkflow) =>
    act(async () => {
      queryClient.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [next], total: 1 });
      await settle();
    });

  beforeEach(() => {
    apiMock.updateIssueWorkflow.mockReset();
    apiMock.listIssueWorkflows.mockReset();
    apiMock.listIssueWorkflows.mockImplementation(async () => queryClient.getQueryData(issueWorkflowKeys.list("ws-1")));
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses });
    queryClient.setQueryData(projectListOptions("ws-1").queryKey, { projects: [], total: 0 });
    queryClient.setQueryData(issueWorkflowKeys.list("ws-1"), { workflows: [workflow()], total: 1 });
    renderWithI18n(
      <QueryClientProvider client={queryClient}>
        <WorkflowEditorPage workflowId="wf-1" seed={null} canEdit onBack={() => {}} onOpen={() => {}} />
      </QueryClientProvider>,
    );
  });
  afterEach(cleanup);

  async function openMappingDialog() {
    apiMock.updateIssueWorkflow.mockRejectedValueOnce(
      new ApiError("mapping", 409, "Conflict", {
        code: "workflow_status_mapping_required",
        plan: {
          required: [{ status_key: "in_progress", issue_count: 2, suggested_status_key: "todo" }],
          unchanged: [],
          total_issues: 2,
          affected_issues: 2,
        },
      }),
    );
    fireEvent.change(screen.getByDisplayValue(/Delivery/), { target: { value: "Delivery v2" } });
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
      await settle();
    });
    await waitFor(() => expect(screen.getByRole("dialog")).toBeTruthy());
  }

  it("does not save the old draft from the dialog after another admin's change and a catalog refresh", async () => {
    await openMappingDialog();
    expect(apiMock.updateIssueWorkflow).toHaveBeenCalledTimes(1);

    await publish(workflow({ description: "Changed by the other admin", version: "v-2026-01-01-b", updated_at: "2026-01-01T00:00:09Z" }));
    await act(async () => {
      queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses: [...statuses, statusEntry("in_review")] });
      await settle();
    });

    apiMock.updateIssueWorkflow.mockResolvedValue(workflow());
    await act(async () => {
      fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: /Save/ }));
      await settle();
    });

    expect(apiMock.updateIssueWorkflow).toHaveBeenCalledTimes(1);
  });

  it("does not move the accepted version to the other admin's token on a refresh while the conflict is open", async () => {
    await openMappingDialog();
    await publish(workflow({ description: "Changed by the other admin", version: "v-2026-01-01-b", updated_at: "2026-01-01T00:00:09Z" }));
    await act(async () => {
      queryClient.setQueryData(issueStatusKeys.list("ws-1"), { statuses: [...statuses, statusEntry("in_review")] });
      await settle();
    });

    // Choosing to keep the edits is the only way the token moves.
    apiMock.updateIssueWorkflow.mockResolvedValue(workflow());
    fireEvent.click(screen.getByText("Keep my edits"));
    await act(async () => {
      fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: /Save/ }));
      await settle();
    });

    expect(apiMock.updateIssueWorkflow).toHaveBeenCalledTimes(2);
    expect(apiMock.updateIssueWorkflow.mock.calls[1]![1]).toMatchObject({ expected_version: "v-2026-01-01-b" });
  });
});
