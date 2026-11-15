"use client";

import type { ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { chatSessionOptions } from "@multica/core/chat/queries";
import { useWorkspaceId } from "@multica/core/hooks";
import { useWorkspacePaths } from "@multica/core/paths";
import { AppLink } from "../../navigation";
import { ISSUE_CHIP_CLASS } from "../../issues/components/issue-chip";

/**
 * Navigable chip for a DM link: the DM title, opening that DM (and message)
 * in-app. While the title resolves, or when the server will not resolve it
 * (deleted, no access), `fallback` renders instead, so the link never vanishes.
 */
export function ChatSessionMentionCard({
  sessionId,
  messageId,
  fallback,
}: {
  sessionId: string;
  messageId?: string | null;
  fallback: ReactNode;
}) {
  const wsId = useWorkspaceId();
  const p = useWorkspacePaths();
  const { data } = useQuery(chatSessionOptions(wsId, sessionId));
  if (!data?.title) return <>{fallback}</>;

  const message = messageId ? `&message=${encodeURIComponent(messageId)}` : "";
  return (
    <AppLink
      href={`${p.chat()}?session=${encodeURIComponent(sessionId)}${message}`}
      newTabTitle={data.title}
      className="issue-mention align-middle"
    >
      <span className={`${ISSUE_CHIP_CLASS} cursor-pointer hover:bg-accent transition-colors`}>
        <span className="min-w-0 truncate text-foreground">{data.title}</span>
      </span>
    </AppLink>
  );
}
