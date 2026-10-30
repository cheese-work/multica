// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { AgentRuntime, MemberWithUser, RuntimePause } from "@multica/core/types";
import {
  deriveMachinePauseState,
  endedPauseNotice,
  pauseRightsHolders,
  pauseTarget,
  remainingParts,
} from "./machine-pause-state";

const NOW = Date.parse("2026-10-30T07:00:00Z");
const MIN = 60_000;

function runtime(overrides: Partial<AgentRuntime> = {}): AgentRuntime {
  return {
    id: "rt-1",
    workspace_id: "ws-1",
    daemon_id: "daemon-1",
    name: "Claude (x99)",
    runtime_mode: "local",
    provider: "claude",
    launch_header: "",
    status: "online",
    device_info: "",
    metadata: {},
    owner_id: "user-owner",
    visibility: "private",
    last_seen_at: null,
    created_at: "",
    updated_at: "",
    pause: null,
    daemon_running_tasks: 0,
    ...overrides,
  };
}

function pause(overrides: Partial<RuntimePause> = {}): RuntimePause {
  return {
    reason: "maintenance",
    paused_by: "user-owner",
    paused_at: new Date(NOW - 12 * MIN).toISOString(),
    expires_at: new Date(NOW + 108 * MIN).toISOString(),
    ...overrides,
  };
}

const derive = (runtimes: AgentRuntime[], over: { online?: boolean; fallbackTasks?: number; now?: number } = {}) =>
  deriveMachinePauseState(runtimes, { now: NOW, online: true, fallbackTasks: 0, ...over });

describe("deriveMachinePauseState", () => {
  it("running: not paused, counts the daemon's tasks", () => {
    expect(derive([runtime({ daemon_running_tasks: 3 })])).toMatchObject({
      phase: "running", reason: null, tasks: 3, online: true, remainingMs: null, endingSoon: false,
    });
  });

  it("pausing: paused with tasks left is draining, with the time left", () => {
    expect(derive([runtime({ pause: pause(), daemon_running_tasks: 3 })])).toMatchObject({
      phase: "draining", reason: "maintenance", tasks: 3, remainingMs: 108 * MIN, endingSoon: false,
    });
  });

  it("drained: paused with no tasks left is safe to shut down", () => {
    expect(derive([runtime({ pause: pause(), daemon_running_tasks: 0 })])).toMatchObject({
      phase: "drained", tasks: 0,
    });
  });

  it("offline: keeps the pause and the stale count", () => {
    const state = derive([runtime({ status: "offline", pause: pause({ reason: "incident" }), daemon_running_tasks: 2 })], {
      online: false,
    });
    expect(state).toMatchObject({ phase: "draining", reason: "incident", tasks: 2, online: false });
  });

  it("offline and not paused stays running-phase with online false", () => {
    expect(derive([runtime({ status: "offline" })], { online: false })).toMatchObject({ phase: "running", online: false });
  });

  it("ending soon: under five minutes left", () => {
    const state = derive([runtime({ pause: pause({ expires_at: new Date(NOW + 4 * MIN).toISOString() }) })]);
    expect(state).toMatchObject({ endingSoon: true, remainingMs: 4 * MIN });
  });

  it("exactly five minutes left is not ending soon", () => {
    const state = derive([runtime({ pause: pause({ expires_at: new Date(NOW + 5 * MIN).toISOString() }) })]);
    expect(state.endingSoon).toBe(false);
  });

  it("ended: a pause past its expiry counts as not paused before the server refetch", () => {
    const state = derive([runtime({ pause: pause({ expires_at: new Date(NOW - 1).toISOString() }), daemon_running_tasks: 1 })]);
    expect(state).toMatchObject({ phase: "running", reason: null, tasks: 1, remainingMs: null });
  });

  it("keeps a pause with no usable expiry paused and omits the time", () => {
    const state = derive([runtime({ pause: pause({ expires_at: "" }) })]);
    expect(state).toMatchObject({ phase: "drained", remainingMs: null, endingSoon: false });
  });

  it("falls back to the task snapshot when the server sent no count", () => {
    expect(derive([runtime({ daemon_running_tasks: null, pause: pause() })], { fallbackTasks: 2 })).toMatchObject({
      phase: "draining", tasks: 2,
    });
  });

  it("reads the pause from any runtime of the machine and takes the highest count", () => {
    const state = derive([
      runtime({ id: "a", daemon_running_tasks: 1 }),
      runtime({ id: "b", pause: pause(), daemon_running_tasks: 4 }),
    ]);
    expect(state).toMatchObject({ phase: "draining", tasks: 4 });
  });
});

