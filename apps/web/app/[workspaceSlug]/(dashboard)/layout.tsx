"use client";

import { Suspense } from "react";
import { DashboardLayout } from "@multica/views/layout";
import { MulticaIcon } from "@multica/ui/components/common/multica-icon";
import { SearchTrigger } from "@multica/views/search";
import { FloatingChat } from "@multica/views/chat/floating-chat";
import { PushSync } from "@multica/views/settings/push-sync";
import { WebNotificationBridge } from "@/components/web-notification-bridge";
import { WorkspaceDocumentTitle } from "@/platform/workspace-document-title";
import { WorkspaceSearchCommand } from "@/components/workspace-search-command";

export default function Layout({ children }: { children: React.ReactNode }) {
  return (
    <>
      {/* useSearchParams requires a Suspense boundary in the app router. Sits
          outside DashboardLayout so the tab is named while the guard is still
          resolving the workspace. */}
      <Suspense fallback={null}>
        <WorkspaceDocumentTitle />
      </Suspense>
      <DashboardLayout
        loadingIndicator={<MulticaIcon className="size-6" />}
        searchSlot={<SearchTrigger />}
        extra={
          <>
            <Suspense fallback={null}>
              <WorkspaceSearchCommand />
            </Suspense>
            <WebNotificationBridge />
            <PushSync />
            <FloatingChat />
          </>
        }
      >
        {children}
      </DashboardLayout>
    </>
  );
}
