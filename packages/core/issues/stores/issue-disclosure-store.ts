import { create } from "zustand";

interface IssueDisclosureStore {
  readonly descriptionExpandedIssueIds: ReadonlySet<string>;
  readonly expandedThreadIdsByIssue: Record<string, ReadonlySet<string>>;
  setDescriptionExpanded: (issueId: string, expanded: boolean) => void;
  setThreadExpanded: (issueId: string, rootId: string, expanded: boolean) => void;
  expandAllThreads: (issueId: string, rootIds: readonly string[]) => void;
  collapseAllThreads: (issueId: string) => void;
  clearIssue: (issueId: string) => void;
}

const EMPTY_IDS: ReadonlySet<string> = new Set();
const EMPTY_THREADS: Record<string, ReadonlySet<string>> = {};

function withoutIssue(
  threadsByIssue: Record<string, ReadonlySet<string>>,
  issueId: string,
): Record<string, ReadonlySet<string>> {
  const { [issueId]: _, ...remaining } = threadsByIssue;
  return remaining;
}

/**
 * Session-only preferences for issue description and long-thread disclosure.
 * Query-owned resolution and persisted manual comment collapse stay in their
 * existing stores; this store intentionally contains no server data.
 */
export const useIssueDisclosureStore = create<IssueDisclosureStore>()((set) => ({
  descriptionExpandedIssueIds: EMPTY_IDS,
  expandedThreadIdsByIssue: EMPTY_THREADS,
  setDescriptionExpanded: (issueId, expanded) =>
    set((state) => {
      if (state.descriptionExpandedIssueIds.has(issueId) === expanded) return state;
      const next = new Set(state.descriptionExpandedIssueIds);
      if (expanded) next.add(issueId);
      else next.delete(issueId);
      return { descriptionExpandedIssueIds: next.size === 0 ? EMPTY_IDS : next };
    }),
  setThreadExpanded: (issueId, rootId, expanded) =>
    set((state) => {
      const current = state.expandedThreadIdsByIssue[issueId] ?? EMPTY_IDS;
      if (current.has(rootId) === expanded) return state;
      const next = new Set(current);
      if (expanded) next.add(rootId);
      else next.delete(rootId);
      return {
        expandedThreadIdsByIssue:
          next.size === 0
            ? withoutIssue(state.expandedThreadIdsByIssue, issueId)
            : { ...state.expandedThreadIdsByIssue, [issueId]: next },
      };
    }),
  expandAllThreads: (issueId, rootIds) =>
    set((state) => {
      if (rootIds.length === 0) return state;
      const current = state.expandedThreadIdsByIssue[issueId] ?? EMPTY_IDS;
      const next = new Set(current);
      for (const rootId of rootIds) next.add(rootId);
      if (next.size === current.size) return state;
      return {
        expandedThreadIdsByIssue: {
          ...state.expandedThreadIdsByIssue,
          [issueId]: next,
        },
      };
    }),
  collapseAllThreads: (issueId) =>
    set((state) => {
      if (!(issueId in state.expandedThreadIdsByIssue)) return state;
      return { expandedThreadIdsByIssue: withoutIssue(state.expandedThreadIdsByIssue, issueId) };
    }),
  clearIssue: (issueId) =>
    set((state) => {
      const hasDescription = state.descriptionExpandedIssueIds.has(issueId);
      const hasThreads = issueId in state.expandedThreadIdsByIssue;
      if (!hasDescription && !hasThreads) return state;
      const descriptions = new Set(state.descriptionExpandedIssueIds);
      descriptions.delete(issueId);
      return {
        descriptionExpandedIssueIds: descriptions.size === 0 ? EMPTY_IDS : descriptions,
        expandedThreadIdsByIssue: hasThreads
          ? withoutIssue(state.expandedThreadIdsByIssue, issueId)
          : state.expandedThreadIdsByIssue,
      };
    }),
}));

export function selectDescriptionExpanded(issueId: string) {
  return (state: IssueDisclosureStore) => state.descriptionExpandedIssueIds.has(issueId);
}

export function selectExpandedThreads(issueId: string) {
  return (state: IssueDisclosureStore): ReadonlySet<string> =>
    state.expandedThreadIdsByIssue[issueId] ?? EMPTY_IDS;
}
