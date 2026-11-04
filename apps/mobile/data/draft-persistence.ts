/**
 * Disk layer for unsent input (drafts) on mobile.
 *
 * Why not `expo-secure-store`: it is for secrets (size limit, Keychain cost
 * per write). Drafts are plain text written often, so they live in
 * AsyncStorage instead.
 *
 * One AsyncStorage key per draft: `multica_draft:v1:<scope>:<key>`, where
 * scope is the workspace id and key is the caller's entity key (for example
 * `chat:<sessionId>`). Each value is `{ updatedAt, value }`; a draft older
 * than 30 days is dropped on read, like web comment drafts.
 *
 * Writes are debounced per draft (a keystroke must not hit disk) and all disk
 * operations are serialized, so a remove or logout wipe always lands after a
 * write that was already in flight. Disk errors are logged and swallowed: a
 * failed draft write must never break typing. The exceptions are `clearAll`
 * (logout) and `clearScope` (workspace deleted): they reject when the delete
 * fails so the caller can tell.
 */
import AsyncStorage from "@react-native-async-storage/async-storage";

export const DRAFT_TTL_MS = 30 * 24 * 60 * 60 * 1000;

const PREFIX = "multica_draft:v1:";
const DEFAULT_DEBOUNCE_MS = 500;

export interface DraftStorage {
  getItem(key: string): Promise<string | null>;
  setItem(key: string, value: string): Promise<void>;
  removeItem(key: string): Promise<void>;
  getAllKeys(): Promise<readonly string[]>;
  multiGet(keys: readonly string[]): Promise<readonly [string, string | null][]>;
  multiRemove(keys: readonly string[]): Promise<void>;
}

export interface DraftPersistence {
  /** Debounced write of `value` (any JSON value) under scope + key. */
  save(scope: string, key: string, value: unknown): void;
  /** Delete the draft and cancel its pending write. */
  remove(scope: string, key: string): void;
  /** All live drafts of one scope whose key starts with `keyPrefix`. */
  loadAll<T>(scope: string, keyPrefix: string): Promise<Record<string, T>>;
  /** Delete expired drafts of every scope. */
  sweepExpired(): Promise<void>;
  /** Write every pending draft now (call when the app is backgrounded). */
  flush(): Promise<void>;
  /** Workspace ids that have drafts on disk for `userId` (scope
   *  `<userId>:<workspaceId>`). */
  workspaceIds(userId: string): Promise<string[]>;
  /** Delete every draft of one scope and drop its pending writes. Rejects
   *  when the disk delete fails. */
  clearScope(scope: string): Promise<void>;
  /** Delete every draft of every scope and drop pending writes. Rejects when
   *  the disk delete fails. */
  clearAll(): Promise<void>;
}

interface Envelope {
  updatedAt: number;
  value: unknown;
}

function diskKey(scope: string, key: string): string {
  return `${PREFIX}${scope}:${key}`;
}

function parseEnvelope(raw: string | null): Envelope | null {
  if (raw === null) return null;
  try {
    const parsed = JSON.parse(raw) as Partial<Envelope> | null;
    if (!parsed || typeof parsed.updatedAt !== "number") return null;
    return { updatedAt: parsed.updatedAt, value: parsed.value };
  } catch {
    return null;
  }
}

