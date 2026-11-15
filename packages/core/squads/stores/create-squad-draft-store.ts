import { createKeyedDraftStore } from "../../drafts/create-keyed-draft-store";

export interface CreateSquadDraftMember {
  type: "agent" | "member";
  id: string;
  name: string;
}

export interface CreateSquadDraft {
  name: string;
  description: string;
  avatarUrl: string | null;
  leaderId: string;
  members: CreateSquadDraftMember[];
}

export const EMPTY_CREATE_SQUAD_DRAFT: CreateSquadDraft = {
  name: "",
  description: "",
  avatarUrl: null,
  leaderId: "",
  members: [],
};

/** Draft key for the create-squad modal; the modal has no per-entity target. */
export const CREATE_SQUAD_DRAFT_KEY = "create";

export const useCreateSquadDraftStore = createKeyedDraftStore<CreateSquadDraft>({
  storageKey: "multica_create_squad_draft",
  emptyData: EMPTY_CREATE_SQUAD_DRAFT,
  hasMeaningful: (d) =>
    d.name !== "" ||
    d.description !== "" ||
    d.avatarUrl !== null ||
    d.leaderId !== "" ||
    d.members.length > 0,
});
