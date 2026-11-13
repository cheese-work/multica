"use client";

import { useEffect, useRef } from "react";
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

// Keys whose saved value is about to change. From the moment a save starts until
// the `saved` prop moves off the value it had then, "typing the saved text" is
// not "back to unchanged": the prop is stale, the server will hold the sent
// text. The prop lags the save (the refetch is not awaited), and a debounced
// editor can deliver its last update on either side of the save finishing.
const awaitingSaved = new Map<string, { savedAtSend: string; inFlight: boolean }>();

/**
 * Bind a text field to its draft. `text` is the draft when one exists, else
 * the saved value; typing the saved value back drops the draft, except while
 * the saved value is awaiting a save (see `awaitingSaved`).
 */
export function useSquadFieldDraft(
  key: string,
  saved: string,
): {
  text: string;
  hasDraft: boolean;
  setText: (next: string) => void;
  discard: () => void;
  /**
   * Run `save` for `sent`. On success the draft is dropped unless the user
   * typed something else meanwhile. On failure it is kept, unless it only
   * equals the saved value. Rethrows the save's error.
   */
  commit: (sent: string, save: () => Promise<void>) => Promise<void>;
} {
  const [draft, setDraft, discard] = usePersistedDraft(
    useSquadFieldDraftStore,
    key,
    EMPTY_FIELD_DRAFT,
  );
  const draftText = () => useSquadFieldDraftStore.getState().getDraft(key)?.text;
  const latestSaved = useRef(saved);
  latestSaved.current = saved;

  // The saved value moved: the awaited save landed (or someone else saved).
  // Release the key, and drop a draft that now only repeats the saved value.
  useEffect(() => {
    const wait = awaitingSaved.get(key);
    if (!wait || wait.inFlight || saved === wait.savedAtSend) return;
    awaitingSaved.delete(key);
    if (draftText() === saved) discard();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, saved]);

  return {
    text: draft.text ?? saved,
    hasDraft: draft.text !== null,
    setText: (next) =>
      next === saved && !awaitingSaved.has(key) ? discard() : setDraft({ text: next }),
    discard,
    commit: async (sent, save) => {
      const wait = { savedAtSend: latestSaved.current, inFlight: true };
      awaitingSaved.set(key, wait);
      try {
        await save();
        if (draftText() === sent) discard();
      } catch (e) {
        awaitingSaved.delete(key);
        if (draftText() === latestSaved.current) discard();
        throw e;
      } finally {
        wait.inFlight = false;
      }
      // The refetch may already have landed while the save was in flight.
      if (latestSaved.current !== wait.savedAtSend && awaitingSaved.get(key) === wait) {
        awaitingSaved.delete(key);
      }
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
