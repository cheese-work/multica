import { createKeyedDraftStore } from "../../drafts/create-keyed-draft-store";

/**
 * Unsent answer to an open decision card: the chosen option and the note. The
 * note is markdown, so images uploaded into it travel as links inside `text`.
 * Keyed by the bare request id, the key older versions wrote, so unsent drafts
 * survive an upgrade. The chat card and the issue banner share one draft per request.
 */
export interface DecisionAnswerDraft {
  option: string;
  text: string;
}

export const EMPTY_DECISION_ANSWER_DRAFT: DecisionAnswerDraft = { option: "", text: "" };

export const decisionAnswerDraftKey = (requestId: string) => requestId;

export const useDecisionAnswerDraftStore = createKeyedDraftStore<DecisionAnswerDraft>({
  storageKey: "multica_decision_answer_drafts",
  // Kept until sent or discarded, never by age (main 39ee32f3db; workspace rule on unsent input).
  ttlMs: Number.POSITIVE_INFINITY,
  emptyData: EMPTY_DECISION_ANSWER_DRAFT,
  hasMeaningful: (d) => d.option !== "" || d.text.trim() !== "",
});
