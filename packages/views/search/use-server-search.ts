"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "@multica/core/api";
import type { SearchIssueResult, SearchProjectResult } from "@multica/core/types";
import { isLocalSearchReady, searchIssues, searchProjects } from "@multica/core/search-index";
import { scoreText } from "./matching";

export interface SearchResults {
  /**
   * The trimmed query these rows answer. Rows outlive the query that fetched
   * them — they stay painted while the next request is in flight — so every
   * result set carries the question it is the answer to, and anything that can
   * act on a row checks that it still matches what the user typed.
   */
  query: string;
  issues: SearchIssueResult[];
  projects: SearchProjectResult[];
}

export const NO_RESULTS: SearchResults = { query: "", issues: [], projects: [] };

const TITLE_TIER_LIMIT = 8;
const FULL_SEARCH_DELAY_MS = 300;

/**
 * Issue (and optionally project) search for the palette. An empty query
 * clears the results without a request.
 *
 * Without the local index the search has two tiers (CHE-7337). The title tier
 * asks the server for title matches, newest first, on every keystroke. The full
 * ranked search follows after a pause (or `searchNow`, on Enter) and replaces
 * those rows in place; a title response that lands after it is dropped.
 */
export function useServerSearch(query: string, options: { projects: boolean }) {
  const [results, setResults] = useState<SearchResults>(NO_RESULTS);
  const [isLoading, setIsLoading] = useState(false);
  const withProjects = options.projects;
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const fullSearchRef = useRef<(() => Promise<void>) | null>(null);
  const q = query.trim();

  useEffect(() => {
    if (debounceRef.current) clearTimeout(debounceRef.current);
    if (abortRef.current) abortRef.current.abort();

    if (!q) {
      setResults(NO_RESULTS);
      setIsLoading(false);
      return;
    }

    setIsLoading(true);
    const controller = new AbortController();
    abortRef.current = controller;
    // The debounce spares the server a full ranked search per keystroke; the
    // local index answers in milliseconds, so it searches on every keystroke.
    const local = isLocalSearchReady();
    let fullDone = false;
    if (!local) {
      api
        .searchIssues({ q, limit: TITLE_TIER_LIMIT, include_closed: true, title_only: true, signal: controller.signal })
        .then((res) => {
          if (controller.signal.aborted || fullDone) return;
          // The title tier owns issues only; the full search stays the source of
          // truth for projects. Keep the previous projects that still title-match
          // so the group does not vanish between tiers.
          const needle = q.toLowerCase();
          setResults((prev) => ({
            query: q,
            issues: res.issues,
            projects: prev.projects.filter((p) => scoreText(p.title, needle) > 0),
          }));
        })
        .catch(() => {
          // The full search reports the failure; the title tier stays silent.
        });
    }
    const runFull = async () => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
      fullSearchRef.current = null;
      try {
        const [issueRes, projectRes] = await Promise.all([
          searchIssues({ q, limit: 20, include_closed: true, signal: controller.signal }),
          withProjects
            ? searchProjects({ q, limit: 10, include_closed: true, include_archived: true, signal: controller.signal })
            : Promise.resolve({ projects: [] as SearchProjectResult[] }),
        ]);
        if (!controller.signal.aborted) {
          fullDone = true;
          setResults({ query: q, issues: issueRes.issues, projects: projectRes.projects });
          setIsLoading(false);
        }
      } catch {
        if (!controller.signal.aborted) {
          // Drop the previous query's rows rather than leaving them on screen
          // permanently greyed out: the request that would have replaced them
          // is never coming. The list falls through to the empty state.
          fullDone = true;
          setResults({ query: q, issues: [], projects: [] });
          setIsLoading(false);
        }
      }
    };
    fullSearchRef.current = runFull;
    debounceRef.current = setTimeout(runFull, local ? 0 : FULL_SEARCH_DELAY_MS);
  }, [q, withProjects]);

  /** Runs the pending full search now instead of after the pause. */
  const searchNow = useCallback(() => void fullSearchRef.current?.(), []);

  useEffect(
    () => () => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
      if (abortRef.current) abortRef.current.abort();
    },
    [],
  );

  // Rows answering an earlier query are still painted while the next request
  // is in flight — that is what keeps the list from strobing on every
  // keystroke — but they must not be actionable: cmdk re-selects the first
  // valid item whenever the input changes, so a live stale row turns the next
  // Enter into a jump to a result the user has already typed past.
  const stale = results.query !== q;
  return { results, isLoading, stale, searchNow };
}
