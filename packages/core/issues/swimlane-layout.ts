// Swimlane 2.0 layout (CHE-4312 W1). Design lock: design/timeline-views/approved (Direction A, causal columns).
export interface SwimNode { id: string; /** ISO timestamp used only to break ties and cycles. */ at: string; /** Group-by key (default assignee); null = no value. */ lane: string | null }
export interface SwimEdge { source: string; target: string }
export interface SwimLane { key: string | null; nodeIds: string[] }
export interface SwimLayout { lanes: SwimLane[]; rank: Map<string, number> }

const cmp = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0);

/**
 * Lanes by group-by key; x-axis = causal rank (longest path from a start node), never clock time.
 * Each cycle (strongly connected component) is cut by a depth-first walk that starts at its earliest
 * (at, id) node and drops the edge that closes back onto the walk, so the earliest node starts the chain
 * whichever member the cycle is entered through, and the result does not depend on input order.
 */
export function layoutSwimlanes({ nodes, edges }: { nodes: SwimNode[]; edges: SwimEdge[] }): SwimLayout {
  const order = [...nodes].sort((a, b) => cmp(a.at, b.at) || cmp(a.id, b.id));
  const pos = new Map<string, number>();
  order.forEach((node, i) => pos.has(node.id) || pos.set(node.id, i));
  const count = order.length;
  const next: number[][] = order.map(() => []);
  for (const { source, target } of edges) {
    const from = pos.get(source);
    const to = pos.get(target);
    if (from !== undefined && to !== undefined && from !== to) next[from]!.push(to);
  }
  next.forEach((list, i) => (next[i] = [...new Set(list)].sort((a, b) => a - b)));

  // Iterative Tarjan (a long chain must not hit the call-stack limit): comp[i] = strongly connected component.
  const comp = new Int32Array(count).fill(-1);
  const index = new Int32Array(count).fill(-1);
  const low = new Int32Array(count);
  const open: number[] = [];
  let clock = 0;
  let comps = 0;
  for (let root = 0; root < count; root++) {
    if (index[root] !== -1) continue;
    const calls: [number, number][] = [[root, 0]];
    index[root] = low[root] = clock++;
    open.push(root);
    while (calls.length) {
      const top = calls[calls.length - 1]!;
      const [node, at] = top;
      if (at < next[node]!.length) {
        top[1]++;
        const to = next[node]![at]!;
        if (index[to] === -1) {
          index[to] = low[to] = clock++;
          open.push(to);
          calls.push([to, 0]);
        } else if (comp[to] === -1) low[node] = Math.min(low[node]!, index[to]!);
        continue;
      }
      calls.pop();
      if (calls.length) low[calls[calls.length - 1]![0]] = Math.min(low[calls[calls.length - 1]![0]]!, low[node]!);
      if (low[node] !== index[node]) continue;
      for (let member = -1; member !== node; ) comp[(member = open.pop()!)] = comps;
      comps++;
    }
  }

  // Keep every edge between components. Inside a component, walk from its earliest node (state 1 = on the
  // current path) and drop back edges. Roots run in (at, id) order, so the first unvisited node is the earliest.
  const state = new Uint8Array(count);
  const kept: number[][] = order.map((_, i) => next[i]!.filter((to) => comp[to] !== comp[i]));
  for (let root = 0; root < count; root++) {
    if (state[root]) continue;
    const stack: [number, number][] = [[root, 0]];
    state[root] = 1;
    while (stack.length) {
      const top = stack[stack.length - 1]!;
      const [node, at] = top;
      if (at === next[node]!.length) {
        state[node] = 2;
        stack.pop();
        continue;
      }
      top[1]++;
      const to = next[node]![at]!;
      if (comp[to] !== comp[node] || state[to] === 1) continue;
      kept[node]!.push(to);
      if (!state[to]) {
        state[to] = 1;
        stack.push([to, 0]);
      }
    }
  }

  // Longest path over the now-acyclic graph (Kahn).
  const depth = new Array<number>(count).fill(0);
  const waiting = new Int32Array(count);
  kept.forEach((list) => list.forEach((to) => waiting[to]!++));
  const ready = [...Array(count).keys()].filter((i) => !waiting[i]);
  for (let head = 0; head < ready.length; head++) {
    const node = ready[head]!;
    for (const to of kept[node]!) {
      depth[to] = Math.max(depth[to]!, depth[node]! + 1);
      if (!--waiting[to]!) ready.push(to);
    }
  }

  const byLane = new Map<string | null, number[]>();
  order.forEach((node, i) => {
    if (pos.get(node.id) !== i) return;
    const members = byLane.get(node.lane);
    if (members) members.push(i);
    else byLane.set(node.lane, [i]);
  });
  const lanes = [...byLane]
    .sort(([a], [b]) => (a === null ? 1 : b === null ? -1 : cmp(a, b)))
    .map(([key, members]) => ({ key, nodeIds: members.sort((a, b) => depth[a]! - depth[b]! || a - b).map((i) => order[i]!.id) }));
  return { lanes, rank: new Map([...pos].map(([id, i]) => [id, depth[i]!])) };
}
