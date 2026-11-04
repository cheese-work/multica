// @vitest-environment jsdom
import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { setCurrentWorkspace } from "../platform/workspace-storage";
import { resetAllRegisteredDrafts } from "./cleanup-registry";
import {
  beginPendingUpload,
  invalidatePendingUploads,
  settlePendingUpload,
  usePendingUploadCount,
} from "./pending-uploads";

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

  it("writes nothing into another workspace, and keeps counts per workspace", () => {
    const ticket = beginPendingUpload("k");
    setCurrentWorkspace("other", "ws-b");
    const { result } = renderHook(() => usePendingUploadCount("k"));
    expect(result.current).toBe(0);
    const write = vi.fn();
    settlePendingUpload(ticket, write);
    expect(write).not.toHaveBeenCalled();
    setCurrentWorkspace("acme", "ws-a");
    expect(renderHook(() => usePendingUploadCount("k")).result.current).toBe(0);
  });
});
