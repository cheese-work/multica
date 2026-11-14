// Pure graph core shared by Swimlane 2.0 and the hierarchy zoom (CHE-4312 K1).
// No React, no I/O: tree model, collapse, and edge retarget to the nearest visible ancestor.

export type EndpointState = "in_response" | "context" | "not_loaded";
export type GraphEdgeType = "parent" | "duplicate" | "wakeup" | "children_done" | "mention" | "shared_pr" | "dependency" | "unknown";

export interface TimelineNode {
  id: string;
  identifier: string;
  title: string;
  status: string;
  parentId: string | null;
  /** In the response only as tree or edge context (outside scope or window). */
  context?: boolean;
}
export interface TimelineEdge { type: GraphEdgeType; source: string; target: string }
export interface ViewEdge extends TimelineEdge {
  sourceState: EndpointState;
  targetState: EndpointState;
  /** The original endpoint was collapse-hidden; the edge now ends at its visible ancestor ("target inside"). */
  sourceInside: boolean;
  targetInside: boolean;
}
export interface GraphView {
  roots: string[];
  /** Pre-order, input order among siblings, collapse applied. */
  visible: string[];
  hidden: Set<string>;
  edges: ViewEdge[];
  isLeaf(id: string): boolean;
}
interface GraphInput {
  nodes: TimelineNode[];
  edges: TimelineEdge[];
  collapsed?: ReadonlySet<string>;
  /** Endpoints whose edge page or level failed or is pending; never retargeted. */
  notLoaded?: ReadonlySet<string>;
}

/** Parent links into the node set; a parent cycle is cut at its smallest id. */
function parentLinks(nodes: TimelineNode[]): Map<string, string> {
  const ids = new Set(nodes.map((n) => n.id));
  const parentOf = new Map<string, string>();
  for (const n of nodes) if (n.parentId && n.parentId !== n.id && ids.has(n.parentId)) parentOf.set(n.id, n.parentId);
  const done = new Set<string>();
  for (const start of [...ids].sort()) {
    const path: string[] = [];
    for (let id: string | undefined = start; id && !done.has(id); id = parentOf.get(id)) {
      const at = path.indexOf(id);
      if (at >= 0) {
        parentOf.delete(path.slice(at).sort()[0]!);
        break;
      }
      path.push(id);
    }
    path.forEach((id) => done.add(id));
  }
  return parentOf;
}

export function buildGraph({ nodes, edges, collapsed = new Set(), notLoaded = new Set() }: GraphInput): GraphView {
  const parentOf = parentLinks(nodes);
  const children = new Map<string, string[]>();
  const roots: string[] = [];
  for (const { id } of nodes) {
    const parent = parentOf.get(id);
    if (parent) children.set(parent, [...(children.get(parent) ?? []), id]);
    else roots.push(id);
  }

  const visible: string[] = [];
  const walk = (id: string) => {
    visible.push(id);
    if (!collapsed.has(id)) (children.get(id) ?? []).forEach(walk);
  };
  roots.forEach(walk);
  const shown = new Set(visible);
  const hidden = new Set(nodes.map((n) => n.id).filter((id) => !shown.has(id)));

  const byId = new Map(nodes.map((n) => [n.id, n]));
  const stateOf = (id: string): EndpointState => {
    const node = byId.get(id);
    return !node || notLoaded.has(id) ? "not_loaded" : node.context ? "context" : "in_response";
  };
  const visibleAncestor = (id: string): string => {
    let at = id;
    while (!shown.has(at)) at = parentOf.get(at)!; // a hidden node always has a parent
    return at;
  };

  const merged = new Map<string, ViewEdge>();
  for (const e of edges) {
    const sourceState = stateOf(e.source);
    const targetState = stateOf(e.target);
    const sourceInside = sourceState === "in_response" && hidden.has(e.source);
    const targetInside = targetState === "in_response" && hidden.has(e.target);
    const source = sourceInside ? visibleAncestor(e.source) : e.source;
    const target = targetInside ? visibleAncestor(e.target) : e.target;
    if (source === target) continue; // inside one collapsed group
    const key = `${e.type}\0${source}\0${target}`;
    const prior = merged.get(key);
    // Merged edges keep an inside flag from any of them, so input order never changes the indicator.
    merged.set(key, prior
      ? { ...prior, sourceInside: prior.sourceInside || sourceInside, targetInside: prior.targetInside || targetInside }
      : { type: e.type, source, target, sourceState, targetState, sourceInside, targetInside });
  }
  const viewEdges = [...merged.values()];

  return { roots, visible, hidden, edges: viewEdges, isLeaf: (id) => !children.has(id) };
}
