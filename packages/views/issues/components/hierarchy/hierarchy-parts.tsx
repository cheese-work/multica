"use client";

import type { IssueStatusCatalog } from "@multica/core/issue-statuses/queries";
import { useT } from "../../../i18n";
import { bucketOf, type Bucket, type GroupSummary, type HierarchyIssue } from "./hierarchy-model";

export const BUCKET_ORDER: Bucket[] = ["done", "active", "blocked", "todo", "closed"];
const BUCKET_COLOR: Record<Bucket, string> = {
  done: "var(--info)", active: "var(--warning)", blocked: "var(--destructive)", todo: "var(--muted-foreground)", closed: "var(--faint-foreground)",
};

export type Catalog = Pick<IssueStatusCatalog, "categoryOf" | "labelOf">;

export function StatusDot({ bucket, className = "size-2" }: { bucket: Bucket; className?: string }) {
  return <span aria-hidden="true" data-bucket={bucket} className={`inline-block shrink-0 rounded-full ${className}`} style={{ backgroundColor: BUCKET_COLOR[bucket] }} />;
}

export function StatusBar({ counts, total, className = "h-1.5" }: { counts: GroupSummary["counts"]; total: number; className?: string }) {
  return (
    <div aria-hidden="true" className={`flex overflow-hidden rounded-full bg-muted ${className}`}>
      {BUCKET_ORDER.map((bucket) => counts[bucket] > 0 && <i key={bucket} className="block" style={{ width: `${(counts[bucket] / total) * 100}%`, backgroundColor: BUCKET_COLOR[bucket] }} />)}
    </div>
  );
}

export function useBucketLabel() {
  const { t } = useT("issues");
  return (bucket: Bucket) => t(($) => $.hierarchy[`bucket_${bucket}`]);
}

/** "In progress" next to a dot: status is never colour-only. */
export function StatusLabel({ issue, catalog, className = "" }: { issue: HierarchyIssue; catalog: Catalog; className?: string }) {
  return (
    <span className={`inline-flex min-w-0 items-center gap-1.5 text-micro text-muted-foreground ${className}`}>
      <StatusDot bucket={bucketOf(issue.status, catalog.categoryOf(issue.status))} />
      <span className="truncate">{catalog.labelOf(issue.status)}</span>
    </span>
  );
}

/** Card body shared by the sidebar and the canvas: identifier, status, 2-line title. */
export function IssueCardBody({ issue, catalog, current = false, large = false }: { issue: HierarchyIssue; catalog: Catalog; current?: boolean; large?: boolean }) {
  const { t } = useT("issues");
  return (
    <>
      <span className="flex flex-wrap items-center justify-between gap-x-2">
        <span className={`font-mono font-bold ${current ? "text-info" : "text-muted-foreground"} ${large ? "text-caption" : "text-micro"}`}>{issue.identifier}</span>
        {current ? <span className="text-micro font-bold text-info">{t(($) => $.hierarchy.you_are_here)}</span> : <StatusLabel issue={issue} catalog={catalog} />}
      </span>
      <span className={`mt-0.5 line-clamp-2 block break-words ${current ? "font-semibold" : ""} ${large ? "text-label" : "text-caption"}`}>{issue.title}</span>
      {current && <StatusLabel issue={issue} catalog={catalog} className="mt-1" />}
    </>
  );
}

export const CARD_CLASS = "block min-w-0 rounded-lg border border-surface-border bg-muted/40 px-2.5 py-1.5 text-foreground outline-none transition-colors hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring";
export const CURRENT_CLASS = "block min-w-0 rounded-lg border-2 border-info bg-info/10 px-2.5 py-1.5 text-foreground ring-4 ring-info/20";

/** Group block shared by the sidebar leaf and the zoomed-out canvas: totals, status bar, named issues, remainder. */
export function GroupTotals({ summary, label, catalog, large = false }: { summary: GroupSummary; label: string; catalog: Catalog; large?: boolean }) {
  const { t } = useT("issues");
  const bucketLabel = useBucketLabel();
  const rest = (["blocked", "active", "todo", "done", "closed"] as const).filter((bucket) => summary.rest[bucket] > 0);
  return (
    <>
      <span className="flex items-baseline justify-between gap-1 font-bold">
        <span className={large ? "text-title" : "min-w-0"}>{label}</span>
        <span className="shrink-0 text-micro font-normal tabular-nums text-muted-foreground">{summary.counts.done}/{summary.total}</span>
      </span>
      <StatusBar counts={summary.counts} total={summary.total} className={large ? "my-2 h-2.5" : "my-1 h-1"} />
      <span className="flex flex-wrap gap-x-1 gap-y-0.5">
        {summary.named.map((issue) => {
          const bucket = bucketOf(issue.status, catalog.categoryOf(issue.status));
          return (
            <span key={issue.id} title={`${issue.identifier}: ${issue.title}`} className="inline-flex items-center gap-1 rounded-md border border-surface-border px-1 font-mono text-micro font-bold">
              <StatusDot bucket={bucket} className="size-1.5" />{issue.identifier.replace(/^\D+-/, "")}
              <span className="sr-only">{catalog.labelOf(issue.status)}</span>
            </span>
          );
        })}
      </span>
      <span className="mt-0.5 block text-micro text-muted-foreground">
        {rest.map((bucket) => t(($) => $.hierarchy.more, { count: summary.rest[bucket], label: bucketLabel(bucket) })).join(" · ")}
      </span>
    </>
  );
}
