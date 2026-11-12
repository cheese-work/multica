import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { I18nProvider } from "@multica/core/i18n/react";
import { ApiError } from "@multica/core/api";
import { getTestResources } from "../../test/i18n";
import { useWakeupForm } from "./use-wakeup-form";

vi.mock("@tanstack/react-query", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@tanstack/react-query")>()),
  useQuery: () => ({ data: [] }),
}));
vi.mock("./wakeup-condition-names", () => ({
  useConditionNames: () => ({ status: (key: string) => key, label: () => undefined, property: () => undefined, actor: (_type: string, id: string) => id }),
}));
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "Asia/Shanghai" }));

const save = vi.fn();
const onSaved = vi.fn();
const onBusy = vi.fn();
const wrapper = ({ children }: { children: ReactNode }) => (
  <I18nProvider locale="en" resources={getTestResources()}>{children}</I18nProvider>
);
const setup = () =>
  renderHook(() => useWakeupForm({ workspaceId: "ws", defaultAgentId: "emacs", save, onSaved, onBusy }), { wrapper });

beforeEach(() => {
  save.mockReset().mockResolvedValue(undefined);
  onSaved.mockReset();
  onBusy.mockReset();
});

describe("useWakeupForm", () => {
  it("starts from an empty draft for the default agent", () => {
    const { result } = setup();
    expect(result.current.draft).toMatchObject({ condition: null, agentId: "emacs", timezone: "Asia/Shanghai" });
    expect(result.current.error).toBe("");
  });

  it("refuses an incomplete draft without calling save", async () => {
    const { result } = setup();
    await act(() => result.current.submit());
    expect(result.current.error).toBe("Choose a condition.");
    expect(save).not.toHaveBeenCalled();
  });

  it("clears the error on the next edit", async () => {
    const { result } = setup();
    await act(() => result.current.submit());
    act(() => result.current.update({ instruction: "x" }));
    expect(result.current.error).toBe("");
  });

  it("hands save the built payload, then reports it saved", async () => {
    const { result } = setup();
    act(() => result.current.update({ condition: "reply", instruction: "Continue" }));
    await act(() => result.current.submit());
    expect(save).toHaveBeenCalledWith(
      expect.objectContaining({ agent_id: "emacs", instruction: "Continue", kind: "event", event_types: ["comment.created"] }),
    );
    expect(onSaved).toHaveBeenCalledOnce();
    expect(onBusy.mock.calls).toEqual([[true], [false]]);
  });

  it("keeps the draft and explains a capacity refusal", async () => {
    save.mockRejectedValue(new ApiError("full", 400, "Bad Request", { code: "wakeup_capacity_exceeded" }));
    const { result } = setup();
    act(() => result.current.update({ condition: "reply", instruction: "Continue" }));
    await act(() => result.current.submit());
    expect(result.current.error).toMatch(/maximum number of enabled wakeups/);
    expect(result.current.draft.instruction).toBe("Continue");
    expect(onSaved).not.toHaveBeenCalled();
    expect(onBusy.mock.calls).toEqual([[true], [false]]);
  });

  it("shows the generic message for any other failure", async () => {
    save.mockRejectedValue(new Error("boom"));
    const { result } = setup();
    act(() => result.current.update({ condition: "reply", instruction: "Continue" }));
    await act(() => result.current.submit());
    expect(result.current.error).not.toBe("");
    expect(onSaved).not.toHaveBeenCalled();
  });

  it("ignores a second submit while a save is in flight", async () => {
    let finish!: () => void;
    save.mockReturnValue(new Promise<void>((resolve) => (finish = resolve)));
    const { result } = setup();
    act(() => result.current.update({ condition: "reply", instruction: "Continue" }));
    let first!: Promise<void>;
    act(() => {
      first = result.current.submit();
    });
    expect(result.current.pending).toBe(true);
    await act(() => result.current.submit());
    expect(save).toHaveBeenCalledOnce();
    await act(async () => {
      finish();
      await first;
    });
    expect(result.current.pending).toBe(false);
  });

  it("shows the caller's message for a refusal it knows, and the shared one otherwise", async () => {
    const describeError = vi.fn((err: unknown) => (err instanceof Error && err.message === "known" ? "Known refusal." : undefined));
    const { result } = renderHook(
      () => useWakeupForm({ workspaceId: "ws", defaultAgentId: "emacs", save, onSaved, describeError }),
      { wrapper },
    );
    act(() => result.current.update({ condition: "custom", events: ["issue.updated"], instruction: "Go" }));
    save.mockRejectedValueOnce(new Error("known"));
    await act(() => result.current.submit());
    expect(result.current.error).toBe("Known refusal.");
    save.mockRejectedValueOnce(new Error("other"));
    await act(() => result.current.submit());
    expect(result.current.error).not.toBe("Known refusal.");
    expect(result.current.error).not.toBe("");
  });
});
