/**
 * @vitest-environment jsdom
 */
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import type { TimelineEntry } from "../types";
import { sortTimelineEntriesAsc } from "./timeline-sort";

vi.mock("../api", () => ({ api: { listTimeline: vi.fn() }, ApiError: class ApiError extends Error {} }));

const { ingestTimeline, updateTimeline, useTimelineRows } = await import("./timeline-store");

const entry = (id: string, created_at: string): TimelineEntry => ({
  type: "comment",
  id,
  actor_type: "member",
  actor_id: "u1",
  content: id,
  created_at,
});

function setup() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );
  return { qc, wrapper };
}

describe("useTimelineRows", () => {
  it("is undefined before the timeline loads, then derives rows from the store", async () => {
    const { qc, wrapper } = setup();
    const { result } = renderHook(() => useTimelineRows("i1"), { wrapper });
    expect(result.current).toBeUndefined();

    const a = entry("a", "2026-11-24T00:00:00Z");
    act(() => void ingestTimeline(qc, "i1", [a]));

    await waitFor(() => expect(result.current).toEqual([a]));
  });

  it("keeps the rows array while an unrelated row of another issue changes", async () => {
    const { qc, wrapper } = setup();
    ingestTimeline(qc, "i1", [entry("a", "2026-11-24T00:00:00Z")]);
    ingestTimeline(qc, "i2", [entry("z", "2026-11-24T00:00:00Z")]);
    const { result } = renderHook(() => useTimelineRows("i1"), { wrapper });
    const before = result.current;

    act(() => updateTimeline(qc, "i2", (rows) => rows?.map((r) => ({ ...r, content: "changed" }))));
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 10)); });

    expect(result.current).toBe(before);
  });

  it("keeps the order a late-arriving older entry is sorted into", async () => {
    const { qc, wrapper } = setup();
    ingestTimeline(qc, "i1", [
      entry("b", "2026-11-24T00:00:02Z"),
      entry("c", "2026-11-24T00:00:03Z"),
    ]);
    const { result } = renderHook(() => useTimelineRows("i1"), { wrapper });

    act(() =>
      updateTimeline(qc, "i1", (rows) =>
        sortTimelineEntriesAsc([...(rows ?? []), entry("a", "2026-11-24T00:00:01Z")]),
      ),
    );

    await waitFor(() => expect(result.current?.map((r) => r.id)).toEqual(["a", "b", "c"]));
  });

  it("drops a deleted row from the derived list", async () => {
    const { qc, wrapper } = setup();
    ingestTimeline(qc, "i1", [entry("a", "2026-11-24T00:00:00Z"), entry("b", "2026-11-24T00:00:01Z")]);
    const { result } = renderHook(() => useTimelineRows("i1"), { wrapper });

    act(() => updateTimeline(qc, "i1", (rows) => rows?.filter((r) => r.id !== "a")));

    await waitFor(() => expect(result.current?.map((r) => r.id)).toEqual(["b"]));
  });
});
