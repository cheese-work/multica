import { create } from "zustand";
import { createJSONStorage, persist } from "zustand/middleware";
import { defaultStorage } from "../platform/storage";
import {
  EMPTY_INBOX_FILTERS,
  INBOX_DIMENSION_FIELD,
  INBOX_ISSUE_FIELD_DIMENSIONS,
  inboxFilterValues,
  isEmptyInboxFilters,
  sanitizeInboxFilters,
  toggleInboxFilterValue,
  withoutInboxFilter,
  type InboxAgentFilter,
  type InboxFilterKey,
  type InboxFilters,
  type InboxListDimension,
  type InboxReceivedFilter,
} from "./filters";
import type { IssuePriority, IssueStatus } from "../types";

<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
export interface InboxFilters {
  readonly statuses: readonly IssueStatus[];
  readonly priorities: readonly IssuePriority[];
  /** Actor keys — see `inboxActorKey`. */
  readonly actors: readonly string[];
  readonly unreadOnly: boolean;
  /** Only issues no agent is running or queued on. Active inbox only. */
  readonly idleOnly: boolean;
  /** Only rows where the viewer was mentioned, by name or by `@all`. Active inbox only. */
  readonly mentionsOnly: boolean;
}

/**
 * The dimensions the server-filtered archive understands. It has no idle or
 * mention dimension, so those never reach its endpoints or query keys.
 */
export type ArchivedInboxFilters = Omit<InboxFilters, "idleOnly" | "mentionsOnly">;

/**
 * Stable identity for "who this notification came from", used as the value of
 * the `actors` dimension.
 *
 * Members and agents key on their id. `system` has no id — the backend writes
 * an invalid UUID that serializes to null — so it keys on the type alone and
 * every system notification collapses into one bucket. Returns null when the
 * row carries no usable attribution; such a row can never match an actor
 * selection, exactly as a row without an issue can never match a status one.
 */
export function inboxActorKey(item: InboxItem): string | null {
  const type = item.actor_type;
  if (!type) return null;
  if (type === "system") return "system";
  return item.actor_id ? `${type}:${item.actor_id}` : null;
}

/**
 * True when the viewer was named directly or by `@all`. The server writes
 * `mentioned` for `@squad` too and records which source it was in
 * `details.mention_source`. A row from before that was recorded has no source,
 * so its origin is unknown and it does not match.
 */
export function isViewerMention(item: InboxItem): boolean {
  const source = item.details?.mention_source;
  return source === "direct" || source === "all";
}

/** Inverse of `inboxActorKey`, for resolving a selection back to a directory. */
export function inboxActorKeyParts(key: string): { type: string; id: string } {
  const separator = key.indexOf(":");
  if (separator === -1) return { type: key, id: "" };
  return { type: key.slice(0, separator), id: key.slice(separator + 1) };
}

export type InboxPriorityFilterSupport =
  | "unknown"
  | "supported"
  | "unsupported";

export const EMPTY_INBOX_FILTERS: InboxFilters = Object.freeze({
  statuses: Object.freeze([]),
  priorities: Object.freeze([]),
  actors: Object.freeze([]),
  unreadOnly: false,
  idleOnly: false,
  mentionsOnly: false,
});

/** True when nothing is selected in any dimension. */
export function isEmptyInboxFilters(filters: InboxFilters): boolean {
  return inboxFilterCount(filters) === 0;
}
||||||| 2ae2f08005
export interface InboxFilters {
  readonly statuses: readonly IssueStatus[];
  readonly priorities: readonly IssuePriority[];
  /** Actor keys — see `inboxActorKey`. */
  readonly actors: readonly string[];
  readonly unreadOnly: boolean;
  /** Only issues no agent is running or queued on. Active inbox only. */
  readonly idleOnly: boolean;
}

/**
 * The dimensions the server-filtered archive understands. It has no idle
 * dimension, so `idleOnly` never reaches its endpoints or query keys.
 */
export type ArchivedInboxFilters = Omit<InboxFilters, "idleOnly">;

/**
 * Stable identity for "who this notification came from", used as the value of
 * the `actors` dimension.
 *
 * Members and agents key on their id. `system` has no id — the backend writes
 * an invalid UUID that serializes to null — so it keys on the type alone and
 * every system notification collapses into one bucket. Returns null when the
 * row carries no usable attribution; such a row can never match an actor
 * selection, exactly as a row without an issue can never match a status one.
 */
