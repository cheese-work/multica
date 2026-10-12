import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "@multica/views/locales/en/common.json";
import enAuth from "@multica/views/locales/en/auth.json";
import enSettings from "@multica/views/locales/en/settings.json";
import type { ReactNode } from "react";

const TEST_RESOURCES = {
  en: { common: enCommon, auth: enAuth, settings: enSettings },
};

function createWrapper() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return ({ children }: { children: ReactNode }) => (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={qc}>{children}</QueryClientProvider>
    </I18nProvider>
  );
}

const {
  mockIssueCliToken,
  mockIssueDesktopToken,
  mockListWorkspaces,
  mockListMyInvitations,
  mockPush,
  mockReplace,
  searchParamsState,
  authStateRef,
} = vi.hoisted(() => ({
  mockIssueCliToken: vi.fn(),
  mockIssueDesktopToken: vi.fn(),
  mockListWorkspaces: vi.fn(),
  mockListMyInvitations: vi.fn(),
  mockPush: vi.fn(),
  mockReplace: vi.fn(),
  searchParamsState: { params: new URLSearchParams() },
  authStateRef: {
    state: {
      sendCode: vi.fn(),
      verifyCode: vi.fn(),
      user: null as null | { id: string; email: string; onboarded_at?: string | null },
      isLoading: false,
    },
  },
}));

// Mock next/navigation — router spies are hoisted so tests can assert
// which navigation (if any) the page issued.
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: mockPush, replace: mockReplace }),
  usePathname: () => "/login",
  useSearchParams: () => searchParamsState.params,
}));

// Mock auth store — shared LoginPage uses getState().sendCode/verifyCode,
// web wrapper uses useAuthStore((s) => s.user/isLoading). Keep the real
// sanitizeNextUrl so the redirect-sanitization rules are exercised rather
// than silently drifting behind a mock reimplementation.
vi.mock("@multica/core/auth", async () => {
  const actual =
    await vi.importActual<typeof import("@multica/core/auth")>(
      "@multica/core/auth",
    );
  const useAuthStore = Object.assign(
    (selector: (s: typeof authStateRef.state) => unknown) =>
      selector(authStateRef.state),
    { getState: () => authStateRef.state },
  );
  return { ...actual, useAuthStore };
});

// Mock auth-cookie
vi.mock("@/features/auth/auth-cookie", () => ({
  setLoggedInCookie: vi.fn(),
}));

// Mock api
vi.mock("@multica/core/api", () => ({
  api: {
    listWorkspaces: mockListWorkspaces,
    listMyInvitations: mockListMyInvitations,
    verifyCode: vi.fn(),
    setToken: vi.fn(),
    getMe: vi.fn(),
    issueCliToken: mockIssueCliToken,
    issueDesktopToken: mockIssueDesktopToken,
  },
}));

import LoginPage from "./page";

describe("LoginPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    searchParamsState.params = new URLSearchParams();
    authStateRef.state.user = null;
    authStateRef.state.isLoading = false;
    mockListWorkspaces.mockResolvedValue([]);
    mockListMyInvitations.mockResolvedValue([]);
  });

  // Shared LoginPage behavior is canonical in
  // packages/views/auth/login-page.test.tsx. This wrapper suite only owns web
  // platform handoff and redirect behavior.

  it.each(["existing session", "fresh email login"])("requires a click before desktop mint and handoff for %s", async (path) => {
    searchParamsState.params = new URLSearchParams({ platform: "desktop" });
    if (path === "existing session") {
      authStateRef.state.user = { id: "u1", email: "test@multica.ai" };
    }
    mockIssueDesktopToken.mockImplementation(() =>
      Promise.resolve({ token: "handoff-jwt" }),
    );

    const hrefSetter = vi.fn();
    const originalLocation = window.location;
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...originalLocation, set href(value: string) { hrefSetter(value); } },
    });

    try {
      const page = render(<LoginPage />, { wrapper: createWrapper() });
      if (path === "fresh email login") {
        await act(async () => {
          authStateRef.state.user = { id: "u1", email: "test@multica.ai" };
          page.rerender(<LoginPage />);
        });
      }
      const openButton = await screen.findByRole("button", { name: "Open Multica Desktop" });
      expect(mockIssueDesktopToken).not.toHaveBeenCalled();
      expect(mockIssueCliToken).not.toHaveBeenCalled();
      expect(hrefSetter).not.toHaveBeenCalled();
      fireEvent.click(openButton);
      await waitFor(() => {
        expect(hrefSetter).toHaveBeenCalledWith(
          "multica://auth/callback?token=handoff-jwt",
        );
      });
      expect(mockIssueDesktopToken).toHaveBeenCalledTimes(1);
      expect(mockIssueCliToken).not.toHaveBeenCalled();
    } finally {
      Object.defineProperty(window, "location", {
        configurable: true,
        value: originalLocation,
      });
    }
  });

  // Regression: #5009 — the "already authenticated on arrival" effect used to
  // fire for fresh form logins too. verifyCode writes `user` while handleVerify
  // is still fetching the workspace list, so the effect read an empty cache and
  // raced handleSuccess with replace("/workspaces/new"); depending on the
  // interleaving the user could end up stuck on the create-workspace page
  // despite having workspaces.
  describe("post-login redirect ownership (#5009)", () => {
    const onboardedUser = {
      id: "u1",
      email: "test@multica.ai",
      onboarded_at: "2026-01-01T00:00:00Z",
    };

    it("does not redirect from the arrival effect when the user logs in via the form", async () => {
      // Auth settles as logged-out first — the page latches "any user from
      // now on came from the form".
      const wrapper = createWrapper();
      const { rerender } = render(<LoginPage />, { wrapper });
      // verifyCode set the user; the workspace list fetch is still in flight
      // (cache cold). The arrival effect must stay silent — handleSuccess
      // owns this navigation.
      authStateRef.state.user = onboardedUser;
      rerender(<LoginPage />);

      await act(async () => {});
      expect(mockReplace).not.toHaveBeenCalled();
      expect(mockPush).not.toHaveBeenCalled();
      expect(mockListWorkspaces).not.toHaveBeenCalled();
    });

    it("fetches the workspace list before redirecting a visitor who arrived authenticated", async () => {
      // Cold Query cache on a fresh page load: reading it would say "no
      // workspaces" and misroute to /workspaces/new. The effect must fetch.
      authStateRef.state.user = onboardedUser;
      mockListWorkspaces.mockResolvedValue([{ id: "ws-1", slug: "acme" }]);

      render(<LoginPage />, { wrapper: createWrapper() });

      await waitFor(() => {
        expect(mockReplace).toHaveBeenCalledWith("/acme/issues");
      });
      expect(mockListWorkspaces).toHaveBeenCalledTimes(1);
    });

    it("still honors ?next= for a visitor who arrived authenticated", async () => {
      searchParamsState.params = new URLSearchParams({
        next: "/invite/abc",
      });
      authStateRef.state.user = onboardedUser;

      render(<LoginPage />, { wrapper: createWrapper() });

      await waitFor(() => {
        expect(mockReplace).toHaveBeenCalledWith("/invite/abc");
      });
      expect(mockListWorkspaces).not.toHaveBeenCalled();
    });
  });
});
