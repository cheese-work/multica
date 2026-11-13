import type { TimelineEntry } from "@multica/core/types";

/** One status change, read from a `status_changed` activity row. */
export interface StatusHistoryItem {
  id: string;
  createdAt: string;
  from: string;
  to: string;
  /** False for a legacy row: no `details.trigger`, or one that is not an object. */
  recorded: boolean;
  causeKind: string | null;
  authSource: string | null;
  /** Only a link the server kept; an invalid one is dropped and sets `refInvalid`. */
  handoffCommentId: string | null;
  refInvalid: boolean;
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

function asString(value: unknown): string | null {
  return typeof value === "string" && value !== "" ? value : null;
}

function toItem(entry: TimelineEntry): StatusHistoryItem {
  const details = asRecord(entry.details) ?? {};
  const trigger = asRecord(details.trigger);
  return {
    id: entry.id,
    createdAt: entry.created_at,
    from: asString(details.from) ?? "",
    to: asString(details.to) ?? "",
    recorded: trigger !== null,
    causeKind: asString(asRecord(trigger?.cause)?.kind),
    authSource: asString(trigger?.auth_source),
    handoffCommentId: asString(trigger?.handoff_comment_id),
    refInvalid: trigger?.ref_invalid === true,
  };
}

/**
 * The issue's status changes, ordered by `(created_at, id)` — the server's
 * ordering key. `created_at` arrives at second precision, so rows inside one
 * second fall back to `id`, which is a UUIDv7 and sorts by creation.
 */
export function buildStatusHistory(entries: readonly TimelineEntry[]): StatusHistoryItem[] {
  return entries
    .filter((e) => e.type === "activity" && e.action === "status_changed")
    .map(toItem)
    .sort((a, b) => {
      const byTime = Date.parse(a.createdAt) - Date.parse(b.createdAt);
      if (byTime !== 0 && !Number.isNaN(byTime)) return byTime;
      return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
    });
}
