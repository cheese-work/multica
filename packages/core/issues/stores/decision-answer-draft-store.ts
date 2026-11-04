import { createKeyedDraftStore } from "../../drafts/create-keyed-draft-store";

/**
 * Unsent answer to an open decision card: the chosen option and the note. The
 * note is markdown, so images uploaded into it travel as links inside `text`.
 * Keyed `decision:<requestId>`, so the chat card and the issue banner share one
 * draft per request.
 */
export interface DecisionAnswerDraft {
  option: string;
  text: string;
}

export const EMPTY_DECISION_ANSWER_DRAFT: DecisionAnswerDraft = { option: "", text: "" };

export const decisionAnswerDraftKey = (requestId: string) => `decision:${requestId}`;

export const useDecisionAnswerDraftStore = createKeyedDraftStore<DecisionAnswerDraft>({
  storageKey: "multica_decision_answer_drafts",
  emptyData: EMPTY_DECISION_ANSWER_DRAFT,
  hasMeaningful: (d) => d.option !== "" || d.text.trim() !== "",
});

/**
 * Adds an attachment link to the end of the note stored in workspace `slug`; a link already
 * there stays single. `slug` is the workspace the upload started in, not necessarily the active one.
 */
export function appendDecisionAttachment(slug: string | null, requestId: string, markdown: string): void {
  useDecisionAnswerDraftStore.getState().patchDraftIn(slug, decisionAnswerDraftKey(requestId), (current) =>
    current.text.includes(markdown) ? current : { text: `${current.text.trimEnd()}\n\n${markdown}`.trim() },
  );
}
