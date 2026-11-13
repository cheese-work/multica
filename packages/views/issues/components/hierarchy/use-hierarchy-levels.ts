import { useCallback, useEffect, useRef, useState } from "react";
import { useWorkspaceId } from "@multica/core/hooks";
import { fetchRelationsBatched, type RelationsResult } from "@multica/core/issues/graph-queries";
import type { TimelineNode } from "@multica/core/issues/graph-core";
import { FOCUS_CAP } from "./hierarchy-focus-layout";

/** `loading` while pages arrive (nodes so far are shown); `failed` keeps what loaded and offers a retry. */
export interface HierarchyLevel {
  nodes: TimelineNode[];
  status: "loading" | "ready" | "failed";
}

/** The 60 cards a level shows, `hidden` for the "+N" tile; `pending` marks N as a lower bound while paging. */
export function capLevel(level: HierarchyLevel | undefined) {
  const nodes = level?.nodes ?? [];
  return { shown: nodes.slice(0, FOCUS_CAP), hidden: Math.max(0, nodes.length - FOCUS_CAP), pending: level?.status === "loading" };
}

/** Children of `parentIds` in `res`, in response order. */
function childrenOf(res: RelationsResult, parentIds: readonly string[]): TimelineNode[] {
  const parents = new Set(parentIds);
  const kids = new Set(res.edges.filter((e) => e.type === "parent" && parents.has(e.source)).map((e) => e.target));
  return res.nodes
    .filter((n) => kids.has(n.id))
    .map((n) => ({ id: n.id, identifier: n.identifier, title: n.title, status: n.status, parentId: n.parent_issue_id, context: n.context }));
}

/**
 * Lazy hierarchy levels: `levels[d + 1]` holds the children of level `d` (level 0 is the root) and loads on `expand(d)`.
 * Each level has one request in flight; a newer `expand` or a root change aborts it, and a late response is dropped.
 */
export function useHierarchyLevels(rootId: string) {
  const wsId = useWorkspaceId();
  const [levels, setLevels] = useState<Record<number, HierarchyLevel>>({});
  const levelsRef = useRef(levels);
  levelsRef.current = levels;
  const inflight = useRef(new Map<number, AbortController>());

  const reset = useCallback(() => {
    inflight.current.forEach((c) => c.abort());
    inflight.current.clear();
    setLevels({});
  }, []);
  useEffect(() => {
    reset();
    return reset;
  }, [rootId, reset]);

  const expand = useCallback(
    (depth: number) => {
      const parentIds = depth === 0 ? [rootId] : (levelsRef.current[depth]?.nodes ?? []).map((n) => n.id);
      if (parentIds.length === 0) return;
      const target = depth + 1;
      inflight.current.get(target)?.abort();
      const ctl = new AbortController();
      inflight.current.set(target, ctl);
      const current = () => inflight.current.get(target) === ctl && !ctl.signal.aborted;
      const put = (nodes: TimelineNode[], status: HierarchyLevel["status"]) => current() && setLevels((l) => ({ ...l, [target]: { nodes, status } }));

      setLevels((l) => ({ ...l, [target]: { nodes: [], status: "loading" } }));
      fetchRelationsBatched(wsId, parentIds, { types: ["parent"] }, ctl.signal, (partial) => put(childrenOf(partial, parentIds), "loading"))
        .then((res) => put(childrenOf(res, parentIds), res.incomplete ? "failed" : "ready"))
        .catch((e) => {
          if (current()) put(levelsRef.current[target]?.nodes ?? [], "failed");
          else if (!(e instanceof DOMException && e.name === "AbortError")) throw e;
        });
    },
    [wsId, rootId],
  );

  return { levels, expand };
}
