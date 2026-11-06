import { createKeyedDraftStore } from "../../drafts/create-keyed-draft-store";

/**
 * Unsent text of a single-field edit. `base` is the saved value the edit
 * started from, so a restore can tell whether the source moved underneath it.
 * `value: null` is "no edit"; an emptied field (`""`) is still an edit.
 */
export interface TextEditDraft {
  value: string | null;
  base: string | null;
}

/** Stable fallback for `usePersistedDraft`. */
export const EMPTY_TEXT_EDIT: TextEditDraft = { value: null, base: null };

const textEditStore = (storageKey: string) =>
  createKeyedDraftStore<TextEditDraft>({
    storageKey,
    emptyData: EMPTY_TEXT_EDIT,
    hasMeaningful: (d) => d.value !== null,
  });

/** Key: issue id. Inline title edit, saved on blur. */
export const useIssueTitleDraftStore = textEditStore("multica_issue_title_edit_drafts");

/** Key: wakeup id. Edit-prompt dialog. */
export const useWakeupInstructionDraftStore = textEditStore("multica_wakeup_instruction_drafts");

/** Key: annotation note identity (draft key + quote anchor). Note popover. */
export const useAnnotationNoteDraftStore = textEditStore("multica_annotation_note_drafts");

/**
 * Key: issue id (or another form scope). Fields of the create-wakeup form the
 * member has changed; the view overlays them on its own defaults. JSON-safe
 * values only.
 */
export interface WakeupFormDraft {
  fields: Record<string, unknown> | null;
}

export const EMPTY_WAKEUP_FORM_DRAFT: WakeupFormDraft = { fields: null };

export const useWakeupFormDraftStore = createKeyedDraftStore<WakeupFormDraft>({
  storageKey: "multica_wakeup_form_drafts",
  emptyData: EMPTY_WAKEUP_FORM_DRAFT,
  hasMeaningful: (d) => d.fields !== null,
});
