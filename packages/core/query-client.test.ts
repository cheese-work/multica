// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { QueryObserver } from "@tanstack/react-query";
import { createQueryClient } from "./query-client";

function setVisibility(state: "visible" | "hidden") {
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => state });
  document.dispatchEvent(new Event("visibilitychange"));
}

afterEach(() => setVisibility("visible"));

describe("createQueryClient hidden-tab invalidation", () => {
  async function setup() {
    const qc = createQueryClient();
    const fetcher = vi.fn().mockResolvedValue("v");
    // active observer so invalidate would normally refetch
    const o = new QueryObserver(qc, { queryKey: ["a"], queryFn: fetcher });
    const unsub = o.subscribe(() => {});
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));
    return { qc, fetcher, unsub };
  }

  it("visible tab refetches on invalidate", async () => {
    const { qc, fetcher, unsub } = await setup();
    await qc.invalidateQueries({ queryKey: ["a"] });
    expect(fetcher).toHaveBeenCalledTimes(2);
    unsub();
  });

  it("hidden tab defers refetch until visible, then fetches once", async () => {
    const { qc, fetcher, unsub } = await setup();
    setVisibility("hidden");
    await qc.invalidateQueries({ queryKey: ["a"] });
    await qc.invalidateQueries({ queryKey: ["a"] });
    expect(fetcher).toHaveBeenCalledTimes(1);
    setVisibility("visible");
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
    // catch-up cleared the obligation: a later patch + focus must not refetch
    qc.setQueryData(["a"], "patched");
    setVisibility("hidden");
    setVisibility("visible");
    await new Promise((r) => setTimeout(r, 20));
    expect(fetcher).toHaveBeenCalledTimes(2);
    unsub();
  });

  it("hidden invalidation survives a later partial setQueryData", async () => {
    const { qc, fetcher, unsub } = await setup();
    setVisibility("hidden");
    await qc.invalidateQueries({ queryKey: ["a"] });
    qc.setQueryData(["a"], "patched"); // clears isInvalidated but is not authoritative
    expect(fetcher).toHaveBeenCalledTimes(1);
    setVisibility("visible");
    await vi.waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
    // obligation satisfied: a second visible event must not refetch again
    setVisibility("hidden");
    setVisibility("visible");
    await new Promise((r) => setTimeout(r, 20));
    expect(fetcher).toHaveBeenCalledTimes(2);
    unsub();
  });
});