export function inboxActorKey(item: InboxItem): string | null {
  const type = item.actor_type;
  if (!type) return null;
  if (type === "system") return "system";
  return item.actor_id ? `${type}:${item.actor_id}` : null;
}

/** Inverse of `inboxActorKey`, for resolving a selection back to a directory. */
export function inboxActorKeyParts(key: string): { type: string; id: string } {
  const separator = key.indexOf(":");
  if (separator === -1) return { type: key, id: "" };
  return { type: key.slice(0, separator), id: key.slice(separator + 1) };
}

export type InboxPriorityFilterSupport =
  | "unknown"
  | "supported"
  | "unsupported";

export const EMPTY_INBOX_FILTERS: InboxFilters = Object.freeze({
  statuses: Object.freeze([]),
  priorities: Object.freeze([]),
  actors: Object.freeze([]),
  unreadOnly: false,
  idleOnly: false,
});

/** True when nothing is selected in any dimension. */
export function isEmptyInboxFilters(filters: InboxFilters): boolean {
  return inboxFilterCount(filters) === 0;
}
=======
export * from "./filters";
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d

interface InboxFilterState {
  filtersByWorkspace: Record<string, InboxFilters>;
  toggleFilterValue: (wsId: string, dimension: InboxListDimension, value: string) => void;
  setFilterNegated: (wsId: string, dimension: InboxListDimension, negated: boolean) => void;
  toggleStatusFilter: (wsId: string, status: IssueStatus) => void;
  togglePriorityFilter: (wsId: string, priority: IssuePriority) => void;
  toggleActorFilter: (wsId: string, actor: string) => void;
  toggleUnreadOnly: (wsId: string) => void;
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
  toggleIdleOnly: (wsId: string) => void;
  toggleMentionsOnly: (wsId: string) => void;
||||||| 2ae2f08005
  toggleIdleOnly: (wsId: string) => void;
=======
  /** Selecting the current value again clears the dimension. */
  setReceived: (wsId: string, received: InboxReceivedFilter) => void;
  setAgent: (wsId: string, agent: InboxAgentFilter) => void;
  clearFilter: (wsId: string, key: InboxFilterKey) => void;
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
  clearPriorityFilters: (wsId: string) => void;
  clearIssueFieldFilters: (wsId: string) => void;
  clearFilters: (wsId: string) => void;
}

function update(
  state: InboxFilterState,
  wsId: string,
  change: (current: InboxFilters) => InboxFilters,
): Partial<InboxFilterState> | InboxFilterState {
  const current = state.filtersByWorkspace[wsId] ?? EMPTY_INBOX_FILTERS;
  const next = change(current);
  if (next === current) return state;
  // Emptiness is asked of every dimension: an entry is dropped only when
  // nothing in it narrows the list any more. An "is not" picked before its
  // first value narrows nothing yet but must survive until that value comes.
  if (isEmptyInboxFilters(next) && next.negated.length === 0) {
    if (!state.filtersByWorkspace[wsId]) return state;
    const { [wsId]: _removed, ...filtersByWorkspace } = state.filtersByWorkspace;
    return { filtersByWorkspace };
  }
  return { filtersByWorkspace: { ...state.filtersByWorkspace, [wsId]: next } };
}

function toggleListValue(current: InboxFilters, dimension: InboxListDimension, value: string): InboxFilters {
  const field = INBOX_DIMENSION_FIELD[dimension];
  // "Is not" stays when the last value goes, as in the issue filter: the
  // operator is the submenu's own setting, and clearing the field resets it.
  return { ...current, [field]: toggleInboxFilterValue(current[field] as readonly string[], value) };
}

