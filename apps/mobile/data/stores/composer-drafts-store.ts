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
 * Disk is empty until `hydrate(scope)` runs (from the workspace layout).
 * Edits made while it loads are replayed on top of the disk draft per field:
 * a reply replaces the saved one, mentions and attachments are added to the
 * saved ones. Each mentions patch is the whole live list, so only the last
 * one is replayed (a mention picked and removed again stays removed).
 * `text` is the exception. A patch replaces it whole and would
 * overwrite the saved text, so a `text` patch is ignored until `hydrated`
 * and the composer keeps its input read-only until then.
 *
 * Scope-bound writes: a caller that outlives a workspace switch (an upload,
 * a send that settles late) passes the scope it started in as `forScope`; a
 * write for a scope that was left since is dropped instead of landing in the
 * new workspace.
 *
 * Send lifecycle: `beginSend` freezes the disk entry at the submitted draft
 * while the composer empties its input and returns the send's scope.
 * `finishSend` writes whatever the draft holds then: nothing after a
 * success, the restored snapshot after a failure. A send that settles after
 * its scope was left is settled against that scope's own disk entry. A kill
 * mid-send therefore restores the submitted draft.
 *
 * Only completed uploads are written to disk. An upload still running when
 * the app dies is lost and has to be picked again.
 */
import { create } from "zustand";
import type { ComposerAttachmentItem } from "../../components/issue/composer-attachment-row";
import { mergeRestoredText } from "../../lib/merge-restored-text";
import {
  draftPersistence,
  registerDraftCleanup,
  registerScopeCleanup,
} from "../draft-persistence";
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

type DraftOp = (draft: ComposerDraft) => ComposerDraft;

interface State {
  drafts: Record<string, ComposerDraft>;
  /** Scope (user + workspace) the drafts belong to; null before `hydrate`. */
  scopeId: string | null;
  /** True once the disk drafts of `scopeId` are merged into memory. */
  hydrated: boolean;
  /** `forScope` is the scope the caller rendered in; see the header. A `text`
   *  patch is ignored until `hydrated`. */
  patch: (
    key: string,
    patch: Partial<ComposerDraft>,
    forScope?: string | null,
  ) => void;
  /** Functional update for async upload results that land after unmount. */
  patchAttachments: (
    key: string,
    update: (prev: ComposerAttachmentItem[]) => ComposerAttachmentItem[],
    forScope?: string | null,
  ) => void;
  /** The composer is about to empty its input to submit. Returns the scope
   *  the send belongs to; pass it to `finishSend`. */
  beginSend: (key: string) => string | null;
  /** The send settled. When its scope is still current the composer already
   *  emptied or restored the draft; otherwise the scope's disk entry is
   *  settled here. */
  finishSend: (key: string, forScope: string | null, ok: boolean) => void;
  hydrate: (scope: string) => Promise<void>;
  /** A workspace was deleted: if it is the current scope, detach from it so
   *  a pending load and late writes cannot bring its drafts back. */
  dropScope: (scope: string) => void;
  /** Empty memory and detach from disk (logout). */
  reset: () => void;
}

interface OpenSend {
  scope: string;
  key: string;
  submitted: ComposerDraft | undefined;
  /** Draft of the key at the moment its scope was left. */
  parked: ComposerDraft | undefined;
}

let scope: string | null = null;
// Edits made while the disk drafts load, replayed on top of them.
let loading = false;
let loadingOps: { key: string; op: DraftOp }[] = [];
// Latest mentions patch per key made while loading.
let loadingMentions = new Map<string, MentionChipDraft[]>();
// Sends that have not settled. While one is open its disk entry stays at the
// submitted draft.
const sending = new Map<string, OpenSend>();

const sendId = (forScope: string, key: string) => `${forScope}\n${key}`;

function isEmpty(draft: ComposerDraft): boolean {
  return (
    draft.text === "" &&
    draft.mentions.length === 0 &&
    draft.attachments.length === 0 &&
    draft.reply === null
  );
}

function writeDisk(forScope: string, key: string, draft: ComposerDraft | undefined) {
  if (!draft || isEmpty(draft)) {
    draftPersistence.remove(forScope, DISK_PREFIX + key);
    return;
  }
  draftPersistence.save(forScope, DISK_PREFIX + key, {
    ...draft,
    attachments: draft.attachments.filter((a) => a.status === "completed"),
  });
}

function persist(key: string, draft: ComposerDraft | undefined) {
  if (scope === null || loading || sending.has(sendId(scope, key))) return;
  writeDisk(scope, key, draft);
}

function unionMentions(
  saved: MentionChipDraft[],
  added: MentionChipDraft[],
): MentionChipDraft[] {
  const isNew = (m: MentionChipDraft) =>
    !saved.some((x) => x.type === m.type && x.id === m.id);
  return [...saved, ...added.filter(isNew)];
}