export function createDraftPersistence(options: {
  storage: DraftStorage;
  now?: () => number;
  debounceMs?: number;
}): DraftPersistence {
  const { storage, now = Date.now, debounceMs = DEFAULT_DEBOUNCE_MS } = options;

  const pending = new Map<
    string,
    { timer: ReturnType<typeof setTimeout>; envelope: Envelope }
  >();
  let queue: Promise<unknown> = Promise.resolve();

  // The returned promise rejects only when `rethrow` is set. The queue itself
  // never rejects, so one failed operation cannot stall the ones after it.
  function enqueue(
    operation: () => Promise<void>,
    rethrow = false,
  ): Promise<void> {
    const run = async () => {
      try {
        await operation();
      } catch (err) {
        console.warn("[drafts] storage operation failed", err);
        if (rethrow) throw err;
      }
    };
    const result = queue.then(run);
    queue = result.catch(() => undefined);
    return result;
  }

  function write(key: string, envelope: Envelope): Promise<void> {
    return enqueue(() => storage.setItem(key, JSON.stringify(envelope)));
  }

  function expired(envelope: Envelope): boolean {
    return now() - envelope.updatedAt > DRAFT_TTL_MS;
  }

  return {
    save(scope, key, value) {
      const k = diskKey(scope, key);
      const existing = pending.get(k);
      if (existing) clearTimeout(existing.timer);
      const envelope: Envelope = { updatedAt: now(), value };
      const timer = setTimeout(() => {
        pending.delete(k);
        void write(k, envelope);
      }, debounceMs);
      pending.set(k, { timer, envelope });
    },

    remove(scope, key) {
      const k = diskKey(scope, key);
      const existing = pending.get(k);
      if (existing) clearTimeout(existing.timer);
      pending.delete(k);
      void enqueue(() => storage.removeItem(k));
    },

    async loadAll<T>(scope: string, keyPrefix: string) {
      const result: Record<string, T> = {};
      await enqueue(async () => {
        const base = diskKey(scope, "");
        const keys = (await storage.getAllKeys()).filter((k) =>
          k.startsWith(base + keyPrefix),
        );
        if (keys.length === 0) return;
        const stale: string[] = [];
        for (const [k, raw] of await storage.multiGet(keys)) {
          const envelope = parseEnvelope(raw);
          if (envelope && !expired(envelope)) {
            result[k.slice(base.length)] = envelope.value as T;
          } else {
            stale.push(k);
          }
        }
        if (stale.length > 0) await storage.multiRemove(stale);
      });
      return result;
    },

    sweepExpired: () =>
      enqueue(async () => {
        const keys = (await storage.getAllKeys()).filter((k) =>
          k.startsWith(PREFIX),
        );
        const stale: string[] = [];
        for (const [k, raw] of await storage.multiGet(keys)) {
          const envelope = parseEnvelope(raw);
          if (!envelope || expired(envelope)) stale.push(k);
        }
        if (stale.length > 0) await storage.multiRemove(stale);
      }),

    async flush() {
      const entries = [...pending.entries()];
      pending.clear();
      for (const [, { timer }] of entries) clearTimeout(timer);
      await Promise.all(entries.map(([k, { envelope }]) => write(k, envelope)));
      await queue;
    },

    async workspaceIds(userId) {
      const base = diskKey(userId, "");
      const ids = new Set<string>();
      await enqueue(async () => {
        for (const k of await storage.getAllKeys()) {
          if (k.startsWith(base)) ids.add(k.slice(base.length).split(":")[0]);
        }
      });
      return [...ids];
    },

    clearScope(scope) {
      const base = diskKey(scope, "");
      for (const [k, { timer }] of pending) {
        if (!k.startsWith(base)) continue;
        clearTimeout(timer);
        pending.delete(k);
      }
      return enqueue(async () => {
        const keys = (await storage.getAllKeys()).filter((k) =>
          k.startsWith(base),
        );
        if (keys.length > 0) await storage.multiRemove(keys);
      }, true);
    },

    clearAll() {
      for (const { timer } of pending.values()) clearTimeout(timer);
      pending.clear();
      return enqueue(async () => {
        const keys = (await storage.getAllKeys()).filter((k) =>
          k.startsWith(PREFIX),
        );
        if (keys.length > 0) await storage.multiRemove(keys);
      }, true);
    },
  };
}

/** The app-wide instance, backed by AsyncStorage. */
export const draftPersistence = createDraftPersistence({
  storage: AsyncStorage,
});

// Stores that hold drafts in memory register a reset here so logout empties
// them as well as the disk.
const cleanups = new Set<() => void>();

export function registerDraftCleanup(cleanup: () => void): void {
  cleanups.add(cleanup);
}

/** Logout: empty every in-memory draft store and delete all drafts on disk.
 *  Rejects when the disk delete fails; memory is already empty by then. */
export async function clearAllDrafts(): Promise<void> {
  cleanups.forEach((cleanup) => cleanup());
  await draftPersistence.clearAll();
}

const scopeCleanups = new Set<(scope: string) => void>();

/** Stores that hold drafts in memory register a hook here so a deleted
 *  workspace is dropped from memory as well as from disk. */
export function registerScopeCleanup(cleanup: (scope: string) => void): void {
  scopeCleanups.add(cleanup);
}

/** Workspace deleted: drop the drafts of one scope (user + workspace) from
 *  memory and disk. Other workspaces keep theirs. */
export async function clearScopeDrafts(scope: string): Promise<void> {
  scopeCleanups.forEach((cleanup) => cleanup(scope));
  await draftPersistence.clearScope(scope);
}

/** A workspace deleted while this device was offline never sent its
 *  `workspace:deleted` event. Given the user's settled, successfully fetched
 *  workspace list, drop the drafts of every other workspace of that user.
 *  Nothing happens without a list (failed or pending fetch). An empty list
 *  is a valid answer: the user's last workspace was deleted. */
export async function reconcileWorkspaceDrafts(
  userId: string,
  workspaces: readonly { id: string }[] | null | undefined,
): Promise<void> {
  if (!workspaces) return;
  const live = new Set(workspaces.map((w) => w.id));
  for (const id of await draftPersistence.workspaceIds(userId)) {
    if (!live.has(id)) await clearScopeDrafts(`${userId}:${id}`);
  }
}
