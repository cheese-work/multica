// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The module remembers a write that storage refused, so each test loads a fresh copy.
let getPushIntent: typeof import("./push-intent").getPushIntent;
let setPushIntent: typeof import("./push-intent").setPushIntent;

beforeEach(async () => {
  vi.resetModules();
  ({ getPushIntent, setPushIntent } = await import("./push-intent"));
});

afterEach(() => vi.unstubAllGlobals());

describe("push intent", () => {
  it("is unset until a choice is recorded", () => {
    vi.stubGlobal("localStorage", { getItem: () => null, setItem: vi.fn() });
    expect(getPushIntent()).toBeNull();
  });

  it("remembers the last choice", () => {
    const store = new Map<string, string>();
    vi.stubGlobal("localStorage", {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => void store.set(k, v),
    });
    setPushIntent("on");
    expect(getPushIntent()).toBe("on");
    setPushIntent("off");
    expect(getPushIntent()).toBe("off");
  });

  it("ignores a value it did not write", () => {
    vi.stubGlobal("localStorage", { getItem: () => "garbage" });
    expect(getPushIntent()).toBeNull();
  });

  it("treats unavailable storage as unset and never throws", () => {
    expect(getPushIntent()).toBeNull();
    vi.stubGlobal("localStorage", {
      getItem: () => {
        throw new Error("denied");
      },
      setItem: () => {
        throw new Error("denied");
      },
    });
    expect(getPushIntent()).toBeNull();
    expect(() => setPushIntent("on")).not.toThrow();
  });

  describe("when storage refuses the write", () => {
    const refusing = (stored: string | null) => ({
      getItem: () => stored,
      setItem: () => {
        throw new Error("QuotaExceededError");
      },
    });

    it("keeps an opt-out for this page even though storage still says on", () => {
      vi.stubGlobal("localStorage", refusing("on"));
      setPushIntent("off");
      expect(getPushIntent()).toBe("off");
    });

    it("keeps an opt-out when storage cannot be read at all", () => {
      vi.stubGlobal("localStorage", {
        getItem: () => {
          throw new Error("denied");
        },
        setItem: () => {
          throw new Error("denied");
        },
      });
      setPushIntent("off");
      expect(getPushIntent()).toBe("off");
    });

    it("lets a later successful write take over", () => {
      vi.stubGlobal("localStorage", refusing("on"));
      setPushIntent("off");
      const store = new Map<string, string>();
      vi.stubGlobal("localStorage", {
        getItem: (k: string) => store.get(k) ?? null,
        setItem: (k: string, v: string) => void store.set(k, v),
      });
      setPushIntent("on");
      expect(getPushIntent()).toBe("on");
      store.set("multica_push_device", "off");
      expect(getPushIntent()).toBe("off");
    });
  });
});
