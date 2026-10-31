// Whether this browser asked for push, kept across reloads. The browser's own
// subscription cannot say: after an opt-out that failed halfway it may still
// exist, and after a dropped renewal it is gone although the user wants push.
//
// Fail closed: only a stored "on" lets anything register this device again.
// "on" lives in IndexedDB, which the service worker (`public/sw.js`) reads too;
// keep the names below in step with it. An opt-out deletes that record, which
// needs no quota, and also sets a cookie both sides can read, so it holds even
// when the database refuses every write.

export type PushIntent = "on" | "off";

const DB_NAME = "multica-push";
const STORE = "device";
const KEY = "intent";
const OFF_COOKIE = "multica_push_off";
const ONE_YEAR = 60 * 60 * 24 * 365;
// Held for a whole opt-out and for each registration, here and in the worker
// (`public/sw.js`, same name), so they never interleave. Without Web Locks
// (older browsers) there is no ordering: an opt-out still runs, but nothing
// registers on its own (see withAutomaticLock).
const DEVICE_LOCK = "multica-push-device";

// A choice storage could not keep. It wins for this page until a write works.
let unsaved: PushIntent | null = null;

function openDb(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(DB_NAME, 1);
    request.onupgradeneeded = () => void request.result.createObjectStore(STORE);
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error);
  });
}

async function inStore<T>(mode: IDBTransactionMode, run: (store: IDBObjectStore) => IDBRequest<T>): Promise<T> {
  const db = await openDb();
  try {
    return await new Promise<T>((resolve, reject) => {
      const tx = db.transaction(STORE, mode);
      const request = run(tx.objectStore(STORE));
      if (mode === "readonly") request.onsuccess = () => resolve(request.result);
      else tx.oncomplete = () => resolve(request.result);
      tx.onabort = tx.onerror = () => reject(tx.error);
    });
  } finally {
    db.close();
  }
}

function writeOffCookie(maxAge: number): void {
  try {
    const secure = location.protocol === "https:" ? "; Secure" : "";
    document.cookie = `${OFF_COOKIE}=1; Path=/; Max-Age=${maxAge}; SameSite=Lax${secure}`;
  } catch {
    // cookies refused; the database record is the other half
  }
}

function hasOffCookie(): boolean {
  try {
    return document.cookie.split("; ").some((pair) => pair.startsWith(`${OFF_COOKIE}=`));
  } catch {
    return false;
  }
}

async function readStored(): Promise<PushIntent | null> {
  if (hasOffCookie()) return "off";
  try {
    return (await inStore("readonly", (store) => store.get(KEY))) === "on" ? "on" : null;
  } catch {
    return null;
  }
}

export async function getPushIntent(): Promise<PushIntent | null> {
  return unsaved ?? (await readStored());
}

/** Fail closed: true only for a positively stored "on". */
export async function isPushWanted(): Promise<boolean> {
  return (await getPushIntent()) === "on";
}

/** The opt-out: takes the lock where there is one, and always runs. */
export function withDeviceLock<T>(run: () => Promise<T>): Promise<T> {
  const locks = typeof navigator === "undefined" ? undefined : navigator.locks;
  // request() resolves with what the callback's promise resolves to.
  return locks ? (locks.request(DEVICE_LOCK, run) as Promise<T>) : run();
}

/**
 * The one guard for registration the user did not just ask for (app load,
 * recovery after the worker's message). It runs only where Web Locks orders it
 * against an opt-out, and returns `skipped` everywhere else, so a new automatic
 * caller cannot race an opt-out by forgetting the check. The switch in
 * Settings is an explicit action and does not use it.
 */
export function withAutomaticLock<T>(run: () => Promise<T>, skipped: T): Promise<T> {
  const locks = typeof navigator === "undefined" ? undefined : navigator.locks;
  return locks ? (locks.request(DEVICE_LOCK, run) as Promise<T>) : Promise.resolve(skipped);
}

export async function setPushIntent(intent: PushIntent): Promise<void> {
  if (intent === "on") {
    writeOffCookie(0);
    await inStore("readwrite", (store) => store.put("on", KEY)).catch(() => undefined);
  } else {
    writeOffCookie(ONE_YEAR);
    await inStore("readwrite", (store) => store.delete(KEY)).catch(() => undefined);
  }
  const stored = await readStored();
  unsaved = stored === intent || (intent === "off" && stored === null) ? null : intent;
}