export const useInboxFilterStore = create<InboxFilterState>()(
  persist(
    (set) => ({
      filtersByWorkspace: {},
      toggleFilterValue: (wsId, dimension, value) =>
        set((state) => update(state, wsId, (current) => toggleListValue(current, dimension, value))),
      setFilterNegated: (wsId, dimension, negated) =>
        set((state) =>
          update(state, wsId, (current) => {
            if (current.negated.includes(dimension) === negated) return current;
            return {
              ...current,
              negated: negated
                ? [...current.negated, dimension]
                : current.negated.filter((candidate) => candidate !== dimension),
            };
          }),
        ),
      toggleStatusFilter: (wsId, status) =>
        set((state) => update(state, wsId, (current) => toggleListValue(current, "status", status))),
      togglePriorityFilter: (wsId, priority) =>
        set((state) => update(state, wsId, (current) => toggleListValue(current, "priority", priority))),
      toggleActorFilter: (wsId, actor) =>
        set((state) => update(state, wsId, (current) => toggleListValue(current, "actor", actor))),
      toggleUnreadOnly: (wsId) =>
        set((state) => update(state, wsId, (current) => ({ ...current, unreadOnly: !current.unreadOnly }))),
      setReceived: (wsId, received) =>
        set((state) =>
          update(state, wsId, (current) => ({
            ...current,
            received: current.received === received ? null : received,
          })),
        ),
      setAgent: (wsId, agent) =>
        set((state) =>
          update(state, wsId, (current) => ({
            ...current,
            agent: current.agent === agent ? null : agent,
          })),
        ),
      clearFilter: (wsId, key) =>
        set((state) => update(state, wsId, (current) => withoutInboxFilter(current, key))),
      clearPriorityFilters: (wsId) =>
        set((state) =>
          update(state, wsId, (current) =>
            current.priorities.length === 0 ? current : withoutInboxFilter(current, "priority"),
          ),
        ),
      clearIssueFieldFilters: (wsId) =>
        set((state) =>
          update(state, wsId, (current) =>
            INBOX_ISSUE_FIELD_DIMENSIONS.reduce(
              (next, dimension) =>
                inboxFilterValues(next, dimension).length === 0 ? next : withoutInboxFilter(next, dimension),
              current,
            ),
          ),
        ),
      clearFilters: (wsId) =>
        set((state) => {
          if (!state.filtersByWorkspace[wsId]) return state;
          const { [wsId]: _removed, ...filtersByWorkspace } = state.filtersByWorkspace;
          return { filtersByWorkspace };
        }),
    }),
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
  toggleActorFilter: (wsId, actor) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId] ?? EMPTY_INBOX_FILTERS;
      return {
        filtersByWorkspace: {
          ...state.filtersByWorkspace,
          [wsId]: { ...current, actors: toggleValue(current.actors, actor) },
        },
      };
    }),
  toggleUnreadOnly: (wsId) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId] ?? EMPTY_INBOX_FILTERS;
      return {
        filtersByWorkspace: {
          ...state.filtersByWorkspace,
          [wsId]: { ...current, unreadOnly: !current.unreadOnly },
        },
      };
    }),
  toggleIdleOnly: (wsId) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId] ?? EMPTY_INBOX_FILTERS;
      return {
        filtersByWorkspace: {
          ...state.filtersByWorkspace,
          [wsId]: { ...current, idleOnly: !current.idleOnly },
        },
      };
    }),
  toggleMentionsOnly: (wsId) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId] ?? EMPTY_INBOX_FILTERS;
      return {
        filtersByWorkspace: {
          ...state.filtersByWorkspace,
          [wsId]: { ...current, mentionsOnly: !current.mentionsOnly },
        },
      };
    }),
  clearPriorityFilters: (wsId) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId];
      if (!current || current.priorities.length === 0) return state;
      const next = { ...current, priorities: [] };
      // Emptiness is asked of every dimension, not just statuses: dropping the
      // entry while an actor or unread selection survived in it would clear a
      // filter the user set and the backend never had a say in.
      if (isEmptyInboxFilters(next)) {
        const { [wsId]: _removed, ...filtersByWorkspace } =
          state.filtersByWorkspace;
        return { filtersByWorkspace };
      }
      return {
        filtersByWorkspace: { ...state.filtersByWorkspace, [wsId]: next },
      };
    }),
  clearFilters: (wsId) =>
    set((state) => {
      if (!state.filtersByWorkspace[wsId]) return state;
      const { [wsId]: _removed, ...filtersByWorkspace } =
        state.filtersByWorkspace;
      return { filtersByWorkspace };
    }),
}));
||||||| 2ae2f08005
  toggleActorFilter: (wsId, actor) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId] ?? EMPTY_INBOX_FILTERS;
      return {
        filtersByWorkspace: {
          ...state.filtersByWorkspace,
          [wsId]: { ...current, actors: toggleValue(current.actors, actor) },
        },
      };
    }),
  toggleUnreadOnly: (wsId) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId] ?? EMPTY_INBOX_FILTERS;
      return {
        filtersByWorkspace: {
          ...state.filtersByWorkspace,
          [wsId]: { ...current, unreadOnly: !current.unreadOnly },
        },
      };
    }),
  toggleIdleOnly: (wsId) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId] ?? EMPTY_INBOX_FILTERS;
      return {
        filtersByWorkspace: {
          ...state.filtersByWorkspace,
          [wsId]: { ...current, idleOnly: !current.idleOnly },
        },
      };
    }),
  clearPriorityFilters: (wsId) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId];
      if (!current || current.priorities.length === 0) return state;
      const next = { ...current, priorities: [] };
      // Emptiness is asked of every dimension, not just statuses: dropping the
      // entry while an actor or unread selection survived in it would clear a
      // filter the user set and the backend never had a say in.
      if (isEmptyInboxFilters(next)) {
        const { [wsId]: _removed, ...filtersByWorkspace } =
          state.filtersByWorkspace;
        return { filtersByWorkspace };
      }
      return {
        filtersByWorkspace: { ...state.filtersByWorkspace, [wsId]: next },
      };
    }),
  clearFilters: (wsId) =>
    set((state) => {
      if (!state.filtersByWorkspace[wsId]) return state;
      const { [wsId]: _removed, ...filtersByWorkspace } =
        state.filtersByWorkspace;
      return { filtersByWorkspace };
    }),
}));
=======
    {
      // One entry per workspace inside a single key, like the issue view
      // filters: switching workspaces swaps the selection without a reload.
      name: "multica_inbox_filters",
      version: 1,
      storage: createJSONStorage(() => defaultStorage),
      partialize: (state) => ({ filtersByWorkspace: state.filtersByWorkspace }),
      merge: (persisted, current) => {
        const stored = (persisted as { filtersByWorkspace?: unknown } | undefined)?.filtersByWorkspace;
        const filtersByWorkspace: Record<string, InboxFilters> = {};
        if (stored && typeof stored === "object") {
          for (const [wsId, raw] of Object.entries(stored)) {
            const filters = sanitizeInboxFilters(raw);
            if (!isEmptyInboxFilters(filters)) filtersByWorkspace[wsId] = filters;
          }
        }
        return { ...current, filtersByWorkspace };
      },
    },
  ),
);
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d

