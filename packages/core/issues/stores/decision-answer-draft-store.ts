import { createKeyedDraftStore } from "../../drafts/create-keyed-draft-store";

/** The unsent answer on a decision card: the selected option label and the note. */
export interface DecisionAnswerDraft {
  option: string;
  text: string;
}

/** One draft per decision request id; cleared on a successful send or an explicit discard, never by age. */
export const useDecisionAnswerDraftStore = createKeyedDraftStore<DecisionAnswerDraft>({
  storageKey: "multica_decision_answer_drafts",
  ttlMs: Number.POSITIVE_INFINITY,
  emptyData: { option: "", text: "" },
  hasMeaningful: (d) => d.option !== "" || d.text.trim() !== "",
});
