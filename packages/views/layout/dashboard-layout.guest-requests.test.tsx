// @vitest-environment jsdom

import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import enRuntimes from "../locales/en/runtimes.json";
import enCommon from "../locales/en/common.json";
import enChat from "../locales/en/chat.json";
import { NavigationProvider } from "../navigation";

const listRuntimes = vi.hoisted(() => vi.fn());
const listAffected = vi.hoisted(() => vi.fn());
const listOpen = vi.hoisted(() => vi.fn());
const listMembers = vi.hoisted(() => vi.fn());

vi.mock("./dashboard-guard", () => ({
  DashboardGuard: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("../modals/registry", () => ({ ModalRegistry: () => null }));
vi.mock("../onboarding/source-backfill-modal", () => ({ SourceBackfillModal: () => null }));
vi.mock("./app-rail", () => ({ AppRail: () => null }));
vi.mock("./bottom-bar", () => ({ AppBottomBar: () => null }));
vi.mock("./compact-nav-sheet", () => ({ CompactNavSheet: () => null }));
vi.mock("./navigation-progress", () => ({ NavigationProgress: () => null }));
vi.mock("./route-data-prefetch", () => ({
  ShellRouteDataPrefetch: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("../billing/trial-notices", () => ({ WorkspaceTrialNotices: () => null }));
vi.mock("../common/appearance-sync", () => ({ AppearanceSync: () => null }));
vi.mock("./space-panel", () => ({ SpacePanel: () => null }));
vi.mock("./workspace-presence-prefetch", () => ({ WorkspacePresencePrefetch: () => null }));
vi.mock("./global-shortcuts", () => ({ GlobalShortcuts: () => null }));
vi.mock("./workspace-pause-banner", () => ({ WorkspacePauseBanner: () => null }));
vi.mock("./app-shell-context", () => ({
  AppShellProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useCurrentWorkspace: () => ({ id: "ws-1" }),
  useWorkspaceSlug: () => "test-ws",
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: (sel: (s: { user: { id: string } }) => unknown) => sel({ user: { id: "me" } }),
}));
vi.mock("@multica/core/realtime", () => ({ useWSEvent: () => {} }));
vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: {
    listRuntimes,
    listAffectedMachines: listAffected,
    listOpenDecisionRequests: listOpen,
    listMembers,
  },
}));

import { DashboardLayout } from "./dashboard-layout";

function renderShell() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <I18nProvider locale="en" resources={{ en: { runtimes: enRuntimes, common: enCommon, chat: enChat } }}>
      <QueryClientProvider client={client}>
        <NavigationProvider value={{ push: vi.fn(), replace: vi.fn(), back: vi.fn(), pathname: "/test-ws", searchParams: new URLSearchParams(), hash: "", getShareableUrl: (p) => p }}>
          <DashboardLayout>
            <div />
          </DashboardLayout>
        </NavigationProvider>
      </QueryClientProvider>
    </I18nProvider>,
  );
}

describe("DashboardLayout member-only requests", () => {
  beforeEach(() => {
    for (const fn of [listRuntimes, listAffected, listOpen, listMembers]) fn.mockReset();
    listRuntimes.mockResolvedValue([]);
    listAffected.mockResolvedValue([]);
    listOpen.mockResolvedValue([]);
  });

  it("sends neither request for a guest, not even while the member list loads", async () => {
    listMembers.mockResolvedValue([{ user_id: "me", name: "Me", role: "guest" }]);
    renderShell();
    await waitFor(() => expect(listMembers).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 50));
    expect(listAffected).not.toHaveBeenCalled();
    expect(listOpen).not.toHaveBeenCalled();
  });

  it("sends both requests for a member once the role is known", async () => {
    listMembers.mockResolvedValue([{ user_id: "me", name: "Me", role: "member" }]);
    renderShell();
    await waitFor(() => expect(listAffected).toHaveBeenCalled());
    await waitFor(() => expect(listOpen).toHaveBeenCalled());
  });
});
