"use client";

import { useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type RefObject } from "react";
import { useWorkspacePaths } from "@multica/core/paths";
import type { TimelineNode } from "@multica/core/issues/graph-core";
import { Button } from "@multica/ui/components/ui/button";
import { Minus, Plus } from "@multica/ui/icons";
import { AppLink } from "../../../navigation";
import { useT } from "../../../i18n";
import { FocusEdges } from "./hierarchy-focus-edges";
import { focusKeyAction, nextCard } from "./hierarchy-focus-keys";
import { FOCUS_CAP, layoutFocus, type FocusItem } from "./hierarchy-focus-layout";
import { levelMotion } from "./hierarchy-level-motion";
import { summarizeGroup, type HierarchyIssue } from "./hierarchy-model";
import { CARD_CLASS, CURRENT_CLASS, GroupTotals, IssueCardBody, type Catalog } from "./hierarchy-parts";
import { useHierarchyLevels } from "./use-hierarchy-levels";

const PAD = 24;
const toIssue = ({ id, identifier, title, status }: TimelineNode): HierarchyIssue => ({ id, identifier, title, status });
const prefersReducedMotion = () => !!window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;

/**
 * Focus-depth hierarchy (CHE-4312 D4): the children of one focused card at a time (the root at depth 0), the chain
 * from the root down to that card above them, each card's children folded into a count card. Levels load lazily as the focus reaches them.
 * The viewport carries `data-level-ready` (the depth) once the focus level and its count level are settled; the
 * `level-start` / `level-ready` performance marks bracket every depth change for the performance gate.
 */
