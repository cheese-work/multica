"use client";

import { useState, type KeyboardEvent } from "react";
import type { IssueRelations } from "@multica/core/types";
import { filterRelations, RELATION_FILTERS, type RelationFilter } from "@multica/core/issues/relations";
import type { IssueStatusCatalog } from "@multica/core/issue-statuses/queries";
import { useRelationsFilterStore } from "@multica/core/issues/stores/relations-filter-store";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@multica/ui/components/ui/tabs";
import { useT } from "../../i18n";

type RelationNode = IssueRelations["nodes"][number];
const STATUS_COLOR = { unstarted: "var(--muted-foreground)", started: "var(--warning)", done: "var(--info)", closed: "var(--muted-foreground)" } as const;
const EDGE_FILTER = { parent: "parent", wakeup: "wakeup", children_done: "wakeup", mention: "mention" } as const;
const EDGE_DASH = { parent: undefined, wakeup: "8 4", mention: "2 4" } as const;

function phaseOf(node: RelationNode) {
  const suffix = node.title.match(/\(([A-Z][A-Z0-9]*-\d+)\s+([a-z]+\d+)\)\s*$/i);
  return { phase: suffix?.[2], title: suffix ? node.title.slice(0, suffix.index).trim() : node.title };
}
function sortNodes(nodes: RelationNode[]) {
  return [...nodes].sort((first, second) => (phaseOf(first).phase ?? "~").localeCompare(phaseOf(second).phase ?? "~", "en", { numeric: true }) || first.identifier.localeCompare(second.identifier, "en", { numeric: true }));
}

function Bracket({ count }: { count: number }) {
  const height = count * 44 - 8;
  return <svg aria-hidden="true" className="pointer-events-none absolute -left-8 top-0 w-8 overflow-visible text-muted-foreground/60" height={height} viewBox={`0 0 32 ${height}`} fill="none" stroke="currentColor" strokeWidth="1.5">
    <path d={`M0 ${height / 2} H12 M12 18 V${height - 18}`} />
    {Array.from({ length: count }, (_, index) => <path key={index} d={`M12 ${index * 44 + 18} H32`} />)}
  </svg>;
}

