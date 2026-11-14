"use client";

import { useMemo, useRef, type RefObject } from "react";
import { useWorkspacePaths } from "@multica/core/paths";
import { Button } from "@multica/ui/components/ui/button";
import { LocateFixed, Maximize, Minus, Plus } from "@multica/ui/icons";
import { AppLink } from "../../../navigation";
import { useT } from "../../../i18n";
import { layoutHierarchy, intersects, type Box, type Hierarchy, type LayoutItem } from "./hierarchy-layout";
import { CARD_CLASS, CURRENT_CLASS, GroupTotals, IssueCardBody, type Catalog } from "./hierarchy-parts";
import { useCanvasView } from "./use-canvas-view";

export function Canvas({ hierarchy, catalog, viewportRef, onOpen }: { hierarchy: Hierarchy; catalog: Catalog; viewportRef: RefObject<HTMLDivElement | null>; onOpen: (id: string) => void }) {
  const { t } = useT("issues");
  const layouts = useMemo(() => ({ groups: layoutHierarchy(hierarchy, "groups"), cards: layoutHierarchy(hierarchy, "cards") }), [hierarchy]);
  const canvas = useCanvasView(layouts, viewportRef);
  const { view, mode, layout, smooth, visible, bind } = canvas;
  const paths = useWorkspacePaths();
  const percent = Math.round(view.scale * 100);
  // Panning and zooming re-render this component every frame; keeping the card
  // elements memoized lets React skip reconciling them while only the world
  // transform changes. The zoom-in handler reads the live view through a ref.
  const viewRef = useRef(view);
  viewRef.current = view;
  const { reveal, zoomBy } = canvas;
  const items = useMemo(() => layout.items.map((item, index) => (
    <CanvasItem
      key={item.kind === "issue" ? `${item.role}:${item.issue.id}` : `${item.kind}:${item.role}:${index}`}
      item={item}
      catalog={catalog}
      href={item.kind === "issue" ? paths.issueDetail(item.issue.id) : undefined}
      onOpen={onOpen}
      onReveal={reveal}
      onZoomIn={() => {
        const { x, y, w, h } = item.box;
        const current = viewRef.current;
        zoomBy(1 / current.scale, { x: (x + w / 2) * current.scale + current.x, y: (y + h / 2) * current.scale + current.y });
      }}
    />
  )), [layout, catalog, paths, onOpen, reveal, zoomBy]);

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-2">
      <div className="flex flex-wrap items-center gap-1.5" role="toolbar" aria-label={t(($) => $.hierarchy.title)}>
        <Button type="button" variant="outline" size="icon-sm" aria-label={t(($) => $.hierarchy.zoom_out)} onClick={canvas.zoomOut}><Minus /></Button>
        <span role="status" className="w-12 text-center text-caption tabular-nums" aria-label={t(($) => $.hierarchy.zoom_level, { percent })}>{percent}%</span>
        <Button type="button" variant="outline" size="icon-sm" aria-label={t(($) => $.hierarchy.zoom_in)} onClick={canvas.zoomIn}><Plus /></Button>
        <Button type="button" variant="outline" size="sm" onClick={canvas.fit}><Maximize />{t(($) => $.hierarchy.fit)}</Button>
        <Button type="button" variant="outline" size="sm" onClick={canvas.me}><LocateFixed />{t(($) => $.hierarchy.me)}</Button>
        {mode === "groups" && <span className="text-caption text-muted-foreground">{t(($) => $.hierarchy.zoom_in_hint)}</span>}
      </div>
      <div className="relative min-h-0 flex-1 overflow-hidden rounded-xl border border-surface-border bg-muted/20">
        <div
          ref={viewportRef}
          role="group"
          tabIndex={0}
          aria-label={mode === "groups" ? t(($) => $.hierarchy.canvas_groups) : t(($) => $.hierarchy.canvas_cards)}
          data-testid="hierarchy-canvas"
          data-mode={mode}
          className="absolute inset-0 cursor-grab touch-none overflow-hidden outline-none focus-visible:ring-2 focus-visible:ring-ring active:cursor-grabbing"
          {...bind}
        >
          <div
            data-testid="hierarchy-world"
            className={`absolute left-0 top-0 origin-top-left will-change-transform ${smooth ? "transition-transform duration-150 ease-out motion-reduce:transition-none" : ""}`}
            style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.scale})` }}
          >
            <svg aria-hidden="true" className="pointer-events-none absolute left-0 top-0 overflow-visible text-border" width="1" height="1">
              {layout.edges.map((edge, index) => <polyline key={index} points={edge.map(([x, y]) => `${x},${y}`).join(" ")} fill="none" stroke="currentColor" strokeWidth="2" />)}
            </svg>
            {items}
          </div>
        </div>
        {visible && canvas.size && <Overview layout={layout} visible={visible} />}
      </div>
      <p className="text-micro text-muted-foreground">{t(($) => $.hierarchy.hint)}</p>
    </div>
  );
}

function CanvasItem({ item, catalog, href, onOpen, onReveal, onZoomIn }: { item: LayoutItem; catalog: Catalog; href?: string; onOpen: (id: string) => void; onReveal: (box: Box) => void; onZoomIn: () => void }) {
  const { t } = useT("issues");
  const { x, y, w, h } = item.box;
  const style = { left: x, top: y, width: w, height: h };
  if (item.kind === "label") {
    const base = item.role === "siblings" ? t(($) => $.hierarchy.label_siblings, { total: item.total }) : item.role === "children" ? t(($) => $.hierarchy.label_children, { total: item.total }) : t(($) => $.hierarchy.related);
    return <div className="absolute truncate text-caption font-bold text-muted-foreground" style={style}>{item.shown < item.total ? t(($) => $.hierarchy.label_showing, { label: base, shown: item.shown }) : base}</div>;
  }
  if (item.kind === "group") {
    const label = item.role === "siblings" ? t(($) => $.hierarchy.siblings, { count: item.summary.total }) : t(($) => $.hierarchy.children, { count: item.summary.total });
    return (
      <button type="button" onClick={onZoomIn} className="absolute overflow-hidden rounded-xl border border-dashed border-surface-border bg-card p-4 text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring" style={style} onFocus={() => onReveal(item.box)}>
        <GroupTotals summary={item.summary} label={label} catalog={catalog} large />
      </button>
    );
  }
  if (item.role === "current") {
    return <div aria-current="true" className={`${CURRENT_CLASS} absolute overflow-hidden`} style={style}><IssueCardBody issue={item.issue} catalog={catalog} current large /></div>;
  }
  return (
    <AppLink
      href={href!}
      className={`${CARD_CLASS} absolute overflow-hidden`}
      style={style}
      onFocus={() => onReveal(item.box)}
      onClick={(event) => {
        if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || event.button !== 0) return;
        event.preventDefault();
        onOpen(item.issue.id);
      }}
    >
      <IssueCardBody issue={item.issue} catalog={catalog} large />
    </AppLink>
  );
}

const OVERVIEW = { w: 168, h: 104 };

/** Whole-hierarchy thumbnail: the visible window outlined, cards outside it highlighted. */
function Overview({ layout, visible }: { layout: ReturnType<typeof layoutHierarchy>; visible: Box }) {
  const { t } = useT("issues");
  const { bounds } = layout;
  const scale = Math.min(OVERVIEW.w / bounds.w, OVERVIEW.h / bounds.h);
  const blocks = layout.items.filter((item) => item.kind !== "label");
  const off = blocks.filter((item) => !intersects(item.box, visible));
  return (
    <div className="pointer-events-none absolute bottom-2 right-2 rounded-lg border border-surface-border bg-card/90 p-1.5 shadow-sm">
      <svg role="img" aria-label={t(($) => $.hierarchy.overview)} width={OVERVIEW.w} height={OVERVIEW.h} viewBox={`${bounds.x} ${bounds.y} ${OVERVIEW.w / scale} ${OVERVIEW.h / scale}`}>
        {blocks.map((item, index) => {
          const current = item.kind === "issue" && item.role === "current";
          const hidden = !intersects(item.box, visible);
          return <rect key={index} data-testid={hidden ? "overview-offscreen" : undefined} x={item.box.x} y={item.box.y} width={item.box.w} height={item.box.h} rx={6 / scale} className={current ? "fill-info" : hidden ? "fill-warning" : "fill-muted-foreground/40"} />;
        })}
        <rect x={visible.x} y={visible.y} width={visible.w} height={visible.h} fill="none" strokeWidth={1.5 / scale} className="stroke-foreground" />
      </svg>
      <p className="mt-1 text-center text-micro text-muted-foreground">{t(($) => $.hierarchy.overview)}{off.length > 0 && <> · {t(($) => $.hierarchy.offscreen, { count: off.length })}</>}</p>
    </div>
  );
}
