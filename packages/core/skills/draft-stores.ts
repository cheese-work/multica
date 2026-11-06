import { createKeyedDraftStore } from "../drafts/create-keyed-draft-store";

/** The create-skill dialog's manual form. One entry, key {@link CREATE_SKILL_DRAFT_KEY}. */
export interface SkillCreateDraft {
  name: string;
  description: string;
}

export const CREATE_SKILL_DRAFT_KEY = "new";
export const EMPTY_SKILL_CREATE_DRAFT: SkillCreateDraft = { name: "", description: "" };

export const useSkillCreateDraftStore = createKeyedDraftStore<SkillCreateDraft>({
  storageKey: "multica_skill_create_drafts",
  emptyData: EMPTY_SKILL_CREATE_DRAFT,
  hasMeaningful: (d) => d.name.length > 0 || d.description.length > 0,
});

/**
 * Unsaved edits on a skill's detail page, keyed by skill id. Written only
 * while the page differs from the server version it started from, so
 * `baseUpdatedAt` doubles as the "holds edits" marker (every field can be
 * legitimately empty) and tells a restore whether the server moved on since.
 */
export interface SkillEditDraft {
  name: string;
  description: string;
  content: string;
  files: { id?: string; path: string; content: string }[];
  baseUpdatedAt: string;
}

export const EMPTY_SKILL_EDIT_DRAFT: SkillEditDraft = {
  name: "",
  description: "",
  content: "",
  files: [],
  baseUpdatedAt: "",
};

export const useSkillEditDraftStore = createKeyedDraftStore<SkillEditDraft>({
  storageKey: "multica_skill_edit_drafts",
  emptyData: EMPTY_SKILL_EDIT_DRAFT,
  hasMeaningful: (d) => d.baseUpdatedAt !== "",
});
