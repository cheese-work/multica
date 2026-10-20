import { QueryClient } from "@tanstack/react-query";
import type { Issue } from "@multica/core/types";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "@/data/api";
import { issueKeys } from "@/data/queries/issue-keys";
import { useUpdateIssue } from "./issues";

// A project workflow can hand an issue off when it enters a status, and the
// server applies that handoff to any write that does not say what the person
// saw. So the mobile mutation must not send a status without the binding that
// the preview and confirmation produced, whichever screen calls it.

const state = vi.hoisted(() => ({
  qc: undefined as unknown as QueryClient,
}));

vi.mock("@tanstack/react-query", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-query")>();
  return {
    ...actual,
    useQueryClient: () => state.qc,
    useMutation: (
      options: ConstructorParameters<typeof actual.MutationObserver>[1],
    ) => {
      const observer = new actual.MutationObserver(state.qc, options);
      return { mutateAsync: (variables: unknown) => observer.mutate(variables) };
    },
  };
});

vi.mock("@/data/api", () => ({
  api: { updateIssue: vi.fn(async () => ({ id: "issue-1", revision: 2 })) },
}));

vi.mock("@/data/auth-store", () => ({ useAuthStore: vi.fn() }));

vi.mock("@/data/workspace-store", () => ({
  useWorkspaceStore: (
    selector: (s: { currentWorkspaceId: string }) => unknown,
  ) => selector({ currentWorkspaceId: "workspace-1" }),
}));

const wsId = "workspace-1";
const issueId = "issue-1";
const detailKey = issueKeys.detail(wsId, issueId);

const NO_HANDOFF = {
  previous_assignee_type: null,
  previous_assignee_id: null,
  handler_type: null,
  handler_id: null,
};

beforeEach(() => {
  state.qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  state.qc.setQueryData(detailKey, { id: issueId, status: "todo", revision: 1 } as Issue);
  vi.mocked(api.updateIssue).mockClear();
});

describe("useUpdateIssue status writes", () => {
  it("refuses a status change that carries no handoff binding", async () => {
    const update = useUpdateIssue(issueId);

    await expect(update.mutateAsync({ status: "in_review" })).rejects.toThrow();

    expect(api.updateIssue).not.toHaveBeenCalled();
    expect(state.qc.getQueryData<Issue>(detailKey)?.status).toBe("todo");
  });

  it("sends a status change with the binding the confirmation produced", async () => {
    const update = useUpdateIssue(issueId);
    const handoff_preview = { ...NO_HANDOFF, previous_assignee_type: "agent", previous_assignee_id: "forge" };

    await update.mutateAsync({ status: "in_review", handoff_preview });

    expect(api.updateIssue).toHaveBeenCalledWith(issueId, { status: "in_review", handoff_preview });
  });

  it("sends a status change with a stop-runs choice and a handler binding", async () => {
    const update = useUpdateIssue(issueId);
    const handoff_preview = {
      previous_assignee_type: "agent",
      previous_assignee_id: "forge",
      handler_type: "agent",
      handler_id: "sentinel",
    };

    await update.mutateAsync({ status: "in_review", stop_previous_assignee_runs: true, handoff_preview });

    expect(api.updateIssue).toHaveBeenCalledWith(issueId, {
      status: "in_review",
      stop_previous_assignee_runs: true,
      handoff_preview,
    });
  });

  it("writes other fields without a binding", async () => {
    const update = useUpdateIssue(issueId);

    await update.mutateAsync({ priority: "high" });

    expect(api.updateIssue).toHaveBeenCalledWith(issueId, { priority: "high" });
  });
});
