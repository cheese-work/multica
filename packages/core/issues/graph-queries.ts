import { infiniteQueryOptions, type QueryClient } from "@tanstack/react-query";
import { api } from "../api";
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
};

const isAbort = (e: unknown) => e instanceof DOMException && e.name === "AbortError";

/** Walk one chunk's cursor pages, merging each page into the sinks as it loads so a later failure keeps earlier pages. */
async function fetchChunk(
  wsId: string,
  ids: string[],
  opts: RelationsOptions,
  sinks: { nodes: Map<string, RelationsNode>; edges: Map<string, RelationsEdge> },
  signal?: AbortSignal,
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
  } while (cursor);
}

/**
 * Fetch relations for any number of ids in chunks of 200 and merge them.
 * Edges dedupe by (type, source, target). A node is `context` only if every
 * response saw it as context. A failed chunk keeps the pages that loaded and
 * lists its ids in `notLoaded`; a retry refetches everything. Aborts rethrow.
 */
export async function fetchRelationsBatched(
  wsId: string,
  ids: readonly string[],
  opts: RelationsOptions,
  signal?: AbortSignal,
): Promise<RelationsResult> {
  const sinks = { nodes: new Map<string, RelationsNode>(), edges: new Map<string, RelationsEdge>() };
  const notLoaded: string[] = [];
  for (let i = 0; i < ids.length; i += RELATIONS_BATCH_CHUNK) {
    const chunk = ids.slice(i, i + RELATIONS_BATCH_CHUNK);
    try {
      await fetchChunk(wsId, chunk, opts, sinks, signal);
    } catch (e) {
      if (isAbort(e) || signal?.aborted) throw e;
      notLoaded.push(...chunk);
    }
  }
  return { nodes: [...sinks.nodes.values()], edges: [...sinks.edges.values()], incomplete: notLoaded.length > 0, notLoaded };
}

export function issueRelationsOptions(wsId: string, ids: readonly string[], opts: RelationsOptions) {
  return {
    queryKey: issueGraphKeys.relations(wsId, ids, opts),
    queryFn: ({ signal }: { signal?: AbortSignal }) => fetchRelationsBatched(wsId, ids, opts, signal),
    enabled: !!wsId && ids.length > 0,
  };
}

/** Merge timeline pages: issues by id, segments by row id, wakeup events by id; first sighting wins order. */
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

/** Dedupe by row id; a segment without one falls back to its status and span. */
function dedupeSegments<T extends { row_id?: string; status: string; from: string | null; to: string | null }>(segments: T[]): T[] {
  const seen = new Map<string, T>();
  for (const s of segments) {
    const key = s.row_id ?? `${s.status}|${s.from}|${s.to}`;
    if (!seen.has(key)) seen.set(key, s);
  }
  return [...seen.values()];
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
