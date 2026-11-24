"use client";

import type { ReactNode } from "react";
import { SidebarProvider } from "@multica/ui/components/ui/sidebar";
import { AppearanceSync } from "../common/appearance-sync";
import { ModalRegistry } from "../modals/registry";
import { SourceBackfillModal } from "../onboarding/source-backfill-modal";
import { AppRail } from "./app-rail";
import { AppBottomBar } from "./bottom-bar";
import { AppShellProvider } from "./app-shell-context";
import { CompactNavSheet } from "./compact-nav-sheet";
import { DashboardGuard } from "./dashboard-guard";
import { NavigationProgress } from "./navigation-progress";
import { ShellRouteDataPrefetch } from "./route-data-prefetch";
import { SpacePanel } from "./space-panel";
import { WorkspacePresencePrefetch } from "./workspace-presence-prefetch";
import { GlobalShortcuts } from "./global-shortcuts";
import { MachinePauseBanner } from "./machine-pause-banner";
import { WorkspacePauseBanner } from "./workspace-pause-banner";
import { OpenDecisionsChip } from "./open-decisions-chip";

interface DashboardLayoutProps {
  children: ReactNode;
  /** Rendered inside the content column (e.g. ChatWindow, ChatFab — absolute-positioned overlays) */
  extra?: ReactNode;
  /** Loading indicator */
  loadingIndicator?: ReactNode;
}

/**
 * The content card: the space panel and the page share one surface on the
 * app shell, split by a single hairline. From `md` up the bottom bar sits
 * under it, in place of a bottom margin; below `md` the card is the whole
 * window. Desktop builds the same card in its own layout, under the tab bar.
 */
export function AppShellCard({ children }: { children: ReactNode }) {
  return (
    <main data-mc="page" className="relative flex min-h-0 min-w-0 flex-1 flex-row overflow-hidden bg-page-canvas md:mt-2 md:mr-2 md:rounded-page md:shadow-[var(--page-shadow,var(--surface-shadow))] md:ring-1 md:ring-page-border">
      <SpacePanel />
      <div className="relative flex min-w-0 flex-1 flex-col overflow-hidden">{children}</div>
    </main>
  );
}

export function DashboardLayout({
  children,
  extra,
  loadingIndicator,
}: DashboardLayoutProps) {
  return (
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
    <DashboardGuard
      loadingFallback={
        <div className="flex h-svh items-center justify-center">
          {loadingIndicator}
        </div>
      }
    >
      <SidebarProvider className="h-svh bg-app-shell [--sidebar-wrapper-fill:var(--app-shell)]">
        <AppShellProvider>
          <ShellRouteDataPrefetch>
            <GlobalShortcuts />
            <WorkspacePresencePrefetch />
            <AppRail className="hidden md:flex" />
            <CompactNavSheet />
            <AppShellCard>
              <NavigationProgress />
              <MachinePauseBanner />
              <WorkspacePauseBanner />
              <OpenDecisionsChip />
              {children}
              <ModalRegistry />
              <SourceBackfillModal />
              {extra}
            </AppShellCard>
          </ShellRouteDataPrefetch>
        </AppShellProvider>
      </SidebarProvider>
    </DashboardGuard>
||||||| 2ae2f08005
    <DashboardGuard
      loadingFallback={
        <div className="flex h-svh items-center justify-center">
          {loadingIndicator}
        </div>
      }
    >
      <SidebarProvider className="h-svh bg-app-shell [--sidebar-wrapper-fill:var(--app-shell)]">
        <AppShellProvider>
          <ShellRouteDataPrefetch>
            <GlobalShortcuts />
            <WorkspacePresencePrefetch />
            <AppRail className="hidden md:flex" />
            <CompactNavSheet />
            <AppShellCard>
              <NavigationProgress />
              {children}
              <ModalRegistry />
              <SourceBackfillModal />
              {extra}
            </AppShellCard>
          </ShellRouteDataPrefetch>
        </AppShellProvider>
      </SidebarProvider>
    </DashboardGuard>
=======
    <>
      <AppearanceSync />
      <DashboardGuard
        loadingFallback={
          <div className="flex h-svh items-center justify-center">
            {loadingIndicator}
          </div>
        }
      >
        {/* Fills the screen with its content inside the safe area, so the
            strips match what borders them: the card below `md`, where it is
            the whole window, the app shell from `md` up. `data-app-shell`
            lifts the body's side insets (apps/web/app/globals.css). */}
        <SidebarProvider
          data-app-shell=""
          className="h-svh bg-page-canvas pt-[env(safe-area-inset-top)] pr-[env(safe-area-inset-right)] pb-[env(safe-area-inset-bottom)] pl-[env(safe-area-inset-left)] [--sidebar-wrapper-fill:var(--page-canvas)] md:bg-app-shell md:[--sidebar-wrapper-fill:var(--app-shell)]"
        >
          <AppShellProvider>
            <ShellRouteDataPrefetch>
              <GlobalShortcuts />
              <WorkspacePresencePrefetch />
              <AppRail className="hidden md:flex" />
              <CompactNavSheet />
              <div className="flex min-w-0 flex-1 flex-col">
                <AppShellCard>
                  <NavigationProgress />
                  {children}
                  <ModalRegistry />
                  <SourceBackfillModal />
                  {extra}
                </AppShellCard>
                <AppBottomBar />
              </div>
            </ShellRouteDataPrefetch>
          </AppShellProvider>
        </SidebarProvider>
      </DashboardGuard>
    </>
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
  );
}
