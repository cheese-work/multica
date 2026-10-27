"use client";

import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import type { IssueRelations } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { getMermaid, getMermaidLayout, getMermaidThemeVariables } from "../../editor/mermaid-diagram";
import { useThemeVersion } from "../../editor/hooks/use-theme-version";
import { useZoomCanvas } from "../../editor/hooks/use-zoom-canvas";
import { ZoomCanvas, ZoomControls } from "../../editor/zoom-canvas";
import { useT } from "../../i18n";

type EdgeLabels = Record<IssueRelations["edges"][number]["type"], string>;

function labelText(text: string): string {
  return text.replace(/\s/g, " ").replace(/["\\<>#&%[\]{}]/g, (character) => `#${character.codePointAt(0)};`);
}

export function buildRelationsChart(graph: IssueRelations, currentIssueId: string, labels: EdgeLabels): string {
  const nodeIds = new Map(graph.nodes.map((node, index) => [node.id, `n${index}`]));
  const lines = ["flowchart LR"];
  for (const node of graph.nodes) {
    lines.push(`${nodeIds.get(node.id)}["${labelText(`${node.identifier}: ${node.title} (${node.status})`)}"]`);
  }
  const edges = graph.edges.filter((edge) => nodeIds.has(edge.source) && nodeIds.has(edge.target));
  edges.forEach((edge, index) => {
    const arrow = edge.type === "shared_pr" ? "<-->" : "-->";
    lines.push(`${nodeIds.get(edge.source)} ${arrow}|"${labelText(labels[edge.type])}"| ${nodeIds.get(edge.target)}`);
    if (edge.type === "mention") lines.push(`linkStyle ${index} stroke-dasharray:2 4`);
    if (edge.type === "wakeup" || edge.type === "children_done") lines.push(`linkStyle ${index} stroke-dasharray:8 4`);
  });
  const current = nodeIds.get(currentIssueId);
  if (current) lines.push(`classDef current stroke-width:4px`, `class ${current} current`);
  return lines.join("\n");
}

export function IssueRelationsGraph({ graph, currentIssueId, onNavigate }: {
  graph: IssueRelations;
  currentIssueId: string;
  onNavigate: (issueId: string) => void;
}) {
  const { t } = useT("issues");
  const labels: EdgeLabels = {
    parent: t(($) => $.relations.parent), duplicate: t(($) => $.relations.duplicate),
    wakeup: t(($) => $.relations.wakeup), children_done: t(($) => $.relations.children_done),
    mention: t(($) => $.relations.mention), shared_pr: t(($) => $.relations.shared_pr),
    unknown: t(($) => $.relations.unknown),
  };
  const chart = buildRelationsChart(graph, currentIssueId, labels);
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
        mermaid.initialize({ startOnLoad: false, securityLevel: "strict", htmlLabels: false, theme: "base", themeVariables: getMermaidThemeVariables(hostRef.current), suppressErrorRendering: true });
        const { svg } = await mermaid.render(diagramId, chart);
        const size = getMermaidLayout(svg);
        if (!size) throw new Error("missing graph size");
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
      if (node.id === currentIssueId) element.setAttribute("aria-current", "page");
    });
    return { __html: new XMLSerializer().serializeToString(document.documentElement) };
  }, [rendered, issueNode, currentIssueId]);

  return (
    <div ref={hostRef} className="flex min-h-0 flex-1 flex-col">
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
