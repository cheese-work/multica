import { skipToken, useQuery, type QueryClient } from "@tanstack/react-query";
import { useMemo, useRef } from "react";
import { issueKeys, issueTimelineOptions } from "./queries";
import type { TimelineEntry } from "../types";
import {
  removeEntity,
  resolveEntities,
  upsertEntities,
  type EntityTable,
} from "../normalized/entity-table";

/**
 * The by-id store behind an issue timeline (CHE-3236 B1b), the issue-comment
 * twin of the chat message store.
 *
 * `issueKeys.timelineEntities(issueId)` holds one row per timeline entry
 * (comments and activities share the id space). `issueKeys.timeline(issueId)`
 * holds only the ordered ids. Readers derive rows with `useTimelineRows` or
 * `readTimeline`; nothing keeps a second copy of the rows.
 *
 * Every writer goes through `updateTimeline`, which upserts changed rows by id
 * and drops ids that left the list, so a row exists once however it arrived
 * (HTTP page, realtime event, optimistic write). Both entries are per issue so
 * they are garbage collected and removed together with the issue.
 */

type Entities = EntityTable<TimelineEntry>;

const replace = (_existing: TimelineEntry, incoming: TimelineEntry) => incoming;

function sameIds(a: readonly string[] | undefined, b: readonly string[]): boolean {
  return !!a && a.length === b.length && a.every((id, i) => id === b[i]);
}

function sameRows(a: readonly TimelineEntry[] | undefined, b: readonly TimelineEntry[] | undefined): boolean {
  return !!a && !!b && a.length === b.length && a.every((row, i) => row === b[i]);
}

function write(
  qc: QueryClient,
  issueId: string,
  prevIds: string[] | undefined,
  entities: Entities | undefined,
  rows: readonly TimelineEntry[],
): void {
  let next = upsertEntities(
    entities,
    rows.filter((row) => entities?.byId[row.id] !== row),
    replace,
  );
  const kept = new Set(rows.map((row) => row.id));
  for (const id of prevIds ?? []) {
    if (!kept.has(id)) next = removeEntity(next, id) ?? next;
  }
  const nextIds = rows.map((row) => row.id);
  // Entities first: a reader that sees the new ids must find their rows.
  if (next !== entities) qc.setQueryData(issueKeys.timelineEntities(issueId), next);
  if (!sameIds(prevIds, nextIds)) qc.setQueryData(issueKeys.timeline(issueId), nextIds);
}

/**
 * HTTP route: a fetched timeline is an authoritative snapshot, so its rows
 * replace the stored ones and rows the server no longer returns are pruned.
 * A response whose request was aborted writes nothing, so it cannot overwrite
 * a newer write.
 */
export function ingestTimeline(
  qc: QueryClient,
  issueId: string,
  entries: TimelineEntry[],
  signal?: AbortSignal,
): string[] {
  if (!signal?.aborted) {
    write(
      qc,
      issueId,
      qc.getQueryData<string[]>(issueKeys.timeline(issueId)),
      qc.getQueryData<Entities>(issueKeys.timelineEntities(issueId)),
      entries,
    );
  }
  return entries.map((entry) => entry.id);
}

/** Rows of one issue timeline, or undefined while it has never loaded. */
export function readTimeline(qc: QueryClient, issueId: string): TimelineEntry[] | undefined {
  const ids = qc.getQueryData<string[]>(issueKeys.timeline(issueId));
  if (!ids) return undefined;
  return resolveEntities(qc.getQueryData<Entities>(issueKeys.timelineEntities(issueId)), ids);
}

/**
 * The one write door. `change` receives the current rows (undefined before the
 * first load) and returns the rows to keep, in order. Returning its input or
 * undefined writes nothing.
 */
export function updateTimeline(
  qc: QueryClient,
  issueId: string,
  change: (rows: TimelineEntry[] | undefined) => TimelineEntry[] | undefined,
): void {
  const rows = readTimeline(qc, issueId);
  const next = change(rows);
  if (!next || next === rows || sameRows(rows, next)) return;
  write(
    qc,
    issueId,
    qc.getQueryData<string[]>(issueKeys.timeline(issueId)),
    qc.getQueryData<Entities>(issueKeys.timelineEntities(issueId)),
    next,
  );
}

/** Put back rows captured by `readTimeline` (optimistic rollback). */
export function restoreTimeline(
  qc: QueryClient,
  issueId: string,
  rows: TimelineEntry[] | undefined,
): void {
  if (rows) updateTimeline(qc, issueId, () => rows);
}

/**
 * Observe an issue timeline without fetching it. Undefined until a fetch or a
 * write has populated it. The array keeps its identity while no row changed.
 */
export function useTimelineRows(issueId: string): TimelineEntry[] | undefined {
  const { data: ids } = useQuery({ ...issueTimelineOptions(issueId), enabled: false });
  return useResolvedRows(issueId, ids);
}

/** Resolve an observed id list against the issue's entity table. */
export function useResolvedRows(issueId: string, ids: string[] | undefined): TimelineEntry[] | undefined {
  const { data: entities } = useQuery({
    queryKey: issueKeys.timelineEntities(issueId),
    queryFn: skipToken,
  });
  const previous = useRef<TimelineEntry[] | undefined>(undefined);
  return useMemo(() => {
    const rows = ids && resolveEntities(entities as Entities | undefined, ids);
    if (sameRows(previous.current, rows)) return previous.current;
    previous.current = rows;
    return rows;
  }, [entities, ids]);
}
