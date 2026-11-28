// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

const listMembers = vi.hoisted(() => vi.fn());
vi.mock("../api", () => ({ api: { listMembers } }));
vi.mock("../auth", () => ({
  useAuthStore: (sel: (s: { user: { id: string } }) => unknown) => sel({ user: { id: "me" } }),
}));

import { useIsResolvedNonGuest } from "./use-current-member";

function run() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  return renderHook(() => useIsResolvedNonGuest("ws-1"), { wrapper });
}

describe("useIsResolvedNonGuest", () => {
  it("is false while the member list loads, then true for a member", async () => {
    listMembers.mockResolvedValue([{ user_id: "me", role: "member" }]);
    const { result } = run();
    expect(result.current).toBe(false);
    await waitFor(() => expect(result.current).toBe(true));
  });

  it("stays false for a guest", async () => {
    listMembers.mockResolvedValue([{ user_id: "me", role: "guest" }]);
    const { result } = run();
    await waitFor(() => expect(listMembers).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 20));
    expect(result.current).toBe(false);
  });
});
