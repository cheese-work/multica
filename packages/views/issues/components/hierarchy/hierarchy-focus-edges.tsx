import type { FocusEdge } from "./hierarchy-focus-layout";

const DASH = "6 4";

/** Edges into leaf cards are dashed; edges into cards with children are solid. */
export function FocusEdges({ edges }: { edges: FocusEdge[] }) {
  return (
    <svg aria-hidden="true" className="pointer-events-none absolute left-0 top-0 overflow-visible text-border" width="1" height="1">
      {edges.map((edge, index) => <polyline key={index} points={edge.points.map(([x, y]) => `${x},${y}`).join(" ")} fill="none" stroke="currentColor" strokeWidth="2" strokeDasharray={edge.dashed ? DASH : undefined} />)}
    </svg>
  );
}
