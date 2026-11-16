import { createKeyedDraftStore } from "../../drafts/create-keyed-draft-store";

/**
 * Unsent per-item Approve / Reject choices on a batch decision card, keyed by batch id.
 * A choice only selects: sending still needs the button press and, for Approve, the passkey.
 */
export interface DecisionBatchChoices {
  choices: Record<string, "approve" | "reject">;
}

export const useDecisionBatchChoicesStore = createKeyedDraftStore<DecisionBatchChoices>({
  storageKey: "multica_decision_batch_choices",
  // Kept until sent or the batch is decided, never by age (workspace rule on unsent input).
  ttlMs: Number.POSITIVE_INFINITY,
  emptyData: { choices: {} },
  hasMeaningful: (d) => Object.keys(d.choices).length > 0,
});
