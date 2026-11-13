"use client";

// Swimlane 2.0 renderer (CHE-4312 W2). Design lock: design/timeline-views/approved (Direction A, causal columns).
// Presentational: lane and rank come from layoutSwimlanes (W1), endpoint states from buildGraph (K1).
// Not registered in the view-mode popup yet (W3).
import { useId, useMemo } from "react";
import { useWorkspaceId } from "@multica/core/hooks";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { buildGraph, type GraphEdgeType, type TimelineEdge, type TimelineNode } from "@multica/core/issues/graph-core";
import { layoutSwimlanes } from "@multica/core/issues/swimlane-layout";
import { useT } from "../../../i18n";
import { useStatusLabel } from "../../utils/status-label";
import { StatusIcon } from "../status-icon";

export interface Swimlane2Node extends TimelineNode {
  /** ISO timestamp; breaks ties and cycles in the causal order, never the x position. */
  at: string;
  /** Group-by key (default assignee); null = no value. */
  lane: string | null;
}
export interface Swimlane2GraphProps {
  nodes: Swimlane2Node[];
  edges: TimelineEdge[];
  /** Endpoints whose edge page failed or is still loading; edges to them are not drawn. */
  notLoaded?: ReadonlySet<string>;
  /** More timeline pages exist than are loaded. */
  hasMoreIssues?: boolean;
  onRetry?: () => void;
  onLoadMore?: () => void;
  laneLabel?: (key: string | null) => string;
}

const LANE_W = 160;
const COL_W = 210;
const NODE_W = 170;
const NODE_H = 40;
const ROW_H = 52;
const PAD = 12;

/** One style per drawn edge type: line style plus a label, never colour alone (design lock). */
const EDGE_STYLE = {
  wakeup: { color: "text-foreground", dash: undefined, arrow: true, width: 1.6 },
  dependency: { color: "text-info", dash: "6 4", arrow: true, width: 1.6 },
  parent: { color: "text-muted-foreground", dash: undefined, arrow: false, width: 1.2 },
  mention: { color: "text-success", dash: "1 4", arrow: true, width: 2 },
} as const satisfies Partial<Record<GraphEdgeType, { color: string; dash: string | undefined; arrow: boolean; width: number }>>;
type DrawnType = keyof typeof EDGE_STYLE;
const DRAWN = Object.keys(EDGE_STYLE) as DrawnType[];
const isDrawn = (type: GraphEdgeType): type is DrawnType => type in EDGE_STYLE;

