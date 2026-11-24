/**
 * By-id entity table, the normalization shared by every Stage B store
 * (chat messages now, issue comments in B1b). Lists elsewhere hold ordered IDs
 * and resolve rows from here. Every function is pure and reference-stable:
 * when nothing changed it returns the input, so observers do not re-render.
 */

export interface EntityTable<T extends { id: string }> {
  byId: Record<string, T>;
}

export const EMPTY_ENTITY_TABLE: EntityTable<never> = { byId: {} };

/** Incoming fields fill only the gaps in `existing`; a field `existing` defines wins. */
export function mergeFillGaps<T extends object>(existing: T, incoming: T): T {
  let next: T | null = null;
  for (const key of Object.keys(incoming) as (keyof T)[]) {
    if (existing[key] !== undefined || incoming[key] === undefined) continue;
    next ??= { ...existing };
    next[key] = incoming[key];
  }
  return next ?? existing;
}

/** Incoming fields win; fields only `existing` defines survive. For authoritative server snapshots. */
export function mergeSnapshot<T extends object>(existing: T, incoming: T): T {
  let changed = false;
  for (const key of Object.keys(incoming) as (keyof T)[]) {
    if (incoming[key] !== undefined && existing[key] !== incoming[key]) {
      changed = true;
      break;
    }
  }
  if (!changed) return existing;
  const next = { ...existing };
  for (const key of Object.keys(incoming) as (keyof T)[]) {
    if (incoming[key] !== undefined) next[key] = incoming[key];
  }
  return next;
}

/** Upsert rows by id with `merge` deciding the winner of a field clash. */
export function upsertEntities<T extends { id: string }>(
  table: EntityTable<T> | undefined,
  rows: readonly T[],
  merge: (existing: T, incoming: T) => T,
): EntityTable<T> {
  const base = table ?? { byId: {} };
  let byId: Record<string, T> | null = null;
  for (const row of rows) {
    const current = (byId ?? base.byId)[row.id];
    const next = current ? merge(current, row) : row;
    if (next === current) continue;
    byId ??= { ...base.byId };
    byId[row.id] = next;
  }
  if (byId) return { byId };
  return table ?? base;
}

export function removeEntity<T extends { id: string }>(
  table: EntityTable<T> | undefined,
  id: string,
): EntityTable<T> | undefined {
  if (!table || !(id in table.byId)) return table;
  const { [id]: _dropped, ...byId } = table.byId;
  return { byId };
}

/** Resolve ordered ids to rows. An id with no entry is skipped; a repeated id appears once. */
export function resolveEntities<T extends { id: string }>(
  table: EntityTable<T> | undefined,
  ids: readonly string[],
): T[] {
  const seen = new Set<string>();
  const rows: T[] = [];
  for (const id of ids) {
    const row = table?.byId[id];
    if (!row || seen.has(id)) continue;
    seen.add(id);
    rows.push(row);
  }
  return rows;
}
