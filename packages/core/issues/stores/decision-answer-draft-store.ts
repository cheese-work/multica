import { createKeyedDraftStore } from "../../drafts/create-keyed-draft-store";
import { registerDraftCleanup } from "../../drafts/cleanup-registry";
import { getCurrentSlug } from "../../platform/workspace-storage";

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

// An upload that settles after its card unmounted reports through a callback that outlives the
// card. `captureDecisionDraftScope` is taken while mounted; `appendLateDecisionNote` writes only
// if the workspace and session are still the same, and appends instead of replacing, so a note
// edited in between survives.
let sessionEpoch = 0;
registerDraftCleanup({
  storageKey: "multica_decision_answer_drafts_session",
  workspaceScoped: false,
  resetInMemory: () => { sessionEpoch++; },
});

export const captureDecisionDraftScope = () => ({ slug: getCurrentSlug(), epoch: sessionEpoch });

export function appendLateDecisionNote(
  scope: ReturnType<typeof captureDecisionDraftScope>,
  requestId: string,
  base: string,
  markdown: string,
) {
  if (scope.slug !== getCurrentSlug() || scope.epoch !== sessionEpoch) return;
  const key = decisionAnswerDraftKey(requestId);
  const store = useDecisionAnswerDraftStore.getState();
  const current = store.getDraft(key)?.text ?? "";
  const head = base.trimEnd();
  // The editor re-emits its baseline plus the settled attachment link.
  const added = markdown.startsWith(head) ? markdown.slice(head.length).trim() : null;
  const next = added === null ? current || markdown : current.includes(added) ? current : `${current.trimEnd()}\n\n${added}`.trim();
  if (next !== current) store.setDraft(key, { text: next });
}
