import { useQuery } from "@tanstack/react-query";
import { issueKeys } from "@multica/core/issues/queries";
import type { TimelineEntry } from "@multica/core/types";

/**
 * The other half of an ask/answer pair: for an ask comment the quote that
 * replies to it (the latest one), for a quote the ask it replies to. Read from
 * the cached timeline (never fetched here), so it is undefined until the
 * timeline is loaded or when the pair is not on this issue.
 */
export function decisionCounterpartId(timeline: TimelineEntry[] | undefined, entry: TimelineEntry): string | undefined {
  if (!timeline) return undefined;
  if (entry.decision_comment_kind === "ask") {
    return timeline.findLast((e) => e.decision_comment_kind === "quote" && e.parent_id === entry.id)?.id;
  }
  if (entry.decision_comment_kind === "quote" && entry.parent_id) {
    return timeline.find((e) => e.id === entry.parent_id && e.decision_comment_kind === "ask")?.id;
  }
  return undefined;
}

export function useDecisionCounterpart(issueId: string, entry: TimelineEntry): string | undefined {
  const isDecision = entry.decision_comment_kind === "ask" || entry.decision_comment_kind === "quote";
  const { data } = useQuery({
    queryKey: issueKeys.timeline(issueId),
    queryFn: () => Promise.resolve<TimelineEntry[]>([]),
    enabled: false,
    select: (timeline: TimelineEntry[]) => (isDecision ? decisionCounterpartId(timeline, entry) : undefined),
  });
  return data;
}
