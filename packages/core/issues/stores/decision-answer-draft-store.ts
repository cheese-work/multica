import { getDraftGeneration, type DraftOrigin } from "../../drafts/cleanup-registry";
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
 * Adds an attachment link to the end of the stored note, as a delayed write that belongs to
 * `origin`; a link already there stays single. Dropped when another tab cleared the draft.
 */
export function appendDecisionAttachment(origin: DraftOrigin, requestId: string, markdown: string): void {
  const key = decisionAnswerDraftKey(requestId);
  const store = useDecisionAnswerDraftStore.getState();
  if (origin.generation !== getDraftGeneration(origin.slug)) return;
  // Another tab's logout wiped shared storage before this tab heard of it: do not persist the
  // note this tab still holds.
  if (store.isClearedElsewhere(key)) return store.clearDraft(key);
  const text = store.getDraft(key)?.text ?? "";
  if (text.includes(markdown)) return;
  store.setDraftIn(origin, key, { text: `${text.trimEnd()}\n\n${markdown}`.trim() });
}
