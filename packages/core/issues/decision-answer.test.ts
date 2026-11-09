// @vitest-environment node

import { describe, expect, it } from "vitest";
import { batchCoverAck } from "./decision-answer";

describe("batchCoverAck", () => {
  // Same vector as the server test (decision_batch_card_test.go): the two sides must agree.
  it("matches the server digest and ignores the order of the covered ids", async () => {
    const batch = "00000000-0000-4000-8000-0000000000b1";
    const covered = ["00000000-0000-4000-8000-0000000000c2", "00000000-0000-4000-8000-0000000000c1"];
    const want = "012ee7331cfe77d20ca96126e0cae16df3acabee3149e329bb3054f42d0211e1";
    expect(await batchCoverAck(batch, covered, "Option B")).toBe(want);
    expect(await batchCoverAck(batch, [...covered].reverse(), "Option B")).toBe(want);
  });

  it("changes with the option and with the covered set", async () => {
    const batch = "00000000-0000-4000-8000-0000000000b1";
    const base = await batchCoverAck(batch, ["a"], "Option B");
    expect(await batchCoverAck(batch, ["a"], "Option A")).not.toBe(base);
    expect(await batchCoverAck(batch, ["a", "b"], "Option B")).not.toBe(base);
  });
});
