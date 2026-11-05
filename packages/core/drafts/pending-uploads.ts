import { useSyncExternalStore } from "react";
import { getCurrentSlug, subscribeToCurrentSlug } from "../platform/workspace-storage";
import { currentDraftOrigin, registerDraftCleanup, type DraftOrigin } from "./cleanup-registry";

/**
 * In-memory (never persisted) record of uploads still in flight per draft key. An upload
 * outlives the component that started it, so the component cannot be the one to remember it:
 * a card that scrolls out and back must still see "pending", and a late settle must write only
 * if the draft it belongs to was not sent, discarded, logged out or deleted meanwhile.
 *
 * An upload belongs to the workspace it started in and is cancelled when another workspace
 * becomes active (Cheese decision, CHE-3521): nothing is ever written into a workspace that is
 * not the active one. The key then reports a notice until the next upload, Send or Discard.
 * Logout, Send and Discard invalidate a ticket too. The checks are synchronous, so they hold in
 * the microtask right after an unmount.
 */
interface Entry {
  slug: string | null;
  generation: number;
  pending: number;
  controllers: Set<AbortController>;
}

export interface UploadTicket {
  key: string;
  slug: string | null;
  generation: number;
  epoch: number;
  /** Where the settled result is written; stale once logout or a delete cleared its workspace. */
  origin: DraftOrigin;
  /** Aborted when the workspace changes. Pass it to the upload request. */
  signal: AbortSignal;
  controller: AbortController;
}

let entries = new Map<string, Entry>();
let cancelled = new Set<string>();
let epoch = 0;
// Never reused, so a ticket cannot match an entry created after its own was dropped.
let lastGeneration = 0;
const listeners = new Set<() => void>();

const emit = () => listeners.forEach((listener) => listener());
const entryId = (slug: string | null, key: string) => `${slug ?? ""}\u0000${key}`;

registerDraftCleanup({
  storageKey: "multica_pending_uploads",
  workspaceScoped: false,
  resetInMemory: () => {
    epoch++;
    entries = new Map();
    cancelled = new Set();
    emit();
  },
});

subscribeToCurrentSlug((slug) => {
  for (const [id, entry] of [...entries]) {
    if (entry.slug === slug) continue;
    if (entry.pending > 0) {
      entry.controllers.forEach((controller) => controller.abort());
      cancelled.add(id);
    }
    entries.delete(id);
  }
  emit();
});

export function beginPendingUpload(key: string): UploadTicket {
  const slug = getCurrentSlug();
  const id = entryId(slug, key);
  const entry = entries.get(id) ?? { slug, generation: ++lastGeneration, pending: 0, controllers: new Set() };
  const controller = new AbortController();
  entry.controllers.add(controller);
  entries.set(id, { ...entry, pending: entry.pending + 1 });
  cancelled.delete(id);
  emit();
  return {
    key,
    slug,
    generation: entry.generation,
    epoch,
    origin: currentDraftOrigin(slug),
    signal: controller.signal,
    controller,
  };
}

/**
 * Ends an upload. `onCurrent` runs first, and only when nothing invalidated the ticket and its
 * workspace is still the active one, so the pending count never drops before the result is written.
 * A result dropped because its workspace is no longer active leaves the cancelled notice.
 */
export function settlePendingUpload(ticket: UploadTicket, onCurrent?: () => void): void {
  const id = entryId(ticket.slug, ticket.key);
  const entry = entries.get(id);
  if (ticket.epoch !== epoch || !entry || entry.generation !== ticket.generation) return;
  if (ticket.slug === getCurrentSlug()) onCurrent?.();
  // Settled after the switch but before the slug notification ran: that notification will see
  // nothing pending, so the notice has to be recorded here.
  else cancelled.add(id);
  entry.controllers.delete(ticket.controller);
  entries.set(id, { ...entry, pending: Math.max(0, entry.pending - 1) });
  emit();
}

/** Send, Discard: uploads still in flight for this key no longer belong to any draft. */
export function invalidatePendingUploads(key: string): void {
  const id = entryId(getCurrentSlug(), key);
  const entry = entries.get(id);
  if (entry) entries.set(id, { ...entry, generation: ++lastGeneration, pending: 0, controllers: new Set() });
  cancelled.delete(id);
  emit();
}

const subscribe = (listener: () => void) => {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
};

export function usePendingUploadCount(key: string): number {
  return useSyncExternalStore(
    subscribe,
    () => entries.get(entryId(getCurrentSlug(), key))?.pending ?? 0,
    () => 0,
  );
}

/** True when an upload for this key was cancelled by a workspace switch and nothing has replaced it. */
export function useUploadCancelledNotice(key: string): boolean {
  return useSyncExternalStore(
    subscribe,
    () => cancelled.has(entryId(getCurrentSlug(), key)),
    () => false,
  );
}
