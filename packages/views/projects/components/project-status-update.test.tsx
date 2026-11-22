import React from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import type { Project } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { ProjectStatusBadge } from "./project-badge";
import { useProjectFieldUpdate } from "./use-project-field-update";

const mocks = vi.hoisted(() => ({
  toastError: vi.fn(),
  updateProject: vi.fn(),
}));

vi.mock("sonner", () => ({ toast: { error: mocks.toastError, success: vi.fn() } }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/ui/components/ui/dropdown-menu", () => ({
  DropdownMenu: ({ children }: { children: React.ReactNode }) => <>{children}</>,
  DropdownMenuTrigger: ({ render }: { render: React.ReactNode }) => <>{render}</>,
  DropdownMenuContent: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  DropdownMenuItem: ({ children, onClick }: { children: React.ReactNode; onClick?: () => void }) => (
    <button type="button" onClick={onClick}>{children}</button>
  ),
}));

const PROJECT = { id: "p1", title: "P", status: "planned", priority: "none" } as unknown as Project;
const DETAIL_KEY = ["projects", "ws-1", "detail", "p1"];

function Harness() {
  const { data: project } = useQuery({ queryKey: DETAIL_KEY, queryFn: () => PROJECT, staleTime: Infinity });
  const { handleUpdate, statusPending } = useProjectFieldUpdate("p1");
  if (!project) return null;
  return (
    <div>
      <span data-testid="status">{project.status}</span>
      <ProjectStatusBadge project={project} handleUpdate={handleUpdate} statusPending={statusPending} />
    </div>
  );
}

function renderHarness() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  qc.setQueryData(DETAIL_KEY, PROJECT);
  renderWithI18n(
    <QueryClientProvider client={qc}>
      <Harness />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mocks.toastError.mockReset();
  mocks.updateProject.mockReset();
  setApiInstance({ updateProject: mocks.updateProject } as unknown as ApiClient);
});

describe("project status change", () => {
  it("shows the server error in a toast and keeps the old status when rejected", async () => {
    mocks.updateProject.mockRejectedValue(new Error("project has live runs"));
    const user = userEvent.setup();
    renderHarness();

    await user.click(screen.getByRole("button", { name: "Paused" }));

    await waitFor(() => expect(mocks.toastError).toHaveBeenCalledWith("project has live runs"));
    expect(screen.getByTestId("status")).toHaveTextContent("planned");
  });

  it("shows the control as pending until the server answers", async () => {
    let resolve!: (v: unknown) => void;
    mocks.updateProject.mockReturnValue(new Promise((r) => (resolve = r)));
    const user = userEvent.setup();
    renderHarness();
    const trigger = () => screen.getAllByRole("button", { name: "Planned" })[0]!;

    await user.click(screen.getByRole("button", { name: "Paused" }));

    await waitFor(() => expect(trigger()).toBeDisabled());
    expect(screen.getByTestId("status")).toHaveTextContent("planned");
    resolve({ ...PROJECT, status: "paused" });
    await waitFor(() => expect(trigger()).toBeEnabled());
  });
});