export function FocusCanvas({ root, catalog, viewportRef, onOpen }: { root: HierarchyIssue; catalog: Catalog; viewportRef: RefObject<HTMLDivElement | null>; onOpen: (id: string) => void }) {
  const { t } = useT("issues");
  const paths = useWorkspacePaths();
  const { levels, expand } = useHierarchyLevels(root.id);
  const [depth, setDepth] = useState(0);
  // The card whose children the focus level shows (depth >= 1); null at depth 0, where the level is the root itself.
  const [focusId, setFocusId] = useState<string | null>(null);
  const [activeId, setActiveId] = useState<string | null>(null);

  const deepest = Math.max(0, ...Object.entries(levels).filter(([, l]) => l.nodes.length > 0).map(([d]) => Number(d)));
  const settled = (d: number) => levels[d]?.status !== undefined && levels[d]!.status !== "loading";
  const ready = (depth === 0 || settled(depth)) && settled(depth + 1);
  const failed = [depth, depth + 1].filter((d) => d >= 1 && levels[d]?.status === "failed");

  // The count level below the focus loads when the focus reaches it; levels above are loaded already.
  const hasNext = !!levels[depth + 1];
  useEffect(() => { if (!hasNext) expand(depth); }, [depth, hasNext, expand]);

  const { layout, path, cards, canIn, hidden } = useMemo(() => {
    const parentOf = new Map<string, string | null>();
    const byId = new Map<string, HierarchyIssue>([[root.id, root]]);
    Object.values(levels).forEach((l) => l.nodes.forEach((n) => { parentOf.set(n.id, n.parentId); byId.set(n.id, toIssue(n)); }));
    const kids = new Map<string, HierarchyIssue[]>();
    levels[depth + 1]?.nodes.forEach((n) => n.parentId && kids.set(n.parentId, [...(kids.get(n.parentId) ?? []), toIssue(n)]));
    const siblings = depth === 0 ? [root] : (levels[depth]?.nodes ?? []).filter((n) => n.parentId === focusId).map(toIssue);
    const cards = siblings.slice(0, 1000);
    const active = cards.find((c) => c.id === activeId) ?? cards[0];
    const path: HierarchyIssue[] = [];
    for (let id = depth === 0 ? null : focusId, n = 0; id && n < depth; n++) {
      const issue = byId.get(id);
      if (!issue) break;
      path.unshift(issue);
      id = id === root.id ? null : parentOf.get(id) ?? null;
    }
    const level = cards.map((issue) => ({ issue, children: summarizeGroup(kids.get(issue.id) ?? [], catalog.categoryOf) }));
    const canIn = !!active && (kids.get(active.id)?.length ?? 0) > 0;
    return { layout: layoutFocus({ chain: path, level }), path, cards: cards.length, canIn, hidden: Math.max(0, cards.length - FOCUS_CAP) };
  }, [levels, depth, focusId, activeId, root, catalog.categoryOf]);
  const activeCard = layout.items.find((i): i is Extract<FocusItem, { kind: "card" }> => i.kind === "card" && i.issue.id === activeId) ?? layout.items.find((i) => i.kind === "card");
  const pending = depth > 0 && levels[depth]?.status !== "ready";
  const withChildren = layout.items.filter((i) => i.kind === "count").length;

  const pendingMark = useRef(false);
  const focusAfter = useRef<string | null>(null);
  const begin = () => { performance.mark?.("level-start"); pendingMark.current = true; };
  /** Into `parentId`'s children, one level down. */
  const stepIn = (parentId: string) => {
    begin();
    focusAfter.current = "";
    setFocusId(parentId);
    setActiveId(null);
    setDepth(depth + 1);
  };
  /** Back to `to` (< depth): the chain card there stays active. */
  const stepOut = (to: number) => {
    if (to < 0 || to >= depth) return;
    begin();
    focusAfter.current = path[to]?.id ?? "";
    setFocusId(to === 0 ? null : (path[to - 1]?.id ?? null));
    setActiveId(path[to]?.id ?? null);
    setDepth(to);
  };
  useEffect(() => {
    if (focusAfter.current === null) return;
    const target = focusAfter.current ? viewportRef.current?.querySelector<HTMLElement>(`[data-card-id="${focusAfter.current}"]`) : viewportRef.current;
    focusAfter.current = null;
    target?.focus();
  }, [depth, focusId, viewportRef]);
  useEffect(() => {
    if (!ready || !pendingMark.current) return;
    pendingMark.current = false;
    performance.mark?.("level-ready");
  }, [ready, depth]);

  // Level change: 240 ms ease-out; reduced motion changes instantly. A second step cancels the first.
  const worldRef = useRef<HTMLDivElement>(null);
  const animation = useRef<Animation | undefined>(undefined);
  const shownDepth = useRef(depth);
  useLayoutEffect(() => {
    if (shownDepth.current === depth) return;
    const direction = depth > shownDepth.current ? 1 : -1;
    shownDepth.current = depth;
    animation.current?.cancel();
    const motion = levelMotion(prefersReducedMotion());
    if (motion.durationMs === 0) return;
    animation.current = worldRef.current?.animate?.(
      [{ opacity: 0, transform: motion.slide ? `translateY(${direction * 12}px)` : "none" }, { opacity: 1, transform: "none" }],
      { duration: motion.durationMs, easing: motion.easing },
    );
  }, [depth]);
  useEffect(() => () => animation.current?.cancel(), []);

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const action = focusKeyAction(event, depth);
    if (!action) return;
    // Enter on a focused card or button keeps its native activation; so does Space on a button.
    // Space on a card link has none, so it toggles the level.
    const onViewport = event.target === event.currentTarget;
    const onButton = (event.target as HTMLElement).closest("button") !== null;
    if ((action.type === "open" && !onViewport) || (action.type === "toggle" && onButton)) return;
    event.preventDefault();
    event.stopPropagation();
    const into = () => { if (canIn && activeCard) stepIn(activeCard.issue.id); };
    if (action.type === "step-in") into();
    else if (action.type === "step-out") stepOut(depth - 1);
    else if (action.type === "toggle") { if (canIn) into(); else stepOut(depth - 1); }
    else if (action.type === "open") { if (activeCard && activeCard.issue.id !== root.id) onOpen(activeCard.issue.id); }
    else if (activeCard) {
      const next = layout.items[nextCard(layout.items, layout.items.indexOf(activeCard), action.direction)];
      if (next?.kind !== "card") return;
      setActiveId(next.issue.id);
      viewportRef.current?.querySelector<HTMLElement>(`[data-card-id="${next.issue.id}"]`)?.focus();
    }
  };

  const world = { left: PAD - layout.bounds.x, top: PAD - layout.bounds.y };
  const shape = { width: layout.bounds.w + PAD * 2, height: layout.bounds.h + PAD * 2 };
  const place = ({ x, y, w, h }: FocusItem["box"]) => ({ left: x, top: y, width: w, height: h });
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-2">
      <div className="flex flex-wrap items-center gap-1.5" role="toolbar" aria-label={t(($) => $.hierarchy.title)}>
        <Button type="button" variant="outline" size="icon-sm" aria-label={t(($) => $.hierarchy.step_out)} disabled={depth === 0} onClick={() => stepOut(depth - 1)}><Minus /></Button>
        <span aria-hidden="true" className="min-w-24 text-center text-caption tabular-nums">{t(($) => $.hierarchy.depth, { depth, max: deepest })}</span>
        <Button type="button" variant="outline" size="icon-sm" aria-label={t(($) => $.hierarchy.step_in)} disabled={!canIn} onClick={() => activeCard && stepIn(activeCard.issue.id)}><Plus /></Button>
        <span role="status" className="sr-only">{ready ? t(($) => $.hierarchy.depth_announce, { depth, max: deepest, count: cards, withChildren }) : ""}</span>
        {failed.length > 0 && (
          <span role="alert" className="flex items-center gap-2 text-caption">
            {t(($) => $.hierarchy.load_failed)}
            <Button type="button" variant="outline" size="xs" onClick={() => failed.forEach((d) => expand(d - 1))}>{t(($) => $.hierarchy.retry)}</Button>
          </span>
        )}
      </div>
      <div className="relative min-h-0 flex-1 overflow-hidden rounded-xl border border-surface-border bg-muted/20">
        <div
          ref={viewportRef}
          role="group"
          tabIndex={0}
          aria-label={t(($) => $.hierarchy.canvas_focus, { depth })}
          aria-busy={!ready}
          data-testid="hierarchy-canvas"
          data-level-ready={ready ? depth : undefined}
          onKeyDown={onKeyDown}
          className="absolute inset-0 overflow-auto outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <div ref={worldRef} data-testid="hierarchy-world" className="relative" style={shape}>
            <div className="absolute" style={world}>
              <FocusEdges edges={layout.edges} />
              {layout.items.map((item, index) => <FocusItemView key={itemKey(item, index)} item={item} root={root} catalog={catalog} style={place(item.box)} href={item.kind === "card" ? paths.issueDetail(item.issue.id) : ""} pending={pending} hidden={hidden} path={path} onOpen={onOpen} onActive={setActiveId} onStepIn={stepIn} onStepOut={stepOut} />)}
            </div>
          </div>
          {!ready && failed.length === 0 && <div role="status" aria-label={t(($) => $.hierarchy.loading)} className="absolute bottom-2 left-2 h-2 w-32 rounded-full bg-muted motion-safe:animate-pulse" />}
        </div>
      </div>
      <p className="text-micro text-muted-foreground">{t(($) => $.hierarchy.focus_hint)}</p>
    </div>
  );
}

