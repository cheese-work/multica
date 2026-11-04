import { useSyncExternalStore } from "react";
import { getCurrentSlug } from "../platform/workspace-storage";
import { onWorkspaceDraftsCleared, registerDraftCleanup } from "./cleanup-registry";

/**
 * In-memory (never persisted) record of uploads still in flight per draft key. An upload
 * outlives the component that started it, so the component cannot be the one to remember it:
 * a card that scrolls out and back must still see "pending", and a late settle must write only
 * if the draft it belongs to was not sent, discarded, logged out or moved to another workspace.
 *
 * A ticket is valid while its key's generation and the session epoch are unchanged. It carries
 * the workspace it started in and settles there, whichever workspace is active by then. Logout,
 * Send, Discard and deleting that workspace's drafts invalidate it. The check is synchronous, so
 * it holds in the microtask right after an unmount.
 */
interface Entry {
  generation: number;
  pending: number;
}

export interface UploadTicket {
  key: string;
  slug: string | null;
  generation: number;
  epoch: number;
}

let entries = new Map<string, Entry>();
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
    emit();
  },
});

onWorkspaceDraftsCleared((slug) => {
  for (const id of [...entries.keys()]) if (id.startsWith(`${slug}\u0000`)) entries.delete(id);
  emit();
});

export function beginPendingUpload(key: string): UploadTicket {
  const slug = getCurrentSlug();
  const id = entryId(slug, key);
  const entry = entries.get(id) ?? { generation: ++lastGeneration, pending: 0 };
  entries.set(id, { ...entry, pending: entry.pending + 1 });
  emit();
  return { key, slug, generation: entry.generation, epoch };
}

/**
 * Ends an upload. `onCurrent` runs first, and only when nothing invalidated the ticket, so the
 * pending count never drops before the result is written. It must write into `ticket.slug`.
 */
export function settlePendingUpload(ticket: UploadTicket, onCurrent?: () => void): void {
  const id = entryId(ticket.slug, ticket.key);
  const entry = entries.get(id);
  if (ticket.epoch !== epoch || !entry || entry.generation !== ticket.generation) return;
  onCurrent?.();
  entries.set(id, { ...entry, pending: Math.max(0, entry.pending - 1) });
  emit();
}

/** Send, Discard: uploads still in flight for this key no longer belong to any draft. */
export function invalidatePendingUploads(key: string): void {
  const id = entryId(getCurrentSlug(), key);
  const entry = entries.get(id);
  if (!entry) return;
  entries.set(id, { generation: ++lastGeneration, pending: 0 });
  emit();
}

export function usePendingUploadCount(key: string): number {
  return useSyncExternalStore(
    (listener) => { listeners.add(listener); return () => { listeners.delete(listener); }; },
    () => entries.get(entryId(getCurrentSlug(), key))?.pending ?? 0,
    () => 0,
  );
}
