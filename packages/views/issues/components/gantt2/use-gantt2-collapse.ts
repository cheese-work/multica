"use client";

import { useCallback, useMemo } from "react";
import { useViewStore, useViewStoreApi } from "@multica/core/issues/stores/view-store-context";

/** Gantt v2 collapse set from the persisted view store (`collapsedIssues.gantt`), shaped for `Gantt2Bars`. */
export function useGantt2Collapse() {
  const ids = useViewStore((s) => s.collapsedIssues.gantt);
  const api = useViewStoreApi();
  const collapsed = useMemo<ReadonlySet<string>>(() => new Set(ids), [ids]);
  const onToggleCollapse = useCallback((issueId: string) => api.getState().toggleIssueCollapsed("gantt", issueId), [api]);
  return { collapsed, onToggleCollapse };
}
