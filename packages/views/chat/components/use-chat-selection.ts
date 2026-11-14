"use client";

import { useCallback, useMemo, useState } from "react";
import type { ChatSession } from "@multica/core/types";
import type { ChatBulkAction } from "@multica/core/chat/mutations";

export type BulkAction = ChatBulkAction;
export type GroupState = "all" | "some" | "none";

/**
 * Multi-select state for the DM list. `null` = not selecting. Ids that left
 * the visible list (chip change, realtime update) drop out of the selection.
 */
export function useChatSelection(visibleIds: readonly string[]) {
  const [picked, setPicked] = useState<ReadonlySet<string> | null>(null);
  const selected = useMemo(() => {
    const visible = new Set(visibleIds);
    return new Set([...(picked ?? [])].filter((id) => visible.has(id)));
  }, [picked, visibleIds]);

  const toggle = useCallback(
    (id: string) =>
      setPicked((prev) => {
        const next = new Set(prev);
        if (!next.delete(id)) next.add(id);
        return next;
      }),
    [],
  );
  const groupState = useCallback(
    (ids: readonly string[]): GroupState => {
      const n = ids.filter((id) => selected.has(id)).length;
      return n === 0 ? "none" : n === ids.length ? "all" : "some";
    },
    [selected],
  );
  const toggleGroup = useCallback(
    (ids: readonly string[]) =>
      setPicked((prev) => {
        const next = new Set(prev);
        const all = ids.every((id) => next.has(id));
        for (const id of ids) {
          if (all) next.delete(id);
          else next.add(id);
        }
        return next;
      }),
    [],
  );

  return {
    active: picked !== null,
    selected,
    enter: useCallback((id: string) => setPicked(new Set([id])), []),
    exit: useCallback(() => setPicked(null), []),
    toggle,
    toggleGroup,
    groupState,
    selectAll: useCallback(() => setPicked(new Set(visibleIds)), [visibleIds]),
  };
}

interface Step {
  action: BulkAction;
  ids: string[];
}
interface Split {
  primary: Step;
  opposite: Step;
}

/**
 * Splits the selection by one two-way attribute. The primary button takes the
 * larger side (a tie keeps the forward action); the arrow offers the other.
 */
function split(sessions: ChatSession[], forward: BulkAction, back: BulkAction, isBack: (s: ChatSession) => boolean): Split {
  const backIds = sessions.filter(isBack).map((s) => s.id);
  const forwardIds = sessions.filter((s) => !isBack(s)).map((s) => s.id);
  return forwardIds.length >= backIds.length
    ? { primary: { action: forward, ids: forwardIds }, opposite: { action: back, ids: backIds } }
    : { primary: { action: back, ids: backIds }, opposite: { action: forward, ids: forwardIds } };
}

const isUnseen = (s: ChatSession) => s.has_unread || (s.unread_count ?? 0) > 0;

/** Bulk-bar plan. Archived DMs are never pinned and never change seen state. */
export function planBulk(sessions: ChatSession[]) {
  const active = sessions.filter((s) => s.status !== "archived");
  return {
    // "forward" for seen = mark seen, i.e. the DMs that are unseen now.
    seen: split(active, "seen", "unseen", (s) => !isUnseen(s)),
    pin: split(active, "pin", "unpin", (s) => !!s.pinned),
    archive: split(sessions, "archive", "unarchive", (s) => s.status === "archived"),
  };
}
