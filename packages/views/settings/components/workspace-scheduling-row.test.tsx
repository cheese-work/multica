// @vitest-environment jsdom

import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Workspace } from "@multica/core/types";
import enSettings from "../../locales/en/settings.json";
import enCommon from "../../locales/en/common.json";

const pauseWorkspace = vi.hoisted(() => vi.fn());
const resumeWorkspace = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { pauseWorkspace, resumeWorkspace },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

import { WorkspaceSchedulingRow } from "./workspace-scheduling-row";

function renderRow(paused: boolean, canManage: boolean) {
  const ws = { id: "ws-1", scheduling_paused_at: paused ? "2026-11-13T00:00:00Z" : null } as Workspace;
  return render(
    <I18nProvider locale="en" resources={{ en: { settings: enSettings, common: enCommon } }}>
      <QueryClientProvider client={new QueryClient()}>
        <WorkspaceSchedulingRow workspace={ws} canManage={canManage} />
      </QueryClientProvider>
    </I18nProvider>,
  );
}

describe("WorkspaceSchedulingRow", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    pauseWorkspace.mockResolvedValue({ id: "ws-1" });
    resumeWorkspace.mockResolvedValue({ id: "ws-1" });
  });

  it("renders nothing for a member who is not owner or admin", () => {
    const { container } = renderRow(false, false);
    expect(container.textContent).toBe("");
  });

  it("pauses the workspace after the owner confirms", async () => {
    renderRow(false, true);
    fireEvent.click(screen.getByRole("button", { name: /pause workspace/i }));
    expect(pauseWorkspace).not.toHaveBeenCalled();
    fireEvent.click(await screen.findByRole("button", { name: /^pause$/i }));
    await waitFor(() => expect(pauseWorkspace).toHaveBeenCalledWith("ws-1"));
  });

  it("offers Resume while paused", async () => {
    renderRow(true, true);
    fireEvent.click(screen.getByRole("button", { name: /resume workspace/i }));
    await waitFor(() => expect(resumeWorkspace).toHaveBeenCalledWith("ws-1"));
  });
});