/** Workspace-isolated filter state with a stable empty fallback. */
export function useInboxFilters(wsId: string): InboxFilters {
  return (
    useInboxFilterStore((state) => state.filtersByWorkspace[wsId]) ??
    EMPTY_INBOX_FILTERS
  );
}
<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2

/**
 * Capability inferred from the parsed response, not from a version string.
 *
 * The new backend always serializes `issue_priority` for every Inbox row
 * (including `null` for notifications without an issue). An older backend
 * omits it. Requiring every returned row to carry a defined value also keeps a
 * priority cache patch from making a mixed rolling-deploy response look fully
 * supported.
 */
export function inboxPriorityFilterSupport(
  items: readonly InboxItem[],
): InboxPriorityFilterSupport {
  if (items.length === 0) return "unknown";
  return items.every((item) => item.issue_priority !== undefined)
    ? "supported"
    : "unsupported";
}

/** Ignore a stale priority selection until the backend capability is proven. */
export function inboxFiltersForPrioritySupport(
  filters: InboxFilters,
  support: InboxPriorityFilterSupport,
): InboxFilters {
  if (support === "supported" || filters.priorities.length === 0) {
    return filters;
  }
  return { ...filters, priorities: [] };
}

const EMPTY_ISSUE_IDS: ReadonlySet<string> = new Set();

/** Page state `filterInboxItems` needs beyond the selected filters. */
export interface InboxFilterContext {
  /** Issues an agent is running or queued on. Read only by `idleOnly`. */
  readonly activeIssueIds?: ReadonlySet<string>;
  /**
   * Group key (`issue_id ?? id`) of the open notification, pinned against the
   * unread dimension only. Set by the page after opening marks the row read.
   */
  readonly pinnedUnreadKey?: string | null;
  /**
   * Group key of the open notification, pinned against the idle dimension
   * only. Set by the page once the row was opened while idle.
   */
  readonly pinnedIdleKey?: string | null;
}

/**
 * The archive is filtered server-side and the backend has no idle or mention
 * dimension, so a selection kept from the active inbox stays dormant there
 * instead of narrowing whichever archived pages happen to be loaded.
 */
export function inboxFiltersForArchive(filters: InboxFilters): InboxFilters {
  return filters.idleOnly || filters.mentionsOnly
    ? { ...filters, idleOnly: false, mentionsOnly: false }
    : filters;
}

