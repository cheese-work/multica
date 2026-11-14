"use client";

import { useState, type RefObject } from "react";
import { useWorkspacePaths } from "@multica/core/paths";
import type { Issue } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { AppLink } from "../../../navigation";
import { useT } from "../../../i18n";
import { Canvas } from "./hierarchy-canvas";
import { CARD_CLASS, CURRENT_CLASS, GroupTotals, IssueCardBody, type Catalog } from "./hierarchy-parts";
import type { Hierarchy } from "./hierarchy-layout";
import { useHierarchy, type RelationCount } from "./use-hierarchy";

const chip = "inline-flex h-7 items-center rounded-full border border-surface-border px-2.5 text-caption font-medium aria-pressed:border-info aria-pressed:bg-info/10";

/** "…" while loading, "?" when the request failed, "N+" when the server omitted nodes. */
const chipCount = (count: RelationCount | null) => (count === null ? "…" : count.value === null ? "?" : count.partial ? `${count.value}+` : `${count.value}`);

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

/** Hierarchy mode, dialog body: relation chips and the zoomable canvas. */
export function HierarchyDialogBody({ issue, viewportRef, onOpen }: { issue: Issue; viewportRef: RefObject<HTMLDivElement | null>; onOpen: (id: string) => void }) {
  const { t } = useT("issues");
  const [wakes, setWakes] = useState(false);
  const [mentions, setMentions] = useState(false);
  const { hierarchy, catalog, wakeCount, mentionCount, mentionsLoading, error, refetch } = useHierarchy(issue, { wakes, mentions });
  const total = hierarchy ? (hierarchy.parent ? 1 : 0) + 1 + hierarchy.siblings.total + hierarchy.children.total : 0;
  return (
    <>
      <div className="flex flex-wrap items-center gap-1.5">
        <span className={chip}>{t(($) => $.hierarchy.chip_hierarchy, { count: total })}</span>
        <button type="button" className={chip} aria-pressed={wakes} onClick={() => setWakes((on) => !on)}>{t(($) => $.hierarchy.chip_wakes, { value: chipCount(wakeCount) })}</button>
        <button type="button" className={chip} aria-pressed={mentions} aria-busy={mentionsLoading} onClick={() => setMentions((on) => !on)}>
          {mentionCount === null ? t(($) => $.hierarchy.chip_mentions_off) : t(($) => $.hierarchy.chip_mentions, { value: chipCount(mentionCount) })}
        </button>
      </div>
      {error ? (
        <div role="alert"><p>{t(($) => $.hierarchy.load_failed)}</p><Button variant="outline" onClick={refetch}>{t(($) => $.hierarchy.retry)}</Button></div>
      ) : hierarchy ? (
        <Canvas hierarchy={hierarchy} catalog={catalog} viewportRef={viewportRef} onOpen={onOpen} />
      ) : (
        <div role="status" aria-label={t(($) => $.hierarchy.loading)} className="min-h-0 flex-1 rounded-xl bg-muted motion-safe:animate-pulse" />
      )}
    </>
  );
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
