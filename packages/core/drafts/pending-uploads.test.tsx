// @vitest-environment jsdom
import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { setCurrentWorkspace } from "../platform/workspace-storage";
import { clearRegisteredWorkspaceDrafts, currentDraftOrigin, resetAllRegisteredDrafts } from "./cleanup-registry";
import {
  beginPendingUpload,
  invalidatePendingUploads,
  settlePendingUpload,
  usePendingUploadCount,
  useUploadCancelledNotice,
} from "./pending-uploads";

const memoryAdapter = () => ({ getItem: () => null, setItem: () => {}, removeItem: () => {}, keys: () => [] as string[] });

describe("pending uploads", () => {
  beforeEach(() => {
    resetAllRegisteredDrafts();
    setCurrentWorkspace("acme", "ws-a");
  });

  it("counts an upload until it settles, and runs the write first", () => {
    const { result } = renderHook(() => usePendingUploadCount("k"));
    let ticket!: ReturnType<typeof beginPendingUpload>;
    act(() => { ticket = beginPendingUpload("k"); });
    expect(result.current).toBe(1);
    const write = vi.fn(() => expect(result.current).toBe(1));
    act(() => settlePendingUpload(ticket, write));
    expect(write).toHaveBeenCalledOnce();
    expect(result.current).toBe(0);
  });

  it("writes nothing after invalidate, and the count stays at zero", () => {
    const { result } = renderHook(() => usePendingUploadCount("k"));
    let ticket!: ReturnType<typeof beginPendingUpload>;
    act(() => { ticket = beginPendingUpload("k"); });
    act(() => invalidatePendingUploads("k"));
    const write = vi.fn();
    act(() => settlePendingUpload(ticket, write));
    expect(write).not.toHaveBeenCalled();
    expect(result.current).toBe(0);
  });

  it("writes nothing after logout", () => {
    const ticket = beginPendingUpload("k");
    resetAllRegisteredDrafts();
    const write = vi.fn();
    settlePendingUpload(ticket, write);
    expect(write).not.toHaveBeenCalled();
  });

  it("cancels an upload when another workspace becomes active, with a notice on return", async () => {
    const { result: count } = renderHook(() => usePendingUploadCount("k"));
    const { result: notice } = renderHook(() => useUploadCancelledNotice("k"));
    let ticket!: ReturnType<typeof beginPendingUpload>;
    act(() => { ticket = beginPendingUpload("k"); });
    await act(async () => { setCurrentWorkspace("other", "ws-b"); await Promise.resolve(); });
    expect(ticket.signal.aborted).toBe(true);
    const write = vi.fn();
    act(() => settlePendingUpload(ticket, write));
    expect(write).not.toHaveBeenCalled();
    await act(async () => { setCurrentWorkspace("acme", "ws-a"); await Promise.resolve(); });
    expect(count.current).toBe(0);
    expect(notice.current).toBe(true);
    act(() => { beginPendingUpload("k"); });
    expect(notice.current).toBe(false);
  });

  it("does not write when the ticket's workspace is no longer active, even before the cancel runs", () => {
    let ticket!: ReturnType<typeof beginPendingUpload>;
    act(() => { ticket = beginPendingUpload("k"); });
    setCurrentWorkspace("other", "ws-b");
    const write = vi.fn();
    settlePendingUpload(ticket, write);
    expect(write).not.toHaveBeenCalled();
  });

  it("shows no notice when the upload had already settled before the switch", async () => {
    const { result: notice } = renderHook(() => useUploadCancelledNotice("k"));
    let ticket!: ReturnType<typeof beginPendingUpload>;
    act(() => { ticket = beginPendingUpload("k"); });
    act(() => settlePendingUpload(ticket));
    await act(async () => { setCurrentWorkspace("other", "ws-b"); await Promise.resolve(); });
    await act(async () => { setCurrentWorkspace("acme", "ws-a"); await Promise.resolve(); });
    expect(notice.current).toBe(false);
  });

  it("writes nothing after that workspace's drafts were cleared", () => {
    let ticket!: ReturnType<typeof beginPendingUpload>;
    act(() => { ticket = beginPendingUpload("k"); });
    clearRegisteredWorkspaceDrafts(memoryAdapter(), "acme");
    // The ticket stays valid; the draft store drops the write through the stale origin.
    expect(ticket.origin.generation).not.toBe(currentDraftOrigin("acme").generation);
  });
});