/**
 * OR within a dimension, AND between dimensions.
 *
 * Every dimension is read off the row the list actually renders. For status
 * and priority that is free — they are properties of the issue, identical on
 * every notification in the group. For the actor it is a deliberate choice:
 * deduplication keeps only a group's newest notification, and that row shows
 * ITS actor's avatar and name. Matching an actor buried in an older row of
 * the same group would answer a different question ("issues Alice has ever
 * touched") than the one the row then displays, so selecting Alice would fill
 * the list with Bob. What you filter by is what you see.
 *
 * `unreadOnly` likewise reads the rendered row's `read`. The unread badge
 * (`useInboxUnreadCount`) counts the same thing from the server's summary
 * endpoint, which applies that newest-per-issue rule in SQL — so "only unread"
 * and the number next to Inbox still cannot disagree.
 *
 * `idleOnly` asks about the issue, like status and priority, so a row without
 * one never matches. `activeIssueIds` are the issues an agent is running or
 * queued on; the caller derives them from the agent task snapshot.
 *
 * `mentionsOnly` asks about the issue's group, not just the newest row: a later
 * comment must not hide a mention, so deduplication copies the mention's source
 * onto the row it keeps (`groupInboxItemsByIssue`). A `@squad`-only mention
 * does not match.
 *
 * `pinnedUnreadKey` / `pinnedIdleKey` name the notification whose detail is
 * open. Opening an unread
 * notification marks it read, and replying from the detail can start an
 * agent on its issue; either would otherwise remove the row the user is
 * working in from under the detail pane. The pinned row therefore survives
 * the unread and idle dimensions only, and each pin only crosses its own
 * dimension: a row pinned for unread is still removed when the idle filter
 * is turned on over a busy issue, and vice versa. The status, priority, and
 * actor dimensions always apply, because changing one of those is an
 * explicit request to filter the notification itself out.
 */
export function filterInboxItems(
  items: InboxItem[],
  filters: InboxFilters,
  {
    activeIssueIds = EMPTY_ISSUE_IDS,
    pinnedUnreadKey = null,
    pinnedIdleKey = null,
  }: InboxFilterContext = {},
): InboxItem[] {
  if (isEmptyInboxFilters(filters)) return items;

  const statuses = new Set(filters.statuses);
  const priorities = new Set(filters.priorities);
  const actors = new Set(filters.actors);
  return items.filter((item) => {
    const actor = inboxActorKey(item);
    const matchesOtherDimensions =
      (statuses.size === 0 ||
        (item.issue_status != null && statuses.has(item.issue_status))) &&
      (priorities.size === 0 ||
        (item.issue_priority != null && priorities.has(item.issue_priority))) &&
      (actors.size === 0 || (actor != null && actors.has(actor)));
    if (!matchesOtherDimensions) return false;
    const key = item.issue_id ?? item.id;
    const matchesUnread =
      !filters.unreadOnly ||
      item.read !== true ||
      (pinnedUnreadKey != null && key === pinnedUnreadKey);
    if (filters.mentionsOnly && !isViewerMention(item)) return false;
    const matchesIdle =
      !filters.idleOnly ||
      (item.issue_id != null && !activeIssueIds.has(item.issue_id)) ||
      (pinnedIdleKey != null && key === pinnedIdleKey);
    return matchesUnread && matchesIdle;
  });
}

export function inboxFilterCount(filters: InboxFilters): number {
  return (
    filters.statuses.length +
    filters.priorities.length +
    filters.actors.length +
    (filters.unreadOnly ? 1 : 0) +
    (filters.idleOnly ? 1 : 0) +
    (filters.mentionsOnly ? 1 : 0)
  );
}
||||||| 2ae2f08005

/**
 * Capability inferred from the parsed response, not from a version string.
 *
 * The new backend always serializes `issue_priority` for every Inbox row
 * (including `null` for notifications without an issue). An older backend
 * omits it. Requiring every returned row to carry a defined value also keeps a
 * priority cache patch from making a mixed rolling-deploy response look fully
 * supported.
 */
export function inboxPriorityFilterSupport(
  items: readonly InboxItem[],
): InboxPriorityFilterSupport {
  if (items.length === 0) return "unknown";
  return items.every((item) => item.issue_priority !== undefined)
    ? "supported"
    : "unsupported";
}

/** Ignore a stale priority selection until the backend capability is proven. */
export function inboxFiltersForPrioritySupport(
  filters: InboxFilters,
  support: InboxPriorityFilterSupport,
): InboxFilters {
  if (support === "supported" || filters.priorities.length === 0) {
    return filters;
  }
  return { ...filters, priorities: [] };
}

