"use client";

import type { RefObject } from "react";
import { useWorkspacePaths } from "@multica/core/paths";
import type { Issue } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { AppLink } from "../../../navigation";
import { useT } from "../../../i18n";
import { FocusCanvas } from "./hierarchy-focus-canvas";
import { CARD_CLASS, CURRENT_CLASS, GroupTotals, IssueCardBody, type Catalog } from "./hierarchy-parts";
import type { Hierarchy } from "./hierarchy-layout";
import { useHierarchy } from "./use-hierarchy";

/** Hierarchy mode, sidebar card: the minimap tree plus the wake / mention counts. */
export function HierarchyCard({ issue, onOpenCanvas }: { issue: Issue; onOpenCanvas: () => void }) {
  const { t } = useT("issues");
  const { hierarchy, loading, error, catalog, wakeCount, mentionCount, refetch } = useHierarchy(issue, { wakes: false, mentions: false });
  return (
    <>
      <div className="p-3" aria-busy={loading}>
        {error ? (
          <div role="alert" className="text-caption">
            <p>{t(($) => $.hierarchy.load_failed)}</p>
            <Button type="button" variant="outline" size="sm" className="mt-2" onClick={refetch}>{t(($) => $.hierarchy.retry)}</Button>
          </div>
        ) : hierarchy ? <MinimapTree hierarchy={hierarchy} catalog={catalog} onOpenCanvas={onOpenCanvas} /> : <MinimapSkeleton />}
      </div>
      {hierarchy && (
        <p className="border-t border-surface-border px-3 py-1.5 text-micro text-muted-foreground">
          {[
            wakeCount && (wakeCount.value === null ? t(($) => $.hierarchy.wakes_unknown) : wakeCount.partial ? t(($) => $.hierarchy.wakes_more, { count: wakeCount.value }) : t(($) => $.hierarchy.wakes, { count: wakeCount.value })),
            mentionCount && (mentionCount.value === null ? t(($) => $.hierarchy.mentions_unknown) : mentionCount.partial ? t(($) => $.hierarchy.mentions_more, { count: mentionCount.value }) : t(($) => $.hierarchy.mentions, { count: mentionCount.value })),
          ].filter(Boolean).join(" · ")}
        </p>
      )}
    </>
  );
}

/** Hierarchy mode, dialog body: the focus-depth canvas (lock A shows no relation chips). */
export function HierarchyDialogBody({ issue, viewportRef, onOpen }: { issue: Issue; viewportRef: RefObject<HTMLDivElement | null>; onOpen: (id: string) => void }) {
  const { catalog } = useHierarchy(issue, { wakes: false, mentions: false });
  return <FocusCanvas root={{ id: issue.id, identifier: issue.identifier, title: issue.title, status: issue.status }} catalog={catalog} viewportRef={viewportRef} onOpen={onOpen} />;
}

function MinimapTree({ hierarchy, catalog, onOpenCanvas }: { hierarchy: Hierarchy; catalog: Catalog; onOpenCanvas: () => void }) {
  const { t } = useT("issues");
  const paths = useWorkspacePaths();
  const { parent, current, siblings, children } = hierarchy;
  const leaf = "min-w-0 rounded-lg border border-dashed border-surface-border bg-muted/20 px-2 py-1.5 text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring";
  return (
    <div className="flex flex-col">
      {parent && (
        <>
          <AppLink href={paths.issueDetail(parent.id)} aria-label={`${t(($) => $.hierarchy.parent)} ${parent.identifier}`} className={CARD_CLASS}><IssueCardBody issue={parent} catalog={catalog} /></AppLink>
          <span aria-hidden="true" className="mx-auto h-3 w-0.5 bg-border" />
        </>
      )}
      <div className="flex items-start gap-2">
        {siblings.total > 0 && (
          <button type="button" onClick={onOpenCanvas} className={`${leaf} w-[38%] shrink-0`}>
            <GroupTotals summary={siblings} label={t(($) => $.hierarchy.siblings, { count: siblings.total })} catalog={catalog} />
          </button>
        )}
        <div aria-current="true" className={`${CURRENT_CLASS} flex-1`}><IssueCardBody issue={current} catalog={catalog} current /></div>
      </div>
      {children.total > 0 && (
        <>
          <span aria-hidden="true" className="mx-auto h-3 w-0.5 bg-border" />
          <button type="button" onClick={onOpenCanvas} className={`${leaf} ml-[38%]`}>
            <GroupTotals summary={children} label={t(($) => $.hierarchy.children, { count: children.total })} catalog={catalog} />
          </button>
        </>
      )}
    </div>
  );
}

/** Same footprint as the loaded tree, so the swap does not shift the sidebar. */
function MinimapSkeleton() {
  const { t } = useT("issues");
  return (
    <div role="status" aria-label={t(($) => $.hierarchy.loading)} className="flex flex-col motion-safe:animate-pulse">
      <span className="sr-only">{t(($) => $.hierarchy.loading)}</span>
      <div className="h-[52px] rounded-lg bg-muted" />
      <span className="mx-auto h-3 w-0.5 bg-muted" />
      <div className="flex gap-2"><div className="h-[82px] w-[38%] rounded-lg bg-muted" /><div className="h-[82px] flex-1 rounded-lg bg-muted" /></div>
      <span className="mx-auto h-3 w-0.5 bg-muted" />
      <div className="ml-[38%] h-[62px] rounded-lg bg-muted" />
    </div>
  );
}
