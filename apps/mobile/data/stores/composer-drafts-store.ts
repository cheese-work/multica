/**
 * Per-entity drafts of the message composer: comment text, reply target,
 * @mention chips and uploaded attachments. Kept in memory so a composer that
 * unmounts (route change, scroll out of a list) finds its draft again, and
 * mirrored to disk (debounced) through `draft-persistence` so it also
 * survives an app cold start. Disk entries expire after 30 days, are scoped
 * per user + workspace, and are deleted on logout.
 *
 * Keys: `comment:<issueId>` and `chat:<sessionId | __new__>`. Chat input
 * text is NOT stored here: `chat-drafts-store` owns it.
 *
 * Disk is empty until `hydrate(scope)` runs (from the workspace layout);
 * writes made before that stay in memory only, and win over the disk copy.
 *
 * Send lifecycle: `beginSend` freezes the disk entry at the submitted draft
 * while the composer empties its input. `finishSend` writes whatever the
 * draft holds then: nothing after a success, the restored snapshot after a
 * failure. A kill mid-send therefore restores the submitted draft.
 *
 * Only completed uploads are written to disk. An upload still running when
 * the app dies is lost and has to be picked again.
 */
import { create } from "zustand";
import type { ComposerAttachmentItem } from "../../components/issue/composer-attachment-row";
import { draftPersistence, registerDraftCleanup } from "../draft-persistence";
import type { MentionChipDraft } from "./mention-draft-store";
import type { ReplyTarget } from "./reply-target-store";

export interface ComposerDraft {
  text: string;
  mentions: MentionChipDraft[];
  attachments: ComposerAttachmentItem[];
  reply: ReplyTarget | null;
}

const DISK_PREFIX = "composer:";
const EMPTY: ComposerDraft = {
  text: "",
  mentions: [],
  attachments: [],
  reply: null,
};

interface State {
  drafts: Record<string, ComposerDraft>;
  /** Scope (user + workspace) the drafts belong to; null before `hydrate`. */
  scopeId: string | null;
  /** True once the disk drafts of `scopeId` are merged into memory. */
  hydrated: boolean;
  patch: (key: string, patch: Partial<ComposerDraft>) => void;
  /** Functional update for async upload results that land after unmount. */
  patchAttachments: (
    key: string,
    update: (prev: ComposerAttachmentItem[]) => ComposerAttachmentItem[],
  ) => void;
  /** The composer is about to empty its input to submit. */
  beginSend: (key: string) => void;
  /** The send settled; the composer already emptied or restored the draft. */
  finishSend: (key: string) => void;
  hydrate: (scope: string) => Promise<void>;
  /** Empty memory and detach from disk (logout). */
  reset: () => void;
}

let scope: string | null = null;
// Keys with an open send: their disk entry stays at the submitted draft.
const sending = new Set<string>();
// Keys written since the current hydrate() began; the disk copy must not
// overwrite them.
let touchedDuringHydrate = new Set<string>();

function isEmpty(draft: ComposerDraft): boolean {
  return (
    draft.text === "" &&
    draft.mentions.length === 0 &&
    draft.attachments.length === 0 &&
    draft.reply === null
  );
}

function persist(key: string, draft: ComposerDraft | undefined) {
  if (scope === null || sending.has(key)) return;
  if (!draft || isEmpty(draft)) {
    draftPersistence.remove(scope, DISK_PREFIX + key);
    return;
  }
  draftPersistence.save(scope, DISK_PREFIX + key, {
    ...draft,
    attachments: draft.attachments.filter((a) => a.status === "completed"),
  });
}

function isDraft(value: unknown): value is ComposerDraft {
  if (typeof value !== "object" || value === null) return false;
  const d = value as Partial<ComposerDraft>;
  return (
    typeof d.text === "string" &&
    Array.isArray(d.mentions) &&
    Array.isArray(d.attachments) &&
    (d.reply === null || typeof d.reply === "object")
  );
}

export const useComposerDraftsStore = create<State>((set, get) => {
  const write = (key: string, next: ComposerDraft) => {
    touchedDuringHydrate.add(key);
    const drafts = { ...get().drafts };
    if (isEmpty(next)) delete drafts[key];
    else drafts[key] = next;
    set({ drafts });
    persist(key, drafts[key]);
  };

  return {
    drafts: {},
    scopeId: null,
    hydrated: false,
    patch: (key, patch) => write(key, { ...(get().drafts[key] ?? EMPTY), ...patch }),
    patchAttachments: (key, update) =>
      get().patch(key, {
        attachments: update(get().drafts[key]?.attachments ?? []),
      }),
    beginSend: (key) => {
      persist(key, get().drafts[key]);
      sending.add(key);
    },
    finishSend: (key) => {
      sending.delete(key);
      persist(key, get().drafts[key]);
    },
    hydrate: async (workspaceScope) => {
      if (scope === workspaceScope) return;
      void draftPersistence.flush();
      scope = workspaceScope;
      touchedDuringHydrate = new Set();
      sending.clear();
      set({ drafts: {}, scopeId: workspaceScope, hydrated: false });
      const onDisk = await draftPersistence.loadAll<unknown>(
        workspaceScope,
        DISK_PREFIX,
      );
      // A newer hydrate() or a logout reset won while we were reading.
      if (scope !== workspaceScope) return;
      const restored: Record<string, ComposerDraft> = {};
      for (const [diskKey, value] of Object.entries(onDisk)) {
        const key = diskKey.slice(DISK_PREFIX.length);
        if (isDraft(value) && !touchedDuringHydrate.has(key)) {
          restored[key] = value;
        }
      }
      set({ drafts: { ...restored, ...get().drafts }, hydrated: true });
    },
    reset: () => {
      scope = null;
      sending.clear();
      touchedDuringHydrate = new Set();
      set({ drafts: {}, scopeId: null, hydrated: false });
    },
  };
});

registerDraftCleanup(() => useComposerDraftsStore.getState().reset());
