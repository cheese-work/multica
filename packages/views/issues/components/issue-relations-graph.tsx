"use client";

import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import type { IssueRelations } from "@multica/core/types";
import { filterRelations, RELATION_FILTERS, type RelationFilter } from "@multica/core/issues/relations";
import type { IssueStatusCatalog } from "@multica/core/issue-statuses/queries";
import { useRelationsFilterStore } from "@multica/core/issues/stores/relations-filter-store";
import { Button } from "@multica/ui/components/ui/button";
import { getMermaid, getMermaidLayout, getMermaidThemeVariables } from "../../editor/mermaid-diagram";
import { useThemeVersion } from "../../editor/hooks/use-theme-version";
import { useZoomCanvas } from "../../editor/hooks/use-zoom-canvas";
import { ZoomCanvas, ZoomControls } from "../../editor/zoom-canvas";
import { useT } from "../../i18n";

const TITLE_LINE_CHARS = 26;
const TITLE_LINES = 2;
const FILL_WIDTH = 1000;
const FILL_HEIGHT = 560;
const MAX_GROW = 2.5;
type EdgeType = IssueRelations["edges"][number]["type"];
// Dash patterns double as the legend: parent/child solid, wakes dashed, mentions dotted,
// duplicates dash-dot, unknown dash-dot-dot. Shared PR is solid but double-headed.
const EDGE_DASH: Partial<Record<EdgeType, string>> = { wakeup: "8 4", children_done: "8 4", mention: "2 4", duplicate: "10 3 2 3", unknown: "12 4 2 4 2 4" };
const CHIP_EDGE: Record<RelationFilter, EdgeType> = { parent: "parent", wakeup: "wakeup", mention: "mention" };
// Edge types with no chip: always drawn, so the legend lists them when present.
const UNFILTERED_EDGES = ["duplicate", "shared_pr", "unknown"] as const;
// Inline style, not a fill-* class: Mermaid's own `.node rect` rule outranks a utility class.
const STATUS_STRIPE = { unstarted: "var(--muted-foreground)", started: "var(--warning)", done: "var(--info)", closed: "color-mix(in srgb, var(--muted-foreground) 50%, transparent)" } as const;