export function IssueRelationsGraph({ graph: fullGraph, currentIssueId, catalog, onNavigate, hrefForIssue, mentionsPending = false }: {
  graph: IssueRelations;
  currentIssueId: string;
  catalog: Pick<IssueStatusCatalog, "categoryOf" | "colorOf" | "labelOf">;
  onNavigate: (issueId: string) => void;
  hrefForIssue: (issueId: string) => string;
  mentionsPending?: boolean;
}) {
  const { t } = useT("issues");
  const hidden = useRelationsFilterStore((state) => state.hidden);
  const toggle = useRelationsFilterStore((state) => state.toggle);
  const [view, setView] = useState("graph");
  const graph = filterRelations(fullGraph, currentIssueId, hidden);
  const current = graph.nodes.find((node) => node.id === currentIssueId);
  const nodesById = new Map(graph.nodes.map((node) => [node.id, node]));
  const parentEdges = graph.edges.filter((edge) => edge.type === "parent" && nodesById.has(edge.source) && nodesById.has(edge.target));
  const parents = sortNodes(graph.nodes.filter((node) => node.id !== currentIssueId && parentEdges.some((edge) => edge.target === currentIssueId && edge.source === node.id)));
  const assigned = new Set([currentIssueId, ...parents.map((node) => node.id)]);
  const children = sortNodes(graph.nodes.filter((node) => !assigned.has(node.id) && parentEdges.some((edge) => edge.source === currentIssueId && edge.target === node.id)));
  children.forEach((node) => assigned.add(node.id));
  const grandchildren = sortNodes(graph.nodes.filter((node) => !assigned.has(node.id) && parentEdges.some((edge) => children.some((child) => child.id === edge.source) && edge.target === node.id)));
  grandchildren.forEach((node) => assigned.add(node.id));
  const related = sortNodes(graph.nodes.filter((node) => !assigned.has(node.id)));
  const directChildren = fullGraph.nodes.filter((node) => fullGraph.edges.some((edge) => edge.type === "parent" && edge.source === currentIssueId && edge.target === node.id));
  const counts = { unstarted: 0, started: 0, done: 0, closed: 0 };
  directChildren.forEach((node) => { counts[catalog.categoryOf(node.status)]++; });
  const parent = fullGraph.nodes.find((node) => fullGraph.edges.some((edge) => edge.type === "parent" && edge.target === currentIssueId && edge.source === node.id));
  const filterCounts = { parent: 0, wakeup: 0, mention: 0 };
  fullGraph.edges.forEach((edge) => {
    const filter = EDGE_FILTER[edge.type as keyof typeof EDGE_FILTER];
    if (filter) filterCounts[filter]++;
  });
  const countLabels = {
    unstarted: t(($) => $.relations.todo_count), started: t(($) => $.relations.in_progress_count),
    done: t(($) => $.relations.done_count), closed: t(($) => $.relations.closed_count),
  };
  const chipLabels: Record<RelationFilter, string> = { parent: t(($) => $.relations.parent_filter), wakeup: t(($) => $.relations.wakeup), mention: t(($) => $.relations.mention) };

  function moveFocus(event: KeyboardEvent<HTMLDivElement>) {
    if (!["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"].includes(event.key)) return;
    const target = event.target as HTMLElement;
    if (!target.matches("a[data-issue-node]")) return;
    const links = Array.from(target.closest('[role="tabpanel"]')!.querySelectorAll<HTMLAnchorElement>("a[data-issue-node]"));
    const index = links.indexOf(target as HTMLAnchorElement);
    const offset = event.key === "ArrowLeft" || event.key === "ArrowUp" ? -1 : 1;
    event.preventDefault();
    links[(index + offset + links.length) % links.length]?.focus();
  }

  function card(node: RelationNode, compact = false, sourceLabel?: string) {
    const isCurrent = node.id === currentIssueId;
    const { phase, title } = phaseOf(node);
    const label = `${node.identifier}: ${node.title} (${node.status})`;
    const color = catalog.colorOf(node.status) ?? STATUS_COLOR[catalog.categoryOf(node.status)];
    return <a key={node.id} data-issue-node data-phase={phase} href={hrefForIssue(node.id)} aria-label={label} title={label} aria-current={isCurrent ? "page" : undefined}
      onClick={(event) => { if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return; event.preventDefault(); onNavigate(node.id); }}
      className={`min-w-0 rounded-xl border text-foreground outline-none transition-colors hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring ${isCurrent ? "border-info bg-info/5 ring-2 ring-info/40" : "border-surface-border bg-surface-raised"} ${compact ? "flex h-9 items-center gap-2 px-3 text-caption" : "block space-y-2 p-3 text-body"}`}>
      {compact ? <>
        <span data-status-dot className="size-2 shrink-0 rounded-full" style={{ backgroundColor: color }} />
        {phase && <span className="shrink-0 rounded-sm bg-muted px-1 font-mono text-micro">{phase}</span>}
        <span className="shrink-0 font-mono text-muted-foreground">{node.identifier}</span>
        {sourceLabel && <span className="shrink-0 text-micro text-muted-foreground">← {sourceLabel}</span>}
        <span className="truncate">{title}</span>
        <span className="sr-only">{catalog.labelOf(node.status)}</span>
      </> : <>
        <span className="flex flex-wrap items-center justify-between gap-2 text-caption"><span className="font-mono font-semibold text-muted-foreground">{node.identifier}</span><span className="inline-flex items-center gap-1 rounded-full bg-muted px-2 py-0.5"><span data-status-dot className="size-2 rounded-full" style={{ backgroundColor: color }} />{catalog.labelOf(node.status)}</span></span>
        <span className={`line-clamp-3 block ${isCurrent ? "font-semibold" : ""}`}>{title}</span>
        {phase && <span className="inline-block rounded-sm bg-muted px-1 font-mono text-micro">{phase}</span>}
        {isCurrent && directChildren.length > 0 && <span className="flex items-center gap-2 text-micro text-info"><span className="h-1 min-w-0 flex-1 overflow-hidden rounded-full bg-muted"><span className="block h-full bg-info" style={{ width: `${counts.done / directChildren.length * 100}%` }} /></span>{counts.done}/{directChildren.length}</span>}
      </>}
    </a>;
  }

  return <Tabs data-testid="relations-report" value={view} onValueChange={(value) => setView(String(value))} className="flex min-h-0 flex-1 flex-col gap-3" onKeyDown={moveFocus}>
    <div className="flex flex-wrap items-center justify-between gap-3">
      <TabsList><TabsTrigger value="graph">{t(($) => $.relations.graph)}</TabsTrigger><TabsTrigger value="list">{t(($) => $.relations.list)}</TabsTrigger></TabsList>
      <div role="group" aria-label={t(($) => $.relations.filter_label)} className="flex flex-wrap gap-2">
        {RELATION_FILTERS.map((filter) => <button key={filter} type="button" aria-pressed={!hidden.includes(filter)} aria-busy={filter === "mention" && mentionsPending} onClick={() => toggle(filter)} className={`inline-flex min-h-8 items-center gap-2 rounded-full border px-3 py-1 text-caption outline-none focus-visible:ring-2 focus-visible:ring-ring ${hidden.includes(filter) ? "border-dashed border-surface-border text-muted-foreground" : "border-surface-border bg-surface-raised text-foreground"}`}>
          <svg width="20" height="8" aria-hidden="true"><line x1="0" y1="4" x2="20" y2="4" stroke="currentColor" strokeWidth="1.5" strokeDasharray={EDGE_DASH[filter]} /></svg>
          {chipLabels[filter]} <span className="font-semibold tabular-nums">{filter === "mention" && hidden.includes(filter) ? "–" : filter === "mention" && mentionsPending ? "…" : filterCounts[filter]}</span>
        </button>)}
      </div>
    </div>
    <div className="grid grid-cols-2 gap-2 text-caption lg:grid-cols-[1.6fr_1fr_1fr_1fr]">
      <section className="space-y-2 rounded-xl border border-surface-border bg-surface-raised p-3">
        <h3 className="text-micro uppercase tracking-wide text-muted-foreground">{t(($) => $.relations.sub_issues)}</h3>
        <div className="text-title-lg font-semibold tabular-nums">{counts.done} / {directChildren.length} <span className="text-caption text-info">{countLabels.done}</span></div>
        <div role="progressbar" aria-label={t(($) => $.relations.sub_issues)} aria-valuemin={0} aria-valuemax={directChildren.length || 1} aria-valuenow={counts.done} className="flex h-1.5 overflow-hidden rounded-full bg-muted">
          {(["done", "started", "unstarted", "closed"] as const).map((category) => <span key={category} style={{ width: `${directChildren.length ? counts[category] / directChildren.length * 100 : 0}%`, backgroundColor: STATUS_COLOR[category] }} />)}
        </div>
        <div className="flex flex-wrap gap-x-3 gap-y-1 text-micro text-muted-foreground">{(["done", "started", "unstarted", "closed"] as const).filter((category) => category !== "closed" || counts.closed > 0).map((category) => <span key={category} className="inline-flex items-center gap-1"><span className="size-2 rounded-full" style={{ backgroundColor: STATUS_COLOR[category] }} />{countLabels[category]} {counts[category]}</span>)}</div>
      </section>
      <section className="space-y-1 rounded-xl border border-surface-border bg-surface-raised p-3"><h3 className="text-micro uppercase tracking-wide text-muted-foreground">{t(($) => $.relations.parent_summary)}</h3><p className="font-mono font-semibold">{parent?.identifier ?? "–"}</p><p className="text-muted-foreground">{parent ? catalog.labelOf(parent.status) : t(($) => $.relations.no_parent)}</p></section>
      <section className="space-y-1 rounded-xl border border-surface-border bg-surface-raised p-3"><h3 className="text-micro uppercase tracking-wide text-muted-foreground">{t(($) => $.relations.wakeup)}</h3><p className="text-title-lg font-semibold tabular-nums">{filterCounts.wakeup}</p><p className="text-muted-foreground">{t(($) => $.relations.wakeup)}</p></section>
      <section className="space-y-1 rounded-xl border border-surface-border bg-surface-raised p-3"><h3 className="text-micro uppercase tracking-wide text-muted-foreground">{t(($) => $.relations.mention)}</h3><p className="text-title-lg font-semibold tabular-nums">{hidden.includes("mention") ? "–" : mentionsPending ? "…" : filterCounts.mention}</p><p className="text-muted-foreground">{hidden.includes("mention") ? t(($) => $.relations.mentions_hint) : t(($) => $.relations.mention)}</p></section>
    </div>
    <TabsContent value="graph" data-view="graph" className="min-h-0 overflow-auto rounded-xl border border-surface-border bg-surface" style={{ backgroundImage: "radial-gradient(var(--surface-border) 1px, transparent 1px)", backgroundSize: "20px 20px" }}>
      <div className="grid min-h-full items-center gap-8 p-7" style={{ gridTemplateColumns: `${parents.length ? "minmax(180px, 0.8fr) " : ""}minmax(220px, 1fr)${children.length ? " minmax(300px, 1.5fr)" : ""}${grandchildren.length ? " minmax(300px, 1.5fr)" : ""}` }}>
        {parents.length > 0 && <div data-relations-column="parent" className="relative space-y-2 after:absolute after:-right-8 after:top-1/2 after:h-px after:w-8 after:bg-muted-foreground/60">{parents.map((node) => card(node))}</div>}
        <div data-relations-column="0">{current && card(current)}</div>
        {children.length > 0 && <div data-relations-column="1" className="relative flex flex-col gap-2"><Bracket count={children.length} />{children.map((node) => card(node, true))}</div>}
        {grandchildren.length > 0 && <div data-relations-column="2" className="relative flex flex-col gap-2"><Bracket count={grandchildren.length} />{grandchildren.map((node) => card(node, true, nodesById.get(parentEdges.find((edge) => edge.target === node.id)?.source ?? "")?.identifier))}</div>}
      </div>
      {related.length > 0 && <div className="flex flex-col gap-2 border-t border-surface-border p-4">{related.map((node) => <div key={node.id} className="space-y-1"><p className="text-micro text-muted-foreground">{Array.from(new Set(graph.edges.filter((edge) => edge.source === node.id || edge.target === node.id).map((edge) => t(($) => $.relations[edge.type])))).join(" · ")}</p>{card(node, true)}</div>)}</div>}
      <div className="flex flex-wrap gap-3 px-4 pb-3 text-micro text-muted-foreground">{(["duplicate", "shared_pr", "unknown"] as const).filter((type) => graph.edges.some((edge) => edge.type === type)).map((type) => <span key={type}>{t(($) => $.relations[type])}</span>)}</div>
    </TabsContent>
    <TabsContent value="list" data-view="list" className="min-h-0 overflow-auto rounded-xl border border-surface-border p-3"><ul className="space-y-2">{[...parents, ...(current ? [current] : []), ...children, ...grandchildren, ...related].map((node) => <li key={node.id}>{card(node)}</li>)}</ul></TabsContent>
    <p className="text-micro text-muted-foreground">{t(($) => $.relations.navigation_hint)}</p>
  </Tabs>;
}
