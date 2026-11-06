import { createKeyedDraftStore } from "../../drafts/create-keyed-draft-store";

/**
 * Unsent answer to an open decision card: the chosen option and the note. The
 * note is markdown, so images uploaded into it travel as links inside `text`.
 * Keyed `decision:<requestId>`, so the chat card and the issue banner share one
 * draft per request. `uploadCancelled`: an upload was still running when the card
 * went away and was cancelled; the card says so until the next upload, Send or Discard.
 */
export interface DecisionAnswerDraft {
  option: string;
  text: string;
  uploadCancelled: boolean;
}

export const EMPTY_DECISION_ANSWER_DRAFT: DecisionAnswerDraft = { option: "", text: "", uploadCancelled: false };

export const decisionAnswerDraftKey = (requestId: string) => `decision:${requestId}`;

export const useDecisionAnswerDraftStore = createKeyedDraftStore<DecisionAnswerDraft>({
  storageKey: "multica_decision_answer_drafts",
  emptyData: EMPTY_DECISION_ANSWER_DRAFT,
  hasMeaningful: (d) => d.option !== "" || d.text.trim() !== "" || d.uploadCancelled,
});
