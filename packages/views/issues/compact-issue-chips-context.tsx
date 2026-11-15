"use client";

import { createContext, useContext, type ReactNode } from "react";

/**
 * Whether issue chips in the surrounding prose show the identifier only.
 *
 * Defaults to false: a chip normally reads `<status> <identifier> <title>`.
 * A surface whose prose wraps badly around long titles (the decision card)
 * opts in; the hover card still shows the title.
 */
const CompactIssueChipsContext = createContext(false);

export function CompactIssueChipsProvider({ children }: { children: ReactNode }) {
  return (
    <CompactIssueChipsContext.Provider value={true}>
      {children}
    </CompactIssueChipsContext.Provider>
  );
}

export function useCompactIssueChips(): boolean {
  return useContext(CompactIssueChipsContext);
}
