import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { signPasskeyChallenge } from "../auth/webauthn";
import { chatKeys } from "../chat/queries";
import { issueKeys } from "./queries";

/**
 * The acknowledgment a batch card's option answer carries: SHA-256 hex of the
 * batch id, the sorted covered ids (comma joined) and the chosen option, one per
 * line. The server computes the same value and refuses the answer without it, so
 * only a client that held the covered ids can answer for them.
 */
export async function batchCoverAck(batchId: string, coveredIds: string[], option: string): Promise<string> {
  const text = `${batchId}\n${[...coveredIds].sort().join(",")}\n${option}`;
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(text));
  return Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

/**
 * Answers an open decision card from the card itself. Not optimistic: the
 * server records the answer, posts the quote on the issue and wakes the
 * assignee; the card flips to answered when the messages refetch. `passkey`
 * runs the challenge and assertion first, for a class the workspace gate blocks.
 */
export function useAnswerDecisionRequest(issueId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async ({ requestId, passkey, covers, ...answer }: { requestId: string; option?: string; text?: string; passkey?: boolean; covers?: string[] }) => {
      // A batch card's option answer also closes the cards it covers: it acknowledges exactly those.
      const input = covers?.length && answer.option ? { ...answer, covers_ack: await batchCoverAck(requestId, covers, answer.option) } : answer;
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
