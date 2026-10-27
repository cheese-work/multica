// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest";
import { useRelationsFilterStore } from "./relations-filter-store";

describe("relations filter store", () => {
  beforeEach(() => { localStorage.clear(); useRelationsFilterStore.setState({ hidden: ["mention"] }); });

  it("hides mentions by default for a fresh user", () => {
    expect(useRelationsFilterStore.getInitialState().hidden).toEqual(["mention"]);
  });

  it("persists a toggle across reload", async () => {
    useRelationsFilterStore.getState().toggle("mention");
    useRelationsFilterStore.getState().toggle("wakeup");
    const saved = localStorage.getItem("multica_relations_filter")!;
    useRelationsFilterStore.setState({ hidden: [] });
    localStorage.setItem("multica_relations_filter", saved);
    await useRelationsFilterStore.persist.rehydrate();
    expect(useRelationsFilterStore.getState().hidden).toEqual(["wakeup"]);
  });
});
