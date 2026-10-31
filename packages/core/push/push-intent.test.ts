// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { getPushIntent, setPushIntent } from "./push-intent";

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
    expect(() => setPushIntent("off")).not.toThrow();
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
});
