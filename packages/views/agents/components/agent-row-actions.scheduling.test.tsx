// @vitest-environment jsdom

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enAgents from "../../locales/en/agents.json";

const TEST_RESOURCES = { en: { common: enCommon, agents: enAgents } };

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/api", () => ({ api: {} }));
vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ agentDetail: (id: string) => `/agents/${id}` }),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("../../navigation", () => ({
  AppLink: ({ children }: { children?: React.ReactNode }) => <a>{children}</a>,
  useIntentNavigate: () => vi.fn(),
}));
// Render the menu content inline so items are queryable without opening a popup.
vi.mock("../../layout/collection-list", () => ({
  CollectionRowMenu: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
}));
vi.mock("@multica/ui/components/ui/dropdown-menu", () => ({
  DropdownMenuItem: ({ children }: { children?: React.ReactNode }) => (
    <button role="menuitem">{children}</button>
  ),
  DropdownMenuSeparator: () => null,
}));

import { AgentRowActions } from "./agent-row-actions";

const agent = { id: "a-1", name: "Tofu", scheduling_paused_at: null } as never;

function renderRow(canSchedule: boolean | undefined, paused = false) {
  const a = { ...(agent as object), scheduling_paused_at: paused ? "2026-11-12T00:00:00Z" : null } as never;
  return render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={new QueryClient()}>
        <AgentRowActions
          agent={a}
          presence={undefined}
          canManage
          canSchedule={canSchedule}
          duplicateHref="/dup"
        />
      </QueryClientProvider>
    </I18nProvider>,
  );
}

describe("AgentRowActions pause / resume visibility", () => {
  it("hides Pause from a member who is not owner or admin", () => {
    renderRow(false);
    expect(screen.queryByRole("menuitem", { name: /pause agent/i })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: /resume agent/i })).toBeNull();
  });

  it("hides the control when canSchedule is not passed", () => {
    renderRow(undefined);
    expect(screen.queryByRole("menuitem", { name: /pause|resume/i })).toBeNull();
  });

  it("shows Pause to an owner or admin, Resume once paused", () => {
    const { unmount } = renderRow(true);
    expect(screen.getByRole("menuitem", { name: /pause agent/i })).toBeTruthy();
    unmount();
    renderRow(true, true);
    expect(screen.getByRole("menuitem", { name: /resume agent/i })).toBeTruthy();
  });
});
