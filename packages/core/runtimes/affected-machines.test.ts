// @vitest-environment jsdom
import { beforeEach, describe, expect, it } from "vitest";
import { parseWithFallback } from "../api/schema";
import {
  AffectedMachinesSchema,
  useDismissedMachineEventsStore,
} from "./affected-machines";

const machine = {
  machine_owner_id: "11111111-1111-4111-8111-111111111111",
  daemon_id: "x99",
  device_name: "congvc-x99",
  state: "paused",
  reason: "incident",
  event_id: "paused:o/x99:1",
  expires_at: "2026-10-30T07:50:00Z",
  waiting_tasks: 4,
};

describe("affected machines response contract", () => {
  it("keeps a paused machine from a new server", () => {
    const parsed = AffectedMachinesSchema.parse({ machines: [machine] });
    expect(parsed.machines).toEqual([machine]);
  });

  it("reads an offline machine with no reason or expiry", () => {
    const parsed = AffectedMachinesSchema.parse({
      machines: [{ ...machine, state: "offline", reason: null, expires_at: null, event_id: "offline:o/x99:2" }],
    });
    expect(parsed.machines[0]).toMatchObject({ state: "offline", reason: null, expires_at: null });
  });

  it("drops a machine with an unknown state instead of guessing one", () => {
    const parsed = AffectedMachinesSchema.parse({
      machines: [{ ...machine, state: "melting" }, { ...machine, daemon_id: "ok" }],
    });
    expect(parsed.machines.map((m: { daemon_id: string }) => m.daemon_id)).toEqual(["ok"]);
  });

  it("falls back to no machines for a malformed body", () => {
    const fallback = { machines: [] };
    expect(parseWithFallback("nope", AffectedMachinesSchema, fallback, { endpoint: "test" })).toEqual(fallback);
    expect(AffectedMachinesSchema.parse({}).machines).toEqual([]);
  });
});

describe("dismissed machine events", () => {
  beforeEach(() => {
    useDismissedMachineEventsStore.setState({ dismissed: [] });
  });

  it("hides a dismissed event and shows a new event of the same machine", () => {
    const store = useDismissedMachineEventsStore.getState();
    store.dismiss(["paused:o/x99:1"]);
    const { dismissed } = useDismissedMachineEventsStore.getState();
    expect(dismissed).toContain("paused:o/x99:1");
    expect(dismissed).not.toContain("paused:o/x99:2");
  });

  it("reopens the given events only", () => {
    const store = useDismissedMachineEventsStore.getState();
    store.dismiss(["a", "b"]);
    useDismissedMachineEventsStore.getState().reopen(["a"]);
    expect(useDismissedMachineEventsStore.getState().dismissed).toEqual(["b"]);
  });

  it("keeps only the 50 most recent dismissals", () => {
    const ids = Array.from({ length: 60 }, (_, i) => `e${i}`);
    useDismissedMachineEventsStore.getState().dismiss(ids);
    const { dismissed } = useDismissedMachineEventsStore.getState();
    expect(dismissed).toHaveLength(50);
    expect(dismissed[0]).toBe("e10");
    expect(dismissed.at(-1)).toBe("e59");
  });
});
