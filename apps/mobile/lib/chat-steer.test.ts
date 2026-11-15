import { describe, expect, it, vi } from "vitest";

import {
  FOLLOW_UP_MAX_VISIBLE_ROWS,
  FOLLOW_UP_ROW_HEIGHT,
  followUpListMaxHeight,
  presentSteer,
  steerQueuedTask,
} from "./chat-steer";

describe("presentSteer", () => {
  it.each(["dispatched", "running", "waiting_local_directory"])(
    "allows Steer on a capable %s head",
    (status) => {
      expect(presentSteer({ headStatus: status, headSupplement: "capable" })).toEqual({
        canSteer: true,
        reasonKey: null,
      });
    },
  );

  it("waits, never reads as unsupported, while the run is starting", () => {
    expect(presentSteer({ headStatus: "running", headSupplement: "starting" })).toEqual({
      canSteer: false,
      reasonKey: "steer.starting",
    });
  });

  it.each(["unsupported", undefined] as const)(
    "keeps the message queued with a reason when head support is %s",
    (headSupplement) => {
      expect(presentSteer({ headStatus: "running", headSupplement })).toEqual({
        canSteer: false,
        reasonKey: "steer.unsupported",
      });
    },
  );

  it("says Steer is not available yet while the head has not started", () => {
    expect(presentSteer({ headStatus: "queued", headSupplement: "capable" })).toEqual({
      canSteer: false,
      reasonKey: "steer.unavailable",
    });
  });

  it("blocks Steer with the permission reason when the agent cannot be run", () => {
    expect(
      presentSteer({ headStatus: "running", headSupplement: "capable", noPermission: true }),
    ).toEqual({ canSteer: false, reasonKey: "steer.no_permission" });
  });
});

describe("steerQueuedTask", () => {
  const deps = () => ({
    steer: vi.fn<(sessionId: string, taskId: string) => Promise<unknown>>().mockResolvedValue({}),
    refresh: vi.fn<(sessionId: string) => void>(),
  });

  it("steers the queued task, then refreshes the session", async () => {
    const d = deps();

    await expect(steerQueuedTask("s1", "t2", d)).resolves.toBe("steered");

    expect(d.steer).toHaveBeenCalledWith("s1", "t2");
    expect(d.refresh).toHaveBeenCalledWith("s1");
  });

  it("reports failure and still refreshes so the follow-up stays queued", async () => {
    const d = deps();
    d.steer.mockRejectedValueOnce(new Error("409 task_supplement_unsupported"));

    await expect(steerQueuedTask("s1", "t2", d)).resolves.toBe("failed");

    expect(d.refresh).toHaveBeenCalledWith("s1");
  });

  it("does not call the server for an optimistic task id", async () => {
    const d = deps();

    await expect(steerQueuedTask("s1", "optimistic-optimistic-1", d)).resolves.toBe("failed");

    expect(d.steer).not.toHaveBeenCalled();
  });
});

describe("followUpListMaxHeight", () => {
  const cap = FOLLOW_UP_MAX_VISIBLE_ROWS * FOLLOW_UP_ROW_HEIGHT;

  it("fits a short queue exactly", () => {
    expect(followUpListMaxHeight(1)).toBe(FOLLOW_UP_ROW_HEIGHT);
    expect(followUpListMaxHeight(FOLLOW_UP_MAX_VISIBLE_ROWS)).toBe(cap);
  });

  it("caps a long queue so the composer stays on screen", () => {
    expect(followUpListMaxHeight(FOLLOW_UP_MAX_VISIBLE_ROWS + 1)).toBe(cap);
    expect(followUpListMaxHeight(200)).toBe(cap);
  });
});
