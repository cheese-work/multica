import type { ProjectStatus, ProjectPriority, ProjectVisibility } from "../types";
import { createDraftStore } from "../drafts/create-draft-store";
import { createKeyedDraftStore } from "../drafts/create-keyed-draft-store";

interface ProjectDraft {
  title: string;
  description: string;
  status: ProjectStatus;
  priority: ProjectPriority;
  leadType?: "member" | "agent";
  leadId?: string;
  icon?: string;
  // Calendar days ("YYYY-MM-DD"); empty/undefined means unset.
  startDate?: string;
  dueDate?: string;
  visibility?: ProjectVisibility;
  memberIds?: string[];
}

const EMPTY_DRAFT: ProjectDraft = {
  title: "",
  description: "",
  status: "planned",
  priority: "none",
  leadType: undefined,
  leadId: undefined,
  icon: undefined,
  startDate: undefined,
  dueDate: undefined,
  visibility: "workspace",
  memberIds: [],
};

export const useProjectDraftStore = createDraftStore<ProjectDraft>({
  storageKey: "multica_project_draft",
  emptyData: EMPTY_DRAFT,
  hasMeaningful: (d) => !!(d.title || d.description),
});

/**
 * Unsent name typed into the rename dialog, one entry per project id.
 * `value: null` means no edit; `base` is the name the edit started from.
 */
interface ProjectRenameDraft {
  value: string | null;
  base: string | null;
}

export const EMPTY_PROJECT_RENAME_DRAFT: ProjectRenameDraft = { value: null, base: null };

export const useProjectRenameDraftStore = createKeyedDraftStore<ProjectRenameDraft>({
  storageKey: "multica_project_rename_drafts",
  emptyData: EMPTY_PROJECT_RENAME_DRAFT,
  hasMeaningful: (d) => d.value !== null,
});