// A failed send of a scope that was left: the submitted draft comes back,
// followed by what the user typed there afterwards.
function mergeDrafts(
  submitted: ComposerDraft,
  parked: ComposerDraft | undefined,
): ComposerDraft {
  if (!parked) return submitted;
  return {
    text: mergeRestoredText(submitted.text, parked.text),
    mentions: unionMentions(submitted.mentions, parked.mentions),
    attachments: [...submitted.attachments, ...parked.attachments],
    reply: parked.reply ?? submitted.reply,
  };
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
  const write = (
    key: string,
    op: DraftOp,
    forScope?: string | null,
    replay: DraftOp = op,
  ) => {
    if (forScope !== undefined && forScope !== scope) return;
    if (loading) loadingOps.push({ key, op: replay });
    const next = op(get().drafts[key] ?? EMPTY);
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
    patch: (key, patch, forScope) => {
      if (!loading) {
        write(key, (draft) => ({ ...draft, ...patch }), forScope);
        return;
      }
      if (forScope !== undefined && forScope !== scope) return;
      const { text: _ignored, mentions, ...rest } = patch;
      if (mentions) loadingMentions.set(key, mentions);
      write(
        key,
        (draft) => ({ ...draft, ...rest, ...(mentions && { mentions }) }),
        forScope,
        (draft) => ({
          ...draft,
          ...rest,
          mentions: mentions
            ? unionMentions(draft.mentions, loadingMentions.get(key) ?? mentions)
            : draft.mentions,
        }),
      );
    },
    patchAttachments: (key, update, forScope) =>
      write(
        key,
        (draft) => ({ ...draft, attachments: update(draft.attachments) }),
        forScope,
      ),
    beginSend: (key) => {
      if (scope === null) return null;
      const submitted = get().drafts[key];
      persist(key, submitted);
      sending.set(sendId(scope, key), {
        scope,
        key,
        submitted,
        parked: undefined,
      });
      return scope;
    },
    finishSend: (key, forScope, ok) => {
      if (forScope === null) return;
      const open = sending.get(sendId(forScope, key));
      if (!open) return;
      sending.delete(sendId(forScope, key));
      if (forScope === scope) {
        persist(key, get().drafts[key]);
        return;
      }
      const failed = open.submitted
        ? mergeDrafts(open.submitted, open.parked)
        : open.parked;
      writeDisk(forScope, key, ok ? open.parked : failed);
    },
    hydrate: async (workspaceScope) => {
      if (scope === workspaceScope) return;
      void draftPersistence.flush();
      // Park open sends of the scope being left (see OpenSend).
      for (const open of sending.values()) {
        if (open.scope === scope) open.parked = get().drafts[open.key];
      }
      scope = workspaceScope;
      loading = true;
      loadingOps = [];
      loadingMentions = new Map();
      set({ drafts: {}, scopeId: workspaceScope, hydrated: false });
      const onDisk = await draftPersistence.loadAll<unknown>(
        workspaceScope,
        DISK_PREFIX,
      );
      // A newer hydrate() or a logout reset won while we were reading.
      if (scope !== workspaceScope) return;
      const drafts: Record<string, ComposerDraft> = {};
      for (const [diskKey, value] of Object.entries(onDisk)) {
        if (isDraft(value)) drafts[diskKey.slice(DISK_PREFIX.length)] = value;
      }
      // A send of this scope that is still open: its submitted draft is on
      // disk but already in flight, so only what the user typed after it
      // (parked when the scope was left) shows up.
      for (const open of sending.values()) {
        if (open.scope !== workspaceScope) continue;
        if (open.parked) drafts[open.key] = open.parked;
        else delete drafts[open.key];
        open.parked = undefined;
      }
      for (const { key, op } of loadingOps) {
        const next = op(drafts[key] ?? EMPTY);
        if (isEmpty(next)) delete drafts[key];
        else drafts[key] = next;
      }
      const touched = new Set(loadingOps.map((o) => o.key));
      loading = false;
      loadingOps = [];
      loadingMentions = new Map();
      set({ drafts, hydrated: true });
      touched.forEach((key) => persist(key, drafts[key]));
    },
    dropScope: (deleted) => {
      for (const [id, open] of sending) {
        if (open.scope === deleted) sending.delete(id);
      }
      if (scope === deleted) get().reset();
    },
    reset: () => {
      scope = null;
      loading = false;
      loadingOps = [];
      loadingMentions = new Map();
      sending.clear();
      set({ drafts: {}, scopeId: null, hydrated: false });
    },
  };
});

registerDraftCleanup(() => useComposerDraftsStore.getState().reset());
registerScopeCleanup((deleted) =>
  useComposerDraftsStore.getState().dropScope(deleted),
);
