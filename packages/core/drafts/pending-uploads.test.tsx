// @vitest-environment jsdom
import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { setCurrentWorkspace } from "../platform/workspace-storage";
import { clearRegisteredWorkspaceDrafts, resetAllRegisteredDrafts } from "./cleanup-registry";
import {
  beginPendingUpload,
  invalidatePendingUploads,
  settlePendingUpload,
  usePendingUploadCount,
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

  it("settles in the workspace the upload started in, even while another is active", () => {
    let ticket!: ReturnType<typeof beginPendingUpload>;
    const { result } = renderHook(() => usePendingUploadCount("k"));
    act(() => { ticket = beginPendingUpload("k"); });
    act(() => setCurrentWorkspace("other", "ws-b"));
    expect(renderHook(() => usePendingUploadCount("k")).result.current).toBe(0);
    const write = vi.fn();
    act(() => settlePendingUpload(ticket, write));
    expect(write).toHaveBeenCalledOnce();
    expect(ticket.slug).toBe("acme");
    act(() => setCurrentWorkspace("acme", "ws-a"));
    expect(result.current).toBe(0);
  });

  it("keeps the count while another workspace is active, and shows it again on return", () => {
    act(() => { beginPendingUpload("k"); });
    act(() => setCurrentWorkspace("other", "ws-b"));
    expect(renderHook(() => usePendingUploadCount("k")).result.current).toBe(0);
    act(() => setCurrentWorkspace("acme", "ws-a"));
    expect(renderHook(() => usePendingUploadCount("k")).result.current).toBe(1);
  });

  it("writes nothing after that workspace's drafts were cleared, even for a recycled generation", () => {
    let ticket!: ReturnType<typeof beginPendingUpload>;
    act(() => { ticket = beginPendingUpload("k"); });
    clearRegisteredWorkspaceDrafts(memoryAdapter(), "acme");
    act(() => { beginPendingUpload("k"); });
    const write = vi.fn();
    act(() => settlePendingUpload(ticket, write));
    expect(write).not.toHaveBeenCalled();
  });
});
