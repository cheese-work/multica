import { StrictMode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { SupportedLocale } from "@multica/core/i18n";
import { I18nProvider } from "@multica/core/i18n/react";
import { NavigationProvider, type NavigationAdapter } from "../../navigation";
import { RESOURCES } from "../../locales";
import { InteractiveLoginBanner } from "./interactive-login-banner";

const mocks = vi.hoisted(() => ({
  logout: vi.fn(),
  endServerSession: vi.fn(),
  clearSession: vi.fn(),
  push: vi.fn(),
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: (
    selector: (state: { logout: () => void; endServerSession: () => Promise<void> }) => unknown,
  ) => selector({ logout: mocks.logout, endServerSession: mocks.endServerSession }),
}));
vi.mock("@multica/core/platform", () => ({
  clearClientSessionData: mocks.clearSession,
}));

function renderBanner(locale: SupportedLocale = "en") {
  const queryClient = new QueryClient();
  const navigation: NavigationAdapter = {
    push: mocks.push,
    replace: vi.fn(),
    back: vi.fn(),
    pathname: "/acme/chat",
    searchParams: new URLSearchParams(),
    hash: "",
    getShareableUrl: (path) => `https://multica.test${path}`,
  };
  render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <I18nProvider locale={locale} resources={RESOURCES}>
          <NavigationProvider value={navigation}>
            <InteractiveLoginBanner />
          </NavigationProvider>
        </I18nProvider>
      </QueryClientProvider>
    </StrictMode>,
  );
  return queryClient;
}

describe("InteractiveLoginBanner", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.endServerSession.mockResolvedValue(undefined);
  });

  it.each([
    ["en", "Sign in"],
    ["zh-Hans", "登录"],
    ["ja", "ログイン"],
    ["ko", "로그인"],
    ["fr", "Se connecter"],
  ] as const)("starts explicit re-login only after the %s sign-in action", async (locale, label) => {
    const queryClient = renderBanner(locale);
    const button = screen.getByRole("button", { name: label });
    expect(screen.getByRole("alert")).toContainElement(button);
    expect(mocks.clearSession).not.toHaveBeenCalled();
    expect(mocks.logout).not.toHaveBeenCalled();
    expect(mocks.push).not.toHaveBeenCalled();

    fireEvent.click(button);

    await waitFor(() => expect(mocks.push).toHaveBeenCalledExactlyOnceWith("/login"));
    expect(mocks.endServerSession).toHaveBeenCalledTimes(1);
    expect(mocks.clearSession).toHaveBeenCalledExactlyOnceWith(queryClient);
    expect(mocks.logout).toHaveBeenCalledTimes(1);
  });

  it("supports explicit keyboard sign-in without automatic logout", async () => {
    const user = userEvent.setup();
    renderBanner();
    await user.tab();
    expect(screen.getByRole("button", { name: "Sign in" })).toHaveFocus();
    expect(mocks.logout).not.toHaveBeenCalled();

    await user.keyboard("{Enter}");

    await waitFor(() => expect(mocks.push).toHaveBeenCalledExactlyOnceWith("/login"));
    expect(mocks.logout).toHaveBeenCalledTimes(1);
  });
});