const itemKey = (item: FocusItem, index: number) =>
  item.kind === "card" || item.kind === "chain" ? `${item.kind}:${item.issue.id}` : item.kind === "count" ? `count:${item.parentId}` : `${item.kind}:${index}`;

interface ItemProps {
  item: FocusItem;
  root: HierarchyIssue;
  catalog: Catalog;
  style: { left: number; top: number; width: number; height: number };
  href: string;
  pending: boolean;
  hidden: number;
  path: HierarchyIssue[];
  onOpen: (id: string) => void;
  onActive: (id: string) => void;
  onStepIn: (parentId: string) => void;
  onStepOut: (depth: number) => void;
}

function FocusItemView({ item, root, catalog, style, href, pending, hidden, path, onOpen, onActive, onStepIn, onStepOut }: ItemProps) {
  const { t } = useT("issues");
  const box = "absolute overflow-hidden";
  if (item.kind === "fold") {
    return <span className={`${box} flex items-center justify-center rounded-lg border border-dashed border-surface-border text-caption text-muted-foreground`} style={style} title={item.hidden.map((i) => i.identifier).join(", ")}>{t(($) => $.hierarchy.chain_folded, { count: item.hidden.length })}</span>;
  }
  if (item.kind === "chain") {
    const target = path.findIndex((p) => p.id === item.issue.id);
    return (
      // ui-pattern-ref: allow canvas chain card, positioned like its AppLink card peers; Button has no card variant
      <button type="button" className={`${CARD_CLASS} ${box} text-left`} style={style} onClick={() => onStepOut(target)}>
        <span className="font-mono text-micro font-bold text-muted-foreground">{item.issue.identifier}</span>
        <span className="line-clamp-1 break-words text-micro">{item.issue.title}</span>
      </button>
    );
  }
  if (item.kind === "count") {
    return (
      // ui-pattern-ref: allow canvas count card, positioned like its AppLink card peers; Button has no card variant
      <button type="button" className={`${box} rounded-xl border border-surface-border bg-card px-2.5 py-1.5 text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring`} style={style} onClick={() => onStepIn(item.parentId)}>
        <GroupTotals summary={item.summary} label={t(($) => $.hierarchy.children, { count: item.summary.total })} catalog={catalog} />
      </button>
    );
  }
  if (item.kind === "more") {
    return <div className={`${box} flex items-center justify-center rounded-lg border border-dashed border-surface-border text-caption text-muted-foreground`} style={style}>{pending ? t(($) => $.hierarchy.more_cards_pending, { count: hidden }) : t(($) => $.hierarchy.more_cards, { count: hidden })}</div>;
  }
  // Dashed border = leaf, solid = has children (same rule as the edge into the card).
  if (item.issue.id === root.id) {
    return <div aria-current="true" data-card-id={item.issue.id} tabIndex={-1} className={`${CURRENT_CLASS} ${box}`} style={style} onFocus={() => onActive(item.issue.id)}><IssueCardBody issue={item.issue} catalog={catalog} current large /></div>;
  }
  return (
    <AppLink
      href={href}
      data-card-id={item.issue.id}
      className={`${CARD_CLASS} ${box} ${item.leaf ? "border-dashed" : ""}`}
      style={style}
      onFocus={() => onActive(item.issue.id)}
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