describe("endedPauseNotice", () => {
  it("returns the last pause once it expired and the machine is running again", () => {
    const last = pause({ expires_at: new Date(NOW - MIN).toISOString() });
    expect(endedPauseNotice(last, derive([runtime()]), NOW)).toBe(last);
  });

  it("returns null when the pause was lifted early (Resume)", () => {
    expect(endedPauseNotice(pause(), derive([runtime()]), NOW)).toBeNull();
  });

  it("returns null while a pause is live, or when none was seen", () => {
    const live = derive([runtime({ pause: pause() })]);
    expect(endedPauseNotice(pause({ expires_at: new Date(NOW - MIN).toISOString() }), live, NOW)).toBeNull();
    expect(endedPauseNotice(null, derive([runtime()]), NOW)).toBeNull();
  });

  it("returns null for a pause with no usable expiry", () => {
    expect(endedPauseNotice(pause({ expires_at: "" }), derive([runtime()]), NOW)).toBeNull();
  });
});

describe("remainingParts", () => {
  it.each([
    [108 * MIN, { hours: 1, minutes: 48 }],
    [120 * MIN, { hours: 2, minutes: 0 }],
    [4 * MIN, { hours: 0, minutes: 4 }],
    [4 * MIN - 1, { hours: 0, minutes: 4 }],
    [1, { hours: 0, minutes: 1 }],
  ])("%d ms rounds up to whole minutes", (ms, expected) => {
    expect(remainingParts(ms)).toEqual(expected);
  });
});

describe("pauseTarget", () => {
  it("lets the machine owner pause through their own runtime, online first", () => {
    const target = pauseTarget(
      [runtime({ id: "other", owner_id: "user-b" }), runtime({ id: "off", status: "offline" }), runtime({ id: "on" })],
      "user-owner",
      "member",
    );
    expect(target?.id).toBe("on");
  });

  it("lets the workspace owner pause through any runtime of the machine", () => {
    expect(pauseTarget([runtime({ id: "x", owner_id: "user-b" })], "user-ws-owner", "owner")?.id).toBe("x");
  });

  it("gives an admin who owns nothing on the machine no target", () => {
    expect(pauseTarget([runtime({ owner_id: "user-b" })], "user-admin", "admin")).toBeNull();
  });

  it("gives no target without a signed-in user or any runtime", () => {
    expect(pauseTarget([runtime()], undefined, "owner")).toBeNull();
    expect(pauseTarget([], "user-owner", "owner")).toBeNull();
  });

  it("an admin who owns a runtime on the machine may pause", () => {
    expect(pauseTarget([runtime({ owner_id: "user-admin" })], "user-admin", "admin")?.id).toBe("rt-1");
  });
});

describe("pauseRightsHolders", () => {
  const member = (user_id: string, name: string, role: MemberWithUser["role"]): MemberWithUser => ({
    id: user_id, workspace_id: "ws-1", user_id, role, created_at: "", name, email: "", avatar_url: null,
  });

  it("names the machine owner, then workspace owners, once each", () => {
    const names = pauseRightsHolders(
      [runtime({ owner_id: "dana" })],
      [member("cheese", "Cheese", "owner"), member("dana", "Dana", "member"), member("eve", "Eve", "admin")],
    );
    expect(names).toEqual(["Dana", "Cheese"]);
  });

  it("does not repeat a person who is both", () => {
    expect(pauseRightsHolders([runtime({ owner_id: "cheese" })], [member("cheese", "Cheese", "owner")])).toEqual(["Cheese"]);
  });

  it("skips owners that are not in the member list", () => {
    expect(pauseRightsHolders([runtime({ owner_id: "ghost" })], [])).toEqual([]);
  });
});
