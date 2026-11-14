import { infiniteQueryOptions, type QueryClient } from "@tanstack/react-query";
import { api } from "../api";
import type { Issue } from "../types";
import type {
  EDGE_TYPES,
  IssueTimelineRequest,
  IssueTimelineResponse,
  RelationsBatchResponse,
} from "../api/issue-graph-schema";

/** Ids per relations request; the server caps a batch here. */
export const RELATIONS_BATCH_CHUNK = 200;

type RelationsNode = RelationsBatchResponse["nodes"][number];
type RelationsEdge = RelationsBatchResponse["edges"][number];
type EdgeType = (typeof EDGE_TYPES)[number];

export interface RelationsOptions {
  types?: EdgeType[];
  includeMentions?: boolean;
}

export interface RelationsResult {
  nodes: RelationsNode[];
  edges: RelationsEdge[];
  /** True when at least one chunk failed; `notLoaded` names its ids. */
  incomplete: boolean;
  notLoaded: string[];
}

/** Timeline and relations queries share one prefix so one call invalidates both. */
export const issueGraphKeys = {
  all: (wsId: string) => ["issues", wsId, "graph"] as const,
  timeline: (wsId: string, request: Omit<IssueTimelineRequest, "page">) =>
    [...issueGraphKeys.all(wsId), "timeline", request] as const,
  relations: (wsId: string, ids: readonly string[], opts: RelationsOptions) =>
    [...issueGraphKeys.all(wsId), "relations", [...ids].sort(), opts] as const,
  rows: (wsId: string, ids: readonly string[]) =>
    [...issueGraphKeys.all(wsId), "rows", [...ids].sort()] as const,
};

/** Ids per issue-rows request; the server caps `ids` queries at 100 rows. */
export const ISSUE_ROWS_CHUNK = 100;

/** Issue rows (assignee, project, parent) for timeline ids, which the timeline response does not carry. */
export async function fetchIssueRowsByIds(ids: readonly string[]): Promise<Issue[]> {
  const chunks: string[][] = [];
  for (let i = 0; i < ids.length; i += ISSUE_ROWS_CHUNK) chunks.push(ids.slice(i, i + ISSUE_ROWS_CHUNK));
  const pages = await Promise.all(chunks.map((chunk) => api.listIssues({ ids: chunk, limit: ISSUE_ROWS_CHUNK })));
  return pages.flatMap((page) => page.issues);
}

export function issueRowsByIdsOptions(wsId: string, ids: readonly string[]) {
  return {
    queryKey: issueGraphKeys.rows(wsId, ids),
    queryFn: () => fetchIssueRowsByIds(ids),
    enabled: !!wsId && ids.length > 0,
  };
}

const isAbort = (e: unknown) => e instanceof DOMException && e.name === "AbortError";

/** Walk one chunk's cursor pages, merging each page into the sinks as it loads so a later failure keeps earlier pages. */
async function fetchChunk(
  wsId: string,
  ids: string[],
  opts: RelationsOptions,
  sinks: { nodes: Map<string, RelationsNode>; edges: Map<string, RelationsEdge> },
  signal?: AbortSignal,
  onPage?: () => void,
) {
  let cursor: string | null = null;
  do {
    const res: RelationsBatchResponse = await api.getIssueRelationsBatch(
      wsId,
      {
        issue_ids: ids,
        types: opts.types,
        include_mentions: opts.includeMentions,
        ...(cursor ? { page: { cursor } } : {}),
      },
      signal,
    );
    for (const n of res.nodes) {
      const prev = sinks.nodes.get(n.id);
      sinks.nodes.set(n.id, prev ? { ...prev, context: prev.context && n.context } : n);
    }
    for (const e of res.edges) sinks.edges.set(`${e.type}|${e.source}|${e.target}`, e);
    cursor = res.next_cursor;
    onPage?.();
  } while (cursor);
}

/**
 * Fetch relations for any number of ids in chunks of 200 and merge them.
 * Edges dedupe by (type, source, target). A node is `context` only if every
 * response saw it as context. A failed chunk keeps the pages that loaded and
 * lists its ids in `notLoaded`; a retry refetches everything. Aborts rethrow.
 * `onProgress` gets the merged result so far after each page, so a caller can show a partial count while paging.
 */