export function Swimlane2Graph({ nodes, edges, notLoaded, hasMoreIssues, onRetry, onLoadMore, laneLabel }: Swimlane2GraphProps) {
  const { t } = useT("issues");
  const wsId = useWorkspaceId();
  const statusLabel = useStatusLabel(wsId);
  const { categoryOf, colorOf, iconOf } = useIssueStatuses(wsId);
  const uid = useId().replace(/:/g, "");

  const { placed, bands, drawn, width, height, byId } = useMemo(() => {
    const laid = nodes.filter((n) => !n.context);
    const view = buildGraph({ nodes, edges, notLoaded });
    const drawn = view.edges.filter((e) => isDrawn(e.type) && e.sourceState === "in_response" && e.targetState === "in_response");
    const { lanes, rank } = layoutSwimlanes({ nodes: laid, edges: drawn });
    const placed = new Map<string, { x: number; y: number }>();
    const bands: { key: string | null; y: number; h: number }[] = [];
    let y = 0;
    let maxRank = 0;
    for (const lane of lanes) {
      const stack = new Map<number, number>();
      for (const id of lane.nodeIds) {
        const r = rank.get(id)!;
        const slot = stack.get(r) ?? 0;
        stack.set(r, slot + 1);
        maxRank = Math.max(maxRank, r);
        placed.set(id, { x: LANE_W + r * COL_W, y: y + PAD + slot * ROW_H });
      }
      const h = Math.max(1, ...stack.values()) * ROW_H + PAD;
      bands.push({ key: lane.key, y, h });
      y += h;
    }
    return { placed, bands, drawn, width: LANE_W + (maxRank + 1) * COL_W, height: y, byId: new Map(nodes.map((n) => [n.id, n])) };
  }, [nodes, edges, notLoaded]);

  const edgeLabel: Record<DrawnType, string> = {
    wakeup: t(($) => $.swimlane2.edge_wakeup),
    dependency: t(($) => $.swimlane2.edge_dependency),
    parent: t(($) => $.swimlane2.edge_parent),
    mention: t(($) => $.swimlane2.edge_mention),
  };
  const counts = DRAWN.map((type) => [type, drawn.filter((e) => e.type === type).length] as const).filter(([, n]) => n > 0);
  const incomplete = !!notLoaded?.size;

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-2">
      {incomplete && (
        <Banner text={t(($) => $.swimlane2.relations_incomplete)} action={t(($) => $.swimlane2.retry)} onAction={onRetry} />
      )}
      {hasMoreIssues && (
        <Banner text={t(($) => $.swimlane2.more_issues)} action={t(($) => $.swimlane2.load_more)} onAction={onLoadMore} />
      )}
      <ul aria-label={t(($) => $.swimlane2.edge_types)} className="flex flex-wrap gap-2 text-caption">
        {counts.map(([type, n]) => (
          <li key={type} className={`flex items-center gap-1.5 rounded-full border px-2 py-0.5 ${EDGE_STYLE[type].color}`}>
            <svg aria-hidden="true" width="26" height="8">
              <path d="M1 4H25" stroke="currentColor" strokeWidth={EDGE_STYLE[type].width} strokeDasharray={EDGE_STYLE[type].dash} strokeLinecap="round" />
            </svg>
            <span className="font-semibold text-foreground">{edgeLabel[type]}</span>
            <span className="text-muted-foreground">{n}</span>
          </li>
        ))}
      </ul>
      <div className="min-h-0 flex-1 overflow-auto">
        <div className="relative" style={{ width, height }}>
          {bands.map((band) => (
            <div key={band.key ?? "none"} className="absolute left-0 border-b" style={{ top: band.y, height: band.h, width }}>
              <span className="absolute left-2 top-3 max-w-[140px] truncate text-caption font-semibold">
                {laneLabel ? laneLabel(band.key) : (band.key ?? t(($) => $.swimlane2.no_lane))}
              </span>
            </div>
          ))}
          <svg aria-hidden="false" className="pointer-events-none absolute left-0 top-0" width={width} height={height}>
            <defs>
              {DRAWN.filter((type) => EDGE_STYLE[type].arrow).map((type) => (
                <marker key={type} id={`${uid}-${type}`} viewBox="0 0 8 8" refX="7" refY="4" markerWidth="8" markerHeight="8" orient="auto" className={EDGE_STYLE[type].color}>
                  <path d="M0 0L8 4L0 8z" fill="currentColor" />
                </marker>
              ))}
            </defs>
            {drawn.map((e) => {
              const from = placed.get(e.source);
              const to = placed.get(e.target);
              if (!from || !to) return null;
              const style = EDGE_STYLE[e.type as DrawnType];
              const x1 = from.x + NODE_W;
              const y1 = from.y + NODE_H / 2;
              const x2 = to.x;
              const y2 = to.y + NODE_H / 2;
              const label = `${edgeLabel[e.type as DrawnType]}: ${byId.get(e.source)!.identifier} → ${byId.get(e.target)!.identifier}`;
              return (
                <g key={`${e.type}|${e.source}|${e.target}`} data-edge-type={e.type} role="img" aria-label={label} className={style.color}>
                  <title>{label}</title>
                  <path
                    d={`M${x1} ${y1}C${x1 + 20} ${y1} ${x2 - 20} ${y2} ${x2} ${y2}`}
                    fill="none"
                    stroke="currentColor"
                    strokeWidth={style.width}
                    strokeDasharray={style.dash}
                    strokeLinecap="round"
                    markerEnd={style.arrow ? `url(#${uid}-${e.type})` : undefined}
                  />
                  {e.type === "parent" && <circle cx={x1} cy={y1} r="3" className="fill-background" stroke="currentColor" />}
                  {e.type === "mention" && (
                    <g>
                      <circle cx={(x1 + x2) / 2} cy={(y1 + y2) / 2} r="9" className="fill-background" stroke="currentColor" />
                      <text x={(x1 + x2) / 2} y={(y1 + y2) / 2 + 4} textAnchor="middle" fontSize="11" fontWeight="700" fill="currentColor">@</text>
                    </g>
                  )}
                </g>
              );
            })}
          </svg>
          {[...placed].map(([id, pos]) => {
            const n = byId.get(id)!;
            const label = statusLabel(n.status);
            return (
              <div
                key={id}
                role="group"
                aria-label={`${n.identifier} ${label}: ${n.title}`}
                title={n.at}
                className="absolute flex flex-col justify-center rounded-lg border bg-card px-3 text-caption"
                style={{ left: pos.x, top: pos.y, width: NODE_W, height: NODE_H }}
              >
                <span className="flex items-center gap-1.5">
                  <StatusIcon status={n.status} category={categoryOf(n.status)} color={colorOf(n.status)} icon={iconOf(n.status)} className="h-3 w-3 shrink-0" />
                  <span className="font-mono text-muted-foreground">{n.identifier}</span>
                  <span className="ml-auto text-muted-foreground">{label}</span>
                </span>
                <span className="truncate">{n.title}</span>
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}

function Banner({ text, action, onAction }: { text: string; action: string; onAction?: () => void }) {
  return (
    <div role="alert" className="flex items-center gap-2 rounded-md border border-warning/40 bg-warning/10 px-3 py-1.5 text-caption">
      <span>{text}</span>
      <button type="button" onClick={onAction} className="ml-auto font-semibold underline">{action}</button>
    </div>
  );
}
