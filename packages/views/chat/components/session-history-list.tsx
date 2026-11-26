"use client";

import { useEffect, useRef, type ReactNode } from "react";
import { Virtuoso, type Components, type VirtuosoHandle } from "react-virtuoso";
import type { ChatSession } from "@multica/core/types";

type ListContext = { footer?: ReactNode };

// Keep component identities stable, including when there is no archive entry.
// Passing an explicit undefined components prop breaks Virtuoso 4.18 builds.
const COMPONENTS: Components<ChatSession, ListContext> = {
  Footer: ({ context }) => <>{context?.footer}</>,
};

export function SessionHistoryList({
  sessions,
  renderRow,
  footer,
  maxHeight,
  estimatedRowHeight = 56,
  revealSessionId,
}: {
  sessions: ChatSession[];
  renderRow: (session: ChatSession) => ReactNode;
  footer?: ReactNode;
  maxHeight?: number;
  estimatedRowHeight?: number;
  /** Row to bring into view; a row outside the rendered window is not mounted until then. */
  revealSessionId?: string | null;
}) {
  const virtuosoRef = useRef<VirtuosoHandle>(null);
  const sessionsRef = useRef(sessions);
  sessionsRef.current = sessions;
  useEffect(() => {
    if (!revealSessionId) return;
    const index = sessionsRef.current.findIndex((session) => session.id === revealSessionId);
    if (index >= 0) virtuosoRef.current?.scrollToIndex({ index, align: "center" });
  }, [revealSessionId]);
  return (
    <Virtuoso
      ref={virtuosoRef}
      style={{
        height: maxHeight == null
          ? "100%"
          : Math.min(maxHeight, sessions.length * estimatedRowHeight),
      }}
      data={sessions}
      defaultItemHeight={estimatedRowHeight}
      increaseViewportBy={200}
      computeItemKey={(_, session) => session.id}
      itemContent={(_, session) => renderRow(session)}
      components={COMPONENTS}
      context={{ footer }}
    />
  );
}
