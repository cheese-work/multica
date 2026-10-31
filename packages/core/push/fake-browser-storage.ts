// Test double for the two stores push-intent.ts uses (IndexedDB and cookies).
// The state outlives a module reload, which is how a test models a fresh page.
// eslint-disable-next-line import-x/no-extraneous-dependencies -- test-only helper, never bundled
import { vi } from "vitest";

export type IdbMode = "ok" | "unavailable" | "read-only" | "no-space";
export type CookieMode = "ok" | "denied";

export interface FakeBrowserStorage {
  idb: Map<string, unknown>;
  cookies: Map<string, string>;
  idbMode: IdbMode;
  cookieMode: CookieMode;
}

export function stubBrowserStorage(initial: Partial<Pick<FakeBrowserStorage, "idbMode" | "cookieMode">> = {}) {
  const storage: FakeBrowserStorage = {
    idb: new Map(),
    cookies: new Map(),
    idbMode: "ok",
    cookieMode: "ok",
    ...initial,
  };

  const transaction = () => {
    const tx: Record<string, unknown> & { error?: unknown } = {};
    const request = (run: () => Error | undefined, result?: () => unknown) => {
      const req: Record<string, unknown> = {};
      queueMicrotask(() => {
        const failure = run();
        if (failure) {
          tx.error = failure;
          (tx.onabort as (() => void) | undefined)?.();
          return;
        }
        req.result = result?.();
        (req.onsuccess as (() => void) | undefined)?.();
        (tx.oncomplete as (() => void) | undefined)?.();
      });
      return req;
    };
    tx.objectStore = () => ({
      get: (key: string) => request(() => undefined, () => storage.idb.get(key)),
      put: (value: unknown, key: string) =>
        request(() => {
          if (storage.idbMode === "read-only" || storage.idbMode === "no-space") return new Error("QuotaExceededError");
          storage.idb.set(key, value);
          return undefined;
        }),
      delete: (key: string) =>
        request(() => {
          if (storage.idbMode === "read-only") return new Error("ReadOnlyError");
          storage.idb.delete(key);
          return undefined;
        }),
    });
    return tx;
  };

  vi.stubGlobal("indexedDB", {
    open: () => {
      const req: Record<string, unknown> = {};
      queueMicrotask(() => {
        if (storage.idbMode === "unavailable") {
          req.error = new Error("denied");
          (req.onerror as (() => void) | undefined)?.();
          return;
        }
        req.result = { createObjectStore: () => ({}), close: () => undefined, transaction };
        (req.onupgradeneeded as (() => void) | undefined)?.();
        (req.onsuccess as (() => void) | undefined)?.();
      });
      return req;
    },
  });

  vi.stubGlobal("location", { protocol: "https:" });
  vi.stubGlobal("document", {
    get cookie() {
      return [...storage.cookies].map(([k, v]) => `${k}=${v}`).join("; ");
    },
    set cookie(line: string) {
      if (storage.cookieMode === "denied") return;
      const [pair = "", ...attrs] = line.split(";").map((part) => part.trim());
      const [name = "", ...rest] = pair.split("=");
      const expired = attrs.some((a) => a.toLowerCase() === "max-age=0");
      if (expired) storage.cookies.delete(name);
      else storage.cookies.set(name, rest.join("="));
    },
  });

  return storage;
}