function labelText(text: string): string {
  return text.replace(/\s/g, " ").replace(/["\\<>#&%[\]{}]/g, (character) => `#${character.codePointAt(0)};`);
}

/** Greedy word wrap into at most TITLE_LINES lines; the last line is cut with an ellipsis. */
function wrapTitle(title: string): string[] {
  const words = title.replace(/\s+/g, " ").trim().split(" ");
  const lines: string[] = [];
  let line = "";
  for (const word of words) {
    if (line && line.length + 1 + word.length > TITLE_LINE_CHARS && lines.length < TITLE_LINES - 1) {
      lines.push(line);
      line = word;
    } else {
      line = line ? `${line} ${word}` : word;
    }
  }
  lines.push(line);
  return lines.map((text) => (text.length > TITLE_LINE_CHARS ? `${text.slice(0, TITLE_LINE_CHARS - 1)}…` : text));
}

export function buildRelationsChart(graph: IssueRelations, currentIssueId: string): string {
  const nodeIds = new Map(graph.nodes.map((node, index) => [node.id, `n${index}`]));
  const lines = ["flowchart LR"];
  for (const node of graph.nodes) {
    lines.push(`${nodeIds.get(node.id)}["${[node.identifier, ...wrapTitle(node.title)].map(labelText).join("<br/>")}"]`);
  }
  const edges = graph.edges.filter((edge) => nodeIds.has(edge.source) && nodeIds.has(edge.target));
  edges.forEach((edge, index) => {
    const arrow = edge.type === "shared_pr" ? "<-->" : "-->";
    lines.push(`${nodeIds.get(edge.source)} ${arrow} ${nodeIds.get(edge.target)}`);
    const dash = EDGE_DASH[edge.type];
    if (dash) lines.push(`linkStyle ${index} stroke-dasharray:${dash}`);
  });
  const current = nodeIds.get(currentIssueId);
  if (current) lines.push(`classDef current stroke-width:4px`, `class ${current} current`);
  return lines.join("\n");
}

function EdgeSample({ type }: { type: EdgeType }) {
  return (
    <svg aria-hidden width="28" height="10" viewBox="0 0 28 10" className="shrink-0" stroke="currentColor" strokeWidth="2">
      <line x1="4" y1="5" x2="24" y2="5" strokeDasharray={EDGE_DASH[type]} />
      {type === "shared_pr" && <path d="M5 1 1 5l4 4M23 1l4 4-4 4" fill="none" strokeWidth="1.5" />}
    </svg>
  );
}

export function IssueRelationsGraph({ graph: fullGraph, currentIssueId, catalog, onNavigate }: {
  graph: IssueRelations;
  currentIssueId: string;
  catalog: Pick<IssueStatusCatalog, "categoryOf" | "colorOf">;
  onNavigate: (issueId: string) => void;
}) {
  const { t } = useT("issues");
  const hidden = useRelationsFilterStore((state) => state.hidden);
  const toggleFilter = useRelationsFilterStore((state) => state.toggle);
  const graph = useMemo(() => filterRelations(fullGraph, currentIssueId, hidden), [fullGraph, currentIssueId, hidden]);
  const filterLabels: Record<RelationFilter, string> = {
    parent: t(($) => $.relations.parent), wakeup: t(($) => $.relations.wakeup), mention: t(($) => $.relations.mention),
  };
  const chart = useMemo(() => buildRelationsChart(graph, currentIssueId), [graph, currentIssueId]);
  const reactId = useId();
  const diagramId = `issue-relations-${reactId.replace(/[^a-zA-Z0-9_-]/g, "")}`;
  const themeVersion = useThemeVersion();
  const hostRef = useRef<HTMLDivElement>(null);
  const [rendered, setRendered] = useState<{ svg: string; size: { width: number; height: number } } | null>(null);
  const [error, setError] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const canvas = useZoomCanvas({ content: rendered?.size ?? null });
  const nodeMap = useMemo(() => new Map(graph.nodes.map((node, index) => [`n${index}`, node])), [graph.nodes]);

  useEffect(() => {
    let cancelled = false;
    setRendered(null);
    setError(false);
    void (async () => {
      try {
        const mermaid = await getMermaid();
        mermaid.initialize({ startOnLoad: false, securityLevel: "strict", htmlLabels: false, theme: "base", themeVariables: getMermaidThemeVariables(hostRef.current), suppressErrorRendering: true, flowchart: { nodeSpacing: 40, rankSpacing: 90, padding: 12, wrappingWidth: 400 } });
        const { svg } = await mermaid.render(diagramId, chart);
        const natural = getMermaidLayout(svg);
        if (!natural) throw new Error("missing graph size");
        // Small graphs are drawn larger than their natural size so the fit-to-view pass
        // (which only ever shrinks) leaves them filling the dialog instead of a thin band.
        const grow = Math.min(MAX_GROW, Math.max(1, Math.min(FILL_WIDTH / natural.width, FILL_HEIGHT / natural.height)));
        const size = { width: Math.round(natural.width * grow), height: Math.round(natural.height * grow) };
        if (!cancelled) setRendered({ svg, size });
      } catch {
        if (!cancelled) setError(true);
      }
    })();
    return () => { cancelled = true; };
  }, [chart, diagramId, themeVersion, attempt]);

  const issueNode = useCallback((element: Element | null) => {
    const node = element?.closest(".node");
    const key = node?.id.startsWith(`${diagramId}-flowchart-`) ? node.id.slice(diagramId.length + 1).match(/^flowchart-(n\d+)-\d+$/)?.[1] : undefined;
    return key ? nodeMap.get(key) : undefined;
  }, [nodeMap, diagramId]);

  const interactiveSvg = useMemo(() => {
    const document = new DOMParser().parseFromString(rendered?.svg ?? "<svg />", "image/svg+xml");
    document.querySelectorAll("text").forEach((element) => {
      const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
      while (walker.nextNode()) {
        walker.currentNode.textContent = walker.currentNode.textContent?.replace(/&#(\d+);/g, (_, code: string) => String.fromCodePoint(Number(code))) ?? "";
      }
    });
    document.querySelectorAll(".node").forEach((element) => {
      const node = issueNode(element);
      if (!node) return;
      element.setAttribute("role", "link");
      element.setAttribute("tabindex", "0");
      element.setAttribute("aria-label", `${node.identifier}: ${node.title} (${node.status})`);
      const tooltip = document.createElementNS("http://www.w3.org/2000/svg", "title");
      tooltip.textContent = `${node.identifier}: ${node.title} (${node.status})`;
      element.prepend(tooltip);
      element.querySelectorAll("text > tspan:first-of-type, text > tspan:first-of-type tspan").forEach((line) => line.setAttribute("style", "font-weight: 700"));
      const box = element.querySelector("rect");
      const [x, y, height] = ["x", "y", "height"].map((name) => Number(box?.getAttribute(name)));
      if (box && [x, y, height].every(Number.isFinite)) {
        const stripe = document.createElementNS("http://www.w3.org/2000/svg", "rect");
        // A custom status keeps its catalog colour; built-ins return null and follow the theme token.
        const fill = catalog.colorOf(node.status) ?? STATUS_STRIPE[catalog.categoryOf(node.status)];
        for (const [name, value] of Object.entries({ x, y, height, width: 6, style: `fill: ${fill}`, "pointer-events": "none" })) stripe.setAttribute(name, String(value));
        element.appendChild(stripe);
      }
      if (node.id === currentIssueId) element.setAttribute("aria-current", "page");
    });
    return { __html: new XMLSerializer().serializeToString(document.documentElement) };
  }, [rendered, issueNode, currentIssueId, catalog]);

  return (
    <div ref={hostRef} className="flex min-h-0 flex-1 flex-col">
      <div role="group" aria-label={t(($) => $.relations.filter_label)} className="flex flex-wrap items-center gap-2 text-caption">
        {RELATION_FILTERS.map((filter) => {
          const visible = !hidden.includes(filter);
          return (
            <button
              key={filter}
              type="button"
              aria-pressed={visible}
              onClick={() => toggleFilter(filter)}
              className={`inline-flex items-center gap-2 rounded-full border px-3 py-1 outline-none focus-visible:ring-2 focus-visible:ring-ring ${visible ? "border-foreground/40 bg-accent text-foreground" : "border-border text-muted-foreground line-through"}`}
            >
              <EdgeSample type={CHIP_EDGE[filter]} />
              {filterLabels[filter]}
            </button>
          );
        })}
        {UNFILTERED_EDGES.filter((type) => graph.edges.some((edge) => edge.type === type)).map((type) => (
          <span key={type} className="inline-flex items-center gap-2 px-1 text-muted-foreground"><EdgeSample type={type} />{t(($) => $.relations[type])}</span>
        ))}
      </div>
      {error ? <div role="alert"><p>{t(($) => $.relations.render_failed)}</p><Button variant="outline" onClick={() => setAttempt((value) => value + 1)}>{t(($) => $.relations.retry)}</Button></div> : !rendered ? <p role="status">{t(($) => $.relations.loading)}</p> : (
        <>
          <ZoomControls canvas={canvas} />
          <ZoomCanvas canvas={canvas} content={rendered.size} label={t(($) => $.relations.title)} className="min-h-64 flex-1">
            <div
              className="h-full w-full [&_svg]:h-full [&_svg]:w-full [&_svg]:max-w-none [&_.node]:cursor-pointer [&_.node:focus-visible]:outline-2 [&_.node:focus-visible]:outline-ring"
              onPointerDown={(event) => { if (event.target instanceof Element && issueNode(event.target)) event.stopPropagation(); }}
              onClick={(event) => { if (event.target instanceof Element) { const node = issueNode(event.target); if (node) onNavigate(node.id); } }}
              onKeyDown={(event) => {
                if ((event.key === "Enter" || event.key === " ") && event.target instanceof Element) {
                  const node = issueNode(event.target);
                  if (node) { event.preventDefault(); event.stopPropagation(); onNavigate(node.id); }
                }
              }}
              dangerouslySetInnerHTML={interactiveSvg}
            />
          </ZoomCanvas>
        </>
      )}
    </div>
  );
}
