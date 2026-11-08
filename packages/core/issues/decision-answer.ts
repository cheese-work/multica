import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { signPasskeyChallenge } from "../auth/webauthn";
import { chatKeys } from "../chat/queries";
import { issueKeys } from "./queries";

/**
 * Answers an open decision card from the card itself. Not optimistic: the
 * server records the answer, posts the quote on the issue and wakes the
 * assignee; the card flips to answered when the messages refetch. `passkey`
 * runs the challenge and assertion first, for a class the workspace gate blocks.
 */
export function useAnswerDecisionRequest(issueId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async ({ requestId, passkey, ...input }: { requestId: string; option?: string; text?: string; passkey?: boolean }) => {
      if (!passkey) return api.answerDecisionRequest(requestId, input);
      // A gated answer: the challenge binds this option and text, and returns the revisions the answer must repeat.
      const { answer_revision, policy_revision, ...challenge } = await api.challengeDecisionAnswer(requestId, input);
      const assertion = await signPasskeyChallenge(challenge);
      return api.answerDecisionRequest(requestId, { ...input, answer_revision, policy_revision, assertion });
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: issueKeys.timeline(issueId) });
      qc.invalidateQueries({ queryKey: chatKeys.messagesAll() });
      qc.invalidateQueries({ queryKey: chatKeys.messagesPageAll() });
    },
  });
}
