import { createKeyedDraftStore } from "../drafts/create-keyed-draft-store";
import type { QuickActionAssigneeType, QuickActionVisibility } from "../types";

/**
 * Overlay on the dialog's initial form (empty, or the action being edited): a
 * field is `undefined` until the user touches it.
 */
export interface QuickActionDraft {
  name?: string;
  description?: string;
  assigneeType?: Exclude<QuickActionAssigneeType, "">;
  assigneeId?: string;
  prompt?: string;
  visibility?: QuickActionVisibility;
}

/** One draft per dialog target: `create` or `edit:<id>`. */
export const useQuickActionDraftStore = createKeyedDraftStore<QuickActionDraft>({
  storageKey: "multica_quick_action_drafts",
  emptyData: {},
  hasMeaningful: (d) => Object.values(d).some((v) => v !== undefined),
});
