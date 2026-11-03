import type { IssueStatusCategory } from "@multica/core/types";

export type Bucket = "done" | "active" | "blocked" | "todo" | "closed";
export interface HierarchyIssue { id: string; identifier: string; title: string; status: string }
export interface GroupSummary {
  total: number;
  counts: Record<Bucket, number>;
  /** Up to NAMED_LIMIT issues worth naming: blocked first, then active. */
  named: HierarchyIssue[];
  /** `counts` minus the named issues, for the "+N active · +N todo" line. */
  rest: Record<Bucket, number>;
}

const NAMED_LIMIT = 3;
const NAMED_BUCKETS: Bucket[] = ["blocked", "active"];
const emptyCounts = (): Record<Bucket, number> => ({ done: 0, active: 0, blocked: 0, todo: 0, closed: 0 });

// "blocked" is the built-in status key; custom statuses fall into their category.
export function bucketOf(status: string, category: IssueStatusCategory): Bucket {
  if (status === "blocked") return "blocked";
  return category === "started" ? "active" : category === "unstarted" ? "todo" : category;
}

export function summarizeGroup(issues: HierarchyIssue[], categoryOf: (status: string) => IssueStatusCategory): GroupSummary {
  const counts = emptyCounts();
  const byBucket = new Map<Bucket, HierarchyIssue[]>();
  for (const issue of issues) {
    const bucket = bucketOf(issue.status, categoryOf(issue.status));
    counts[bucket]++;
    byBucket.set(bucket, [...(byBucket.get(bucket) ?? []), issue]);
  }
  const named = NAMED_BUCKETS.flatMap((bucket) => byBucket.get(bucket) ?? []).slice(0, NAMED_LIMIT);
  const rest = { ...counts };
  for (const issue of named) rest[bucketOf(issue.status, categoryOf(issue.status))]--;
  return { total: issues.length, counts, named, rest };
}
