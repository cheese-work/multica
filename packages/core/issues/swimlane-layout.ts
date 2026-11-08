// Swimlane 2.0 layout (CHE-4312 W1). Design lock: design/timeline-views/approved (Direction A, causal columns).
export interface SwimNode { id: string; /** ISO timestamp used only to break ties and cycles. */ at: string; /** Group-by key (default assignee); null = no value. */ lane: string | null }
export interface SwimEdge { source: string; target: string }
export interface SwimLane { key: string | null; nodeIds: string[] }
export interface SwimLayout { lanes: SwimLane[]; rank: Map<string, number> }

const cmp = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);

/**
 * Lanes by group-by key; x-axis = causal rank (longest path from a start node), never clock time.
 * A cycle is cut where the depth-first walk, started in (at, id) order, meets a node on its own path,
 * so the earliest node starts the chain and the result does not depend on input order.
 */
export function layoutSwimlanes({ nodes, edges }: { nodes: SwimNode[]; edges: SwimEdge[] }): SwimLayout {
  const order = [...nodes].sort((a, b) => cmp(a.at, b.at) || cmp(a.id, b.id));
  const pos = new Map<string, number>();
  order.forEach((node, i) => pos.has(node.id) || pos.set(node.id, i));
  const next: number[][] = order.map(() => []);
  for (const { source, target } of edges) {
    const from = pos.get(source);
    const to = pos.get(target);
    if (from !== undefined && to !== undefined && from !== to) next[from]!.push(to);
  }
  next.forEach((list, i) => (next[i] = [...new Set(list)].sort((a, b) => a - b)));

  // Iterative DFS (a long chain must not hit the call-stack limit); state 1 = on the current path.
  const state = new Uint8Array(order.length);
  const topo: number[] = [];
  const kept: number[][] = order.map(() => []);
  for (let root = 0; root < order.length; root++) {
    if (state[root]) continue;
    const stack: [number, number][] = [[root, 0]];
    state[root] = 1;
    while (stack.length) {
      const top = stack[stack.length - 1]!;
      const [node, at] = top;
      if (at === next[node]!.length) {
        state[node] = 2;
        topo.push(node);
        stack.pop();
        continue;
      }
      top[1]++;
      const to = next[node]![at]!;
      if (state[to] === 1) continue; // back edge: cycle cut
      kept[node]!.push(to);
      if (!state[to]) {
        state[to] = 1;
        stack.push([to, 0]);
      }
    }
  }
  const depth = new Array<number>(order.length).fill(0);
  for (const node of topo.reverse()) for (const to of kept[node]!) depth[to] = Math.max(depth[to]!, depth[node]! + 1);

  const byLane = new Map<string | null, number[]>();
  order.forEach((node, i) => pos.get(node.id) === i && byLane.set(node.lane, [...(byLane.get(node.lane) ?? []), i]));
  const lanes = [...byLane]
    .sort(([a], [b]) => (a === null ? 1 : b === null ? -1 : cmp(a, b)))
    .map(([key, members]) => ({ key, nodeIds: members.sort((a, b) => depth[a]! - depth[b]! || a - b).map((i) => order[i]!.id) }));
  return { lanes, rank: new Map([...pos].map(([id, i]) => [id, depth[i]!])) };
}
