// @vitest-environment jsdom

import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DashboardLayout } from "./dashboard-layout";

vi.mock("./dashboard-guard", () => ({
  DashboardGuard: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("../modals/registry", () => ({ ModalRegistry: () => null }));
vi.mock("../onboarding/source-backfill-modal", () => ({ SourceBackfillModal: () => null }));
vi.mock("./app-rail", () => ({ AppRail: () => null }));
vi.mock("./compact-nav-sheet", () => ({ CompactNavSheet: () => null }));
vi.mock("./navigation-progress", () => ({ NavigationProgress: () => null }));
vi.mock("./route-data-prefetch", () => ({
  ShellRouteDataPrefetch: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("./space-panel", () => ({ SpacePanel: () => null }));
vi.mock("./workspace-presence-prefetch", () => ({ WorkspacePresencePrefetch: () => null }));
vi.mock("./global-shortcuts", () => ({ GlobalShortcuts: () => null }));
vi.mock("./open-decisions-chip", () => ({ OpenDecisionsChip: () => null }));
vi.mock("./app-shell-context", () => ({
  AppShellProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("./machine-pause-banner", () => ({
  MachinePauseBanner: () => <div data-testid="machine-pause-banner" />,
}));
vi.mock("./workspace-pause-banner", () => ({
  WorkspacePauseBanner: () => null,
}));

describe("DashboardLayout", () => {
  it("mounts the machine pause banner above the page in the content card", () => {
    render(
      <DashboardLayout>
        <div data-testid="page" />
      </DashboardLayout>,
    );
    const banner = screen.getByTestId("machine-pause-banner");
    const page = screen.getByTestId("page");
    expect(banner.parentElement).toBe(page.parentElement);
    expect(banner.compareDocumentPosition(page) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
