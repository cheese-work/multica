import { z } from "zod";
import type { IssueTableFilters, IssueTableScope } from "../types";

// Wire contracts of POST /api/issues/timeline and POST /api/issues/relations/batch
// (CHE-4312 S3). A malformed body throws (an empty graph would look like "no
// relations"); an unknown enum value falls back so one new server value never
// blanks a whole page.

const uuid = z.string().uuid();
const time = z.string().nullable();

export const TIMELINE_CATEGORIES = ["unstarted", "started", "done", "closed", "unknown"] as const;
export const WAKEUP_CLASSES = ["self", "cross", "aggregate_children", "external", "unknown_source"] as const;

const TimelineSegmentSchema = z.object({
  row_id: z.string().optional(),
  status: z.string(),
  category: z.enum(TIMELINE_CATEGORIES).catch("unknown"),
  from: time,
  to: time,
  clipped_start: z.boolean().catch(false),
  clipped_end: z.boolean().catch(false),
});

const TimelineIssueSchema = z.object({
  issue_id: uuid,
  status: z.string(),
  history: z.enum(["complete", "partial", "none"]).catch("none"),
  start_known: z.boolean().catch(false),
  actual_start: time,
  actual_end: time,
  presence_from: time,
  presence_to: time,
  start_date: time,
  due_date: time,
  segments: z.array(TimelineSegmentSchema),
});

const WakeupEventSchema = z.object({
  id: z.string(),
  at: z.string(),
  target_issue_id: uuid,
  rule_id: z.string(),
  rule_kind: z.string(),
  condition_type: z.string(),
  source_issue_id: uuid.nullable(),
  class: z.enum(WAKEUP_CLASSES).catch("unknown_source"),
  stage: z.number().int().nullish(),
  run_id: z.string().nullable(),
});

export const IssueTimelineResponseSchema = z.object({
  as_of: z.string(),
  window: z.object({ from: z.string(), to: z.string() }),
  issues: z.array(TimelineIssueSchema),
  wakeup_events: z.array(WakeupEventSchema),
  next_cursor: z.string().nullable(),
});

export const EDGE_TYPES = ["parent", "duplicate", "wakeup", "children_done", "mention", "shared_pr", "dependency"] as const;

export const RelationsBatchResponseSchema = z.object({
  nodes: z.array(z.object({
    id: uuid,
    identifier: z.string(),
    title: z.string(),
    status: z.string(),
    parent_issue_id: uuid.nullable(),
    /** In the workspace but outside the requested ids. */
    context: z.boolean(),
  })),
  edges: z.array(z.object({ source: uuid, target: uuid, type: z.enum([...EDGE_TYPES, "unknown"]).catch("unknown") })),
  next_cursor: z.string().nullable(),
});

export type IssueTimelineResponse = z.infer<typeof IssueTimelineResponseSchema>;
export type RelationsBatchResponse = z.infer<typeof RelationsBatchResponseSchema>;
export type TimelineWakeupEvent = IssueTimelineResponse["wakeup_events"][number];

export interface IssueTimelineRequest {
  scope: IssueTableScope;
  filters: IssueTableFilters;
  /** Omit for the server default: the last 30 days. */
  window?: { from: string; to: string };
  page?: { cursor?: string | null; limit?: number };
}

export interface RelationsBatchRequest {
  issue_ids: string[];
  types?: (typeof EDGE_TYPES)[number][];
  include_mentions?: boolean;
  page?: { cursor?: string | null; limit?: number };
}