export async function fetchRelationsBatched(
  wsId: string,
  ids: readonly string[],
  opts: RelationsOptions,
  signal?: AbortSignal,
  onProgress?: (partial: RelationsResult) => void,
): Promise<RelationsResult> {
  const sinks = { nodes: new Map<string, RelationsNode>(), edges: new Map<string, RelationsEdge>() };
  const notLoaded: string[] = [];
  const snapshot = (): RelationsResult => ({ nodes: [...sinks.nodes.values()], edges: [...sinks.edges.values()], incomplete: notLoaded.length > 0, notLoaded: [...notLoaded] });
  for (let i = 0; i < ids.length; i += RELATIONS_BATCH_CHUNK) {
    const chunk = ids.slice(i, i + RELATIONS_BATCH_CHUNK);
    try {
      await fetchChunk(wsId, chunk, opts, sinks, signal, onProgress && (() => onProgress(snapshot())));
    } catch (e) {
      if (isAbort(e) || signal?.aborted) throw e;
      notLoaded.push(...chunk);
    }
  }
  return snapshot();
}

export function issueRelationsOptions(wsId: string, ids: readonly string[], opts: RelationsOptions) {
  return {
    queryKey: issueGraphKeys.relations(wsId, ids, opts),
    queryFn: ({ signal }: { signal?: AbortSignal }) => fetchRelationsBatched(wsId, ids, opts, signal),
    enabled: !!wsId && ids.length > 0,
  };
}

/** Merge timeline pages: issues by id, segments by row id (unioned), wakeup events by id; first sighting sets the order. */
export function mergeTimelinePages(pages: IssueTimelineResponse[]): IssueTimelineResponse {
  const last = pages[pages.length - 1]!;
  const issues = new Map<string, IssueTimelineResponse["issues"][number]>();
  const events = new Map<string, IssueTimelineResponse["wakeup_events"][number]>();
  for (const page of pages) {
    for (const issue of page.issues) {
      const prev = issues.get(issue.issue_id);
      if (!prev) {
        issues.set(issue.issue_id, { ...issue, segments: dedupeSegments(issue.segments) });
        continue;
      }
      issues.set(issue.issue_id, { ...issue, segments: dedupeSegments([...prev.segments, ...issue.segments]) });
    }
    for (const ev of page.wakeup_events) if (!events.has(ev.id)) events.set(ev.id, ev);
  }
  return { ...last, issues: [...issues.values()], wakeup_events: [...events.values()] };
}

type Segment = IssueTimelineResponse["issues"][number]["segments"][number];

const ms = (t: string) => Date.parse(t);

/**
 * Union segments that share a row id. The server clips segments to the window,
 * so a stretch that crosses a window boundary comes back once per window: keep
 * the earliest `from` (with its `clipped_start`) and the latest `to` (with its
 * `clipped_end`); a null `to` is an open end and wins. A segment without a row
 * id falls back to its status and span and is never merged.
 */
function dedupeSegments(segments: Segment[]): Segment[] {
  const byKey = new Map<string, Segment>();
  for (const s of segments) {
    const key = s.row_id ?? `${s.status}|${s.from}|${s.to}`;
    const prev = byKey.get(key);
    if (!prev || !s.row_id) {
      byKey.set(key, s);
      continue;
    }
    const head = prev.from !== null && (s.from === null || ms(s.from) < ms(prev.from)) ? s : prev;
    const tail = prev.to === null ? prev : s.to === null || ms(s.to) > ms(prev.to) ? s : prev;
    byKey.set(key, { ...prev, from: head.from, clipped_start: head.clipped_start, to: tail.to, clipped_end: tail.clipped_end });
  }
  return [...byKey.values()];
}

export function issueActualTimelineOptions(wsId: string, request: Omit<IssueTimelineRequest, "page">) {
  return infiniteQueryOptions({
    queryKey: issueGraphKeys.timeline(wsId, request),
    queryFn: ({ pageParam, signal }) =>
      api.getIssueActualTimeline(wsId, { ...request, page: { cursor: pageParam } }, signal),
    initialPageParam: null as string | null,
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    enabled: !!wsId,
  });
}

export function invalidateIssueGraph(qc: QueryClient, wsId: string) {
  return qc.invalidateQueries({ queryKey: issueGraphKeys.all(wsId) });
}
