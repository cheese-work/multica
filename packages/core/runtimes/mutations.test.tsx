// @vitest-environment jsdom
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";

const mockPause = vi.hoisted(() => vi.fn());
const mockResume = vi.hoisted(() => vi.fn());

vi.mock("../api", () => ({
  api: {
    pauseRuntimeMachine: (...args: unknown[]) => mockPause(...args),
    resumeRuntimeMachine: (...args: unknown[]) => mockResume(...args),
  },
}));

import { usePauseRuntimeMachine, useResumeRuntimeMachine } from "./mutations";
import { runtimeKeys } from "./queries";

function setup() {
  const qc = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  const invalidate = vi.spyOn(qc, "invalidateQueries");
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );
  return { invalidate, wrapper };
}

describe("machine pause mutations", () => {
  beforeEach(() => {
    mockPause.mockReset().mockResolvedValue({});
    mockResume.mockReset().mockResolvedValue(undefined);
  });

  it("pauses through the API and refreshes the workspace runtimes", async () => {
    const { invalidate, wrapper } = setup();
    const { result } = renderHook(() => usePauseRuntimeMachine("ws-1"), { wrapper });
    await act(() => result.current.mutateAsync({ runtimeId: "rt-1", reason: "incident" }));
    expect(mockPause).toHaveBeenCalledWith("rt-1", "incident");
    expect(invalidate).toHaveBeenCalledWith({ queryKey: runtimeKeys.all("ws-1") });
  });

  it("resumes through the API and refreshes the workspace runtimes", async () => {
    const { invalidate, wrapper } = setup();
    const { result } = renderHook(() => useResumeRuntimeMachine("ws-1"), { wrapper });
    await act(() => result.current.mutateAsync("rt-1"));
    expect(mockResume).toHaveBeenCalledWith("rt-1");
    expect(invalidate).toHaveBeenCalledWith({ queryKey: runtimeKeys.all("ws-1") });
  });

  it("refreshes even when the server refuses, so the page shows the real state", async () => {
    mockResume.mockRejectedValue(new Error("403"));
    const { invalidate, wrapper } = setup();
    const { result } = renderHook(() => useResumeRuntimeMachine("ws-1"), { wrapper });
    await act(async () => {
      await result.current.mutateAsync("rt-1").catch(() => undefined);
    });
    await waitFor(() => expect(invalidate).toHaveBeenCalled());
  });
});
