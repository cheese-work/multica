"use client";

import type { ReactNode } from "react";
import { useT } from "../../i18n";
import { useChatAnnotationScope } from "./chat-annotation-context";

/**
 * The answer text of an agent reply. Once the reply is persisted (`messageId`)
 * it is a named tab stop that readers can select from, and markers for saved
 * comments render in the slot right after it, so Tab reaches them before the
 * footer (reading order). While the reply streams the same wrapper renders
 * without the selection attributes, so the answer node keeps its identity at
 * the live to settled handoff.
 */
export function AnnotatableAnswer({ messageId, children, readOnly = false }: { messageId?: string; children: ReactNode; readOnly?: boolean }) {
  const { t } = useT("chat");
  const { enabled, agentName } = useChatAnnotationScope();
  if (!enabled) return <>{children}</>;
  return (
    <div data-annotation-overlay-host>
      <div
        data-comment-content={readOnly ? undefined : messageId}
        tabIndex={messageId ? 0 : undefined}
        role={messageId ? "group" : undefined}
        aria-label={messageId ? t(($) => $.annotations.source_label, { name: agentName ?? "" }) : undefined}
        className="rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-brand"
      >
        {children}
      </div>
      <div data-annotation-overlay-slot />
    </div>
  );
}
