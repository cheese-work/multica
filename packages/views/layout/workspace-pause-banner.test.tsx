// @vitest-environment jsdom

import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Workspace } from "@multica/core/types";
import enLayout from "../locales/en/layout.json";
import enCommon from "../locales/en/common.json";

const listMembers = vi.hoisted(() => vi.fn());
const resumeWorkspace = vi.hoisted(() => vi.fn());
const state = vi.hoisted(() => ({ workspace: null as unknown }));

vi.mock("@multica/core/paths", () => ({ useCurrentWorkspace: () => state.workspace }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (sel: (s: { user: { id: string } }) => unknown) => sel({ user: { id: "user-me" } }),
}));
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: { listMembers, resumeWorkspace },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }));

import { WorkspacePauseBanner } from "./workspace-pause-banner";

function workspace(paused: boolean): Workspace {
  return {
    id: "ws-1",
    slug: "acme",
    scheduling_paused_at: paused ? new Date(Date.now() - 5 * 60_000).toISOString() : null,
    scheduling_paused_by: paused ? "user-ada" : null,
  } as Workspace;
}

function members(myRole: string) {
  return [
    { user_id: "user-me", role: myRole, name: "Me" },
    { user_id: "user-ada", role: "owner", name: "Ada" },
  ];
}

function renderBanner(ws: Workspace, myRole: string) {
  state.workspace = ws;
  listMembers.mockResolvedValue(members(myRole));
  return render(
    <I18nProvider locale="en" resources={{ en: { layout: enLayout, common: enCommon } }}>
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <WorkspacePauseBanner />
      </QueryClientProvider>
    </I18nProvider>,
  );
}

describe("WorkspacePauseBanner", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resumeWorkspace.mockResolvedValue(workspace(false));
  });

  it("renders nothing while the workspace runs", () => {
    const { container } = renderBanner(workspace(false), "owner");
    expect(container.textContent).toBe("");
  });

  it("shows who paused the workspace and when", async () => {
    renderBanner(workspace(true), "member");
    expect(await screen.findByRole("status")).toBeTruthy();
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("Ada"));
    expect(screen.getByRole("status").textContent).toMatch(/paused/i);
  });

  it("hides Resume from a plain member", async () => {
    renderBanner(workspace(true), "member");
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("Ada"));
    expect(screen.queryByRole("button", { name: /resume/i })).toBeNull();
  });

  it("lets an admin resume through the API", async () => {
    renderBanner(workspace(true), "admin");
    fireEvent.click(await screen.findByRole("button", { name: /resume/i }));
    await waitFor(() => expect(resumeWorkspace).toHaveBeenCalledWith("ws-1"));
  });
});
