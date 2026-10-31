import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import type { DecisionArtifactHunk } from "../types";

export const decisionArtifactKeys = {
  list: (requestId: string) => ["decision-artifacts", requestId] as const,
};

/** The files a decision request approves. An ask without files yields an empty list. */
export function useDecisionArtifacts(requestId: string) {
  return useQuery({
    queryKey: decisionArtifactKeys.list(requestId),
    queryFn: () => api.listDecisionArtifacts(requestId),
    staleTime: 60_000,
  });
}

/** Added and removed line counts over changed hunks only. */
export function countDiffLines(hunks: DecisionArtifactHunk[]) {
  let added = 0;
  let removed = 0;
  for (const hunk of hunks) {
    for (const line of hunk.lines) {
      if (line.startsWith("+")) added++;
      else if (line.startsWith("-")) removed++;
    }
  }
  return { added, removed };
}
