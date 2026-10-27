import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { ApiError } from "../api/client";
import { chatKeys } from "../chat/queries";
import { issueKeys } from "./queries";

export type DisclaimConflict = "changed" | "withdrawn";

/**
 * The Disclaim endpoint answers 409 two ways: the decision's context moved
 * ("decision context changed; retry") or it is already withdrawn. Anything else
 * is not a conflict. An unrecognised 409 reads as "changed": refreshing is the
 * safe response when the server gives no more detail.
 */
export function disclaimConflict(error: unknown): DisclaimConflict | undefined {
  if (!(error instanceof ApiError) || error.status !== 409) return undefined;
  return /already withdrawn/i.test(error.message) ? "withdrawn" : "changed";
}

/**
 * A decision quote comment is `[agent note\n\n]> quoted line\n> quoted line`.
 * Returns the agent's optional note and the human's words without the `> `.
 */
export function splitDecisionQuote(content: string): { note: string; quote: string } {
  const lines = content.split("\n");
  let start = lines.length;
  while (start > 0 && /^>( |$)/.test(lines[start - 1]!)) start--;
  const quote = lines.slice(start).map((line) => line.replace(/^> ?/, "")).join("\n");
  return { note: lines.slice(0, start).join("\n").trim(), quote };
}

/**
 * Withdraws a decision answer. Not optimistic: the server decides whether the
 * request reopens or cancels. A 409 is returned to the caller to show; the
 * card state refreshes from the timeline either way.
 */
export function useDisclaimDecisionAnswer(issueId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (answerId: string) => api.disclaimDecisionAnswer(answerId),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: issueKeys.timeline(issueId) });
      qc.invalidateQueries({ queryKey: chatKeys.messagesAll() });
      qc.invalidateQueries({ queryKey: chatKeys.messagesPageAll() });
    },
  });
}
