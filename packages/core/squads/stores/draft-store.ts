"use client";

import { createKeyedDraftStore } from "../../drafts/create-keyed-draft-store";
import { usePersistedDraft } from "../../drafts/use-persisted-draft";

/**
 * Unsent input on the squad detail page (instructions, name, description,
 * member role), one entry per `${squadId}:${field}` key. A `null` text means
 * "untouched": an empty string is a real edit (clearing a field), so it has to
 * stay distinguishable from no draft.
 */
export interface SquadFieldDraft {
  text: string | null;
}

const EMPTY_FIELD_DRAFT: SquadFieldDraft = { text: null };

export const useSquadFieldDraftStore = createKeyedDraftStore<SquadFieldDraft>({
  storageKey: "multica_squad_field_drafts",
  emptyData: EMPTY_FIELD_DRAFT,
  hasMeaningful: (d) => d.text !== null,
});

/** `${squadId}:${field}`, optionally narrowed by `:${sub}` (e.g. a member). */
export function squadDraftKey(squadId: string, field: string, sub?: string): string {
  return sub ? `${squadId}:${field}:${sub}` : `${squadId}:${field}`;
}

/**
 * Bind a text field to its draft. `text` is the draft when one exists, else
 * the saved value; typing the saved value back drops the draft.
 */
export function useSquadFieldDraft(
  key: string,
  saved: string,
): {
  text: string;
  hasDraft: boolean;
  setText: (next: string) => void;
  discard: () => void;
  /** Drop the draft after a save of `sent`, unless the user typed more since. */
  discardSent: (sent: string) => void;
} {
  const [draft, setDraft, discard] = usePersistedDraft(
    useSquadFieldDraftStore,
    key,
    EMPTY_FIELD_DRAFT,
  );
  return {
    text: draft.text ?? saved,
    hasDraft: draft.text !== null,
    setText: (next) => (next === saved ? discard() : setDraft({ text: next })),
    discard,
    discardSent: (sent) => {
      if (useSquadFieldDraftStore.getState().getDraft(key)?.text === sent) discard();
    },
  };
}

/** Add-member dialog: the picked target and the typed role. */
export interface SquadAddMemberDraft {
  target: { type: "agent" | "member"; id: string; name: string } | null;
  role: string;
}

const EMPTY_ADD_MEMBER_DRAFT: SquadAddMemberDraft = { target: null, role: "" };

export const useSquadAddMemberDraftStore = createKeyedDraftStore<SquadAddMemberDraft>({
  storageKey: "multica_squad_add_member_drafts",
  emptyData: EMPTY_ADD_MEMBER_DRAFT,
  hasMeaningful: (d) => d.target !== null || d.role !== "",
});

export function useSquadAddMemberDraft(squadId: string) {
  return usePersistedDraft(useSquadAddMemberDraftStore, squadId, EMPTY_ADD_MEMBER_DRAFT);
}