const EMPTY_ISSUE_IDS: ReadonlySet<string> = new Set();

/** Page state `filterInboxItems` needs beyond the selected filters. */
export interface InboxFilterContext {
  /** Issues an agent is running or queued on. Read only by `idleOnly`. */
  readonly activeIssueIds?: ReadonlySet<string>;
  /**
   * Group key (`issue_id ?? id`) of the open notification, pinned against the
   * unread dimension only. Set by the page after opening marks the row read.
   */
  readonly pinnedUnreadKey?: string | null;
  /**
   * Group key of the open notification, pinned against the idle dimension
   * only. Set by the page once the row was opened while idle.
   */
  readonly pinnedIdleKey?: string | null;
}

/**
 * The archive is filtered server-side and the backend has no idle dimension,
 * so a selection kept from the active inbox stays dormant there instead of
 * narrowing whichever archived pages happen to be loaded.
 */
export function inboxFiltersForArchive(filters: InboxFilters): InboxFilters {
  return filters.idleOnly ? { ...filters, idleOnly: false } : filters;
}

/**
 * OR within a dimension, AND between dimensions.
 *
 * Every dimension is read off the row the list actually renders. For status
 * and priority that is free — they are properties of the issue, identical on
 * every notification in the group. For the actor it is a deliberate choice:
 * deduplication keeps only a group's newest notification, and that row shows
 * ITS actor's avatar and name. Matching an actor buried in an older row of
 * the same group would answer a different question ("issues Alice has ever
 * touched") than the one the row then displays, so selecting Alice would fill
 * the list with Bob. What you filter by is what you see.
 *
 * `unreadOnly` likewise reads the rendered row's `read`. The unread badge
 * (`useInboxUnreadCount`) counts the same thing from the server's summary
 * endpoint, which applies that newest-per-issue rule in SQL — so "only unread"
 * and the number next to Inbox still cannot disagree.
 *
 * `idleOnly` asks about the issue, like status and priority, so a row without
 * one never matches. `activeIssueIds` are the issues an agent is running or
 * queued on; the caller derives them from the agent task snapshot.
 *
 * `pinnedUnreadKey` / `pinnedIdleKey` name the notification whose detail is
 * open. Opening an unread
 * notification marks it read, and replying from the detail can start an
 * agent on its issue; either would otherwise remove the row the user is
 * working in from under the detail pane. The pinned row therefore survives
 * the unread and idle dimensions only, and each pin only crosses its own
 * dimension: a row pinned for unread is still removed when the idle filter
 * is turned on over a busy issue, and vice versa. The status, priority, and
 * actor dimensions always apply, because changing one of those is an
 * explicit request to filter the notification itself out.
 */
export function filterInboxItems(
  items: InboxItem[],
  filters: InboxFilters,
  {
    activeIssueIds = EMPTY_ISSUE_IDS,
    pinnedUnreadKey = null,
    pinnedIdleKey = null,
  }: InboxFilterContext = {},
): InboxItem[] {
  if (isEmptyInboxFilters(filters)) return items;

  const statuses = new Set(filters.statuses);
  const priorities = new Set(filters.priorities);
  const actors = new Set(filters.actors);
  return items.filter((item) => {
    const actor = inboxActorKey(item);
    const matchesOtherDimensions =
      (statuses.size === 0 ||
        (item.issue_status != null && statuses.has(item.issue_status))) &&
      (priorities.size === 0 ||
        (item.issue_priority != null && priorities.has(item.issue_priority))) &&
      (actors.size === 0 || (actor != null && actors.has(actor)));
    if (!matchesOtherDimensions) return false;
    const key = item.issue_id ?? item.id;
    const matchesUnread =
      !filters.unreadOnly ||
      item.read !== true ||
      (pinnedUnreadKey != null && key === pinnedUnreadKey);
    const matchesIdle =
      !filters.idleOnly ||
      (item.issue_id != null && !activeIssueIds.has(item.issue_id)) ||
      (pinnedIdleKey != null && key === pinnedIdleKey);
    return matchesUnread && matchesIdle;
  });
}

export function inboxFilterCount(filters: InboxFilters): number {
  return (
    filters.statuses.length +
    filters.priorities.length +
    filters.actors.length +
    (filters.unreadOnly ? 1 : 0) +
    (filters.idleOnly ? 1 : 0)
  );
}
=======
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
