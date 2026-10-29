import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { chatKeys } from "../chat/queries";
import { issueKeys } from "./queries";

/**
 * Answers an open decision card from the card itself. Not optimistic: the
 * server records the answer, posts the quote on the issue and wakes the
 * assignee; the card flips to answered when the messages refetch.
 */
export function useAnswerDecisionRequest(issueId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ requestId, ...input }: { requestId: string; option?: string; text?: string }) =>
      api.answerDecisionRequest(requestId, input),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: issueKeys.timeline(issueId) });
      qc.invalidateQueries({ queryKey: chatKeys.messagesAll() });
      qc.invalidateQueries({ queryKey: chatKeys.messagesPageAll() });
    },
  });
}
