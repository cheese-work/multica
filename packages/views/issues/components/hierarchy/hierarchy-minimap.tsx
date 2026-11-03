"use client";

import { useEffect, useId, useRef, useState } from "react";
import { useWorkspacePaths } from "@multica/core/paths";
import type { Issue } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Maximize2 } from "@multica/ui/icons";
import { AppLink } from "../../../navigation";
import { useT } from "../../../i18n";
import { CARD_CLASS, CURRENT_CLASS, GroupTotals, IssueCardBody, type Catalog } from "./hierarchy-parts";
import { HierarchyDialog } from "./hierarchy-dialog";
import type { Hierarchy } from "./hierarchy-layout";
import { useHierarchy } from "./use-hierarchy";

/**
 * Set by the dialog when a card opens another issue: the collapsed minimap of
 * the target takes focus on mount. Module-level because the target's minimap
 * may be a new instance by then (D4).
 */
let focusHeadingOnMount = false;
export const requestHierarchyFocus = () => { focusHeadingOnMount = true; };

export function IssueHierarchy({ issue }: { issue: Issue }) {
  const { t } = useT("issues");
  const headingId = useId();
  const headingRef = useRef<HTMLHeadingElement>(null);
  const [open, setOpen] = useState(false);
  const { hierarchy, loading, error, catalog, wakeCount, mentionCount, refetch } = useHierarchy(issue, { wakes: false, mentions: false });

  useEffect(() => {
    if (!focusHeadingOnMount) return;
    focusHeadingOnMount = false;
    headingRef.current?.focus();
  }, [issue.id]);

  return (
    <section aria-labelledby={headingId} aria-busy={loading} data-testid="issue-hierarchy" className="overflow-hidden rounded-xl border border-surface-border bg-card">
      <div className="flex items-center justify-between border-b border-surface-border py-1.5 pl-3 pr-2">
        <h3 id={headingId} ref={headingRef} tabIndex={-1} className="text-label font-bold outline-none">{t(($) => $.hierarchy.title)}</h3>
        <Button type="button" variant="outline" size="icon-sm" aria-label={t(($) => $.hierarchy.expand)} disabled={!hierarchy} onClick={() => setOpen(true)}>
          <Maximize2 className="size-3.5" />
        </Button>
      </div>
      <div className="p-3">
        {error ? (
          <div role="alert" className="text-caption">
            <p>{t(($) => $.hierarchy.load_failed)}</p>
            <Button type="button" variant="outline" size="sm" className="mt-2" onClick={refetch}>{t(($) => $.hierarchy.retry)}</Button>
          </div>
        ) : hierarchy ? <MinimapTree hierarchy={hierarchy} catalog={catalog} onOpenCanvas={() => setOpen(true)} /> : <MinimapSkeleton />}
      </div>
      {hierarchy && (
        <p className="border-t border-surface-border px-3 py-1.5 text-micro text-muted-foreground">
          {[
            wakeCount && (wakeCount.value === null ? t(($) => $.hierarchy.wakes_unknown) : wakeCount.partial ? t(($) => $.hierarchy.wakes_more, { count: wakeCount.value }) : t(($) => $.hierarchy.wakes, { count: wakeCount.value })),
            mentionCount && (mentionCount.value === null ? t(($) => $.hierarchy.mentions_unknown) : mentionCount.partial ? t(($) => $.hierarchy.mentions_more, { count: mentionCount.value }) : t(($) => $.hierarchy.mentions, { count: mentionCount.value })),
          ].filter(Boolean).join(" · ")}
        </p>
      )}
      {open && <HierarchyDialog issue={issue} headingRef={headingRef} onClose={() => setOpen(false)} />}
    </section>
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
