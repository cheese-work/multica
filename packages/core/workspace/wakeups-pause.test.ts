// @vitest-environment node
import { expect, it } from "vitest";
import { deriveWakeupsPause } from "./wakeups-pause";

it("reads the pause the server stamped and defaults to running", () => {
  expect(deriveWakeupsPause(null)).toEqual({ paused: false, by: "", at: "" });
  expect(deriveWakeupsPause({ settings: { wakeups_paused: "true" } })).toMatchObject({ paused: false });
  expect(
    deriveWakeupsPause({ settings: { wakeups_paused: true, wakeups_paused_by: "u", wakeups_paused_at: "2026-10-15T08:00:00Z" } }),
  ).toEqual({ paused: true, by: "u", at: "2026-10-15T08:00:00Z" });
});
