// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import { createAuthStore, registerAuthStore } from "@multica/core/auth";
import type { ApiClient } from "@multica/core/api/client";
import type { StorageAdapter, User } from "@multica/core/types";
import enCommon from "../locales/en/common.json";
import enAuth from "../locales/en/auth.json";
import { useLogout } from "./use-logout";

// CHE-1795: with cookie auth the
// server logout is what revokes the session, including its push subscriptions.
// A failed or slow one must neither report completion nor clean up, so the user
// can retry. These tests run the real auth store and the real hook; only the
// network, the navigation and the toast are replaced.

const mockClear = vi.hoisted(() => vi.fn());
const mockPush = vi.hoisted(() => vi.fn());
const mockToastError = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/platform", () => ({ clearClientSessionData: mockClear }));
vi.mock("../navigation", () => ({ useNavigation: () => ({ push: mockPush }) }));
vi.mock("sonner", () => ({ toast: { error: mockToastError } }));

const user = { id: "u1", name: "Alice", email: "alice@example.com", avatar_url: null } as User;

function deferred() {
  let resolve!: () => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<void>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function setup(options: { cookieAuth: boolean }) {
  const data: Record<string, string> = options.cookieAuth ? {} : { multica_token: "t" };
  const storage: StorageAdapter = {
    getItem: (k) => data[k] ?? null,
    setItem: (k, v) => void (data[k] = v),
    removeItem: (k) => void delete data[k],
  };
  const api = { logout: vi.fn(), setToken: vi.fn() };
  const onLogout = vi.fn();
  const store = createAuthStore({
    api: api as unknown as ApiClient,
    storage,
    cookieAuth: options.cookieAuth,
    onLogout,
  });
  registerAuthStore(store);
  store.setState({ user, status: "authenticated", isLoading: false });
  const queryClient = new QueryClient();
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={queryClient}>
      <I18nProvider locale="en" resources={{ en: { common: enCommon, auth: enAuth } }}>
        {children}
      </I18nProvider>
    </QueryClientProvider>
  );
  const hook = renderHook(() => useLogout(), { wrapper });
  return { api, store, onLogout, hook, storage };
}

async function press(hook: ReturnType<typeof setup>["hook"]) {
  let result: unknown;
  await act(async () => {
    result = await hook.result.current();
  });
  return result;
}

describe("logout with cookie auth", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("keeps the session, cleans nothing and says so when the server refuses", async () => {
    const { api, store, onLogout, hook } = setup({ cookieAuth: true });
    api.logout.mockRejectedValue(new Error("503 could not end this session's push subscriptions"));

    const done = await press(hook);

    expect(mockClear).not.toHaveBeenCalled();
    expect(mockPush).not.toHaveBeenCalled();
    expect(onLogout).not.toHaveBeenCalled();
    expect(store.getState().status).toBe("authenticated");
    expect(store.getState().user).toEqual(user);
    expect(mockToastError).toHaveBeenCalledTimes(1);
    expect(String(mockToastError.mock.calls[0]?.[0] ?? "")).not.toBe("");
    expect(done).toBe(false);
  });

  it("does not clean up before a slow server answers, then cleans up once", async () => {
    const { api, store, onLogout, hook } = setup({ cookieAuth: true });
    const pending = deferred();
    api.logout.mockReturnValue(pending.promise);

    let finished: Promise<unknown> | undefined;
    await act(async () => {
      finished = hook.result.current();
      await Promise.resolve();
    });
    expect(mockClear).not.toHaveBeenCalled();
    expect(mockPush).not.toHaveBeenCalled();
    expect(store.getState().status).toBe("authenticated");

    await act(async () => {
      pending.resolve();
      await finished;
    });
    expect(mockClear).toHaveBeenCalledTimes(1);
    expect(mockPush).toHaveBeenCalledTimes(1);
    expect(mockPush).toHaveBeenCalledWith("/login");
    expect(onLogout).toHaveBeenCalledTimes(1);
    expect(store.getState().status).toBe("unauthenticated");
  });

  it("retries after a failure and then cleans up exactly once", async () => {
    const { api, store, onLogout, hook } = setup({ cookieAuth: true });
    api.logout.mockRejectedValueOnce(new Error("network")).mockResolvedValueOnce(undefined);

    expect(await press(hook)).toBe(false);
    expect(await press(hook)).toBe(true);

    expect(api.logout).toHaveBeenCalledTimes(2);
    expect(mockClear).toHaveBeenCalledTimes(1);
    expect(mockPush).toHaveBeenCalledTimes(1);
    expect(onLogout).toHaveBeenCalledTimes(1);
    expect(store.getState().status).toBe("unauthenticated");
  });

  it("sends one request and cleans up once when pressed twice while it is in flight", async () => {
    const { api, hook } = setup({ cookieAuth: true });
    const pending = deferred();
    api.logout.mockReturnValue(pending.promise);

    await act(async () => {
      const first = hook.result.current();
      const second = hook.result.current();
      await Promise.resolve();
      pending.resolve();
      await Promise.all([first, second]);
    });

    expect(api.logout).toHaveBeenCalledTimes(1);
    expect(mockClear).toHaveBeenCalledTimes(1);
    expect(mockPush).toHaveBeenCalledTimes(1);
  });
});

describe("logout paths that need no server round trip", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("token mode (desktop, legacy web) logs out locally without calling the server", async () => {
    const { api, store, hook, storage } = setup({ cookieAuth: false });

    await press(hook);

    expect(api.logout).not.toHaveBeenCalled();
    expect(mockClear).toHaveBeenCalledTimes(1);
    expect(mockPush).toHaveBeenCalledWith("/login");
    expect(store.getState().status).toBe("unauthenticated");
    expect(storage.getItem("multica_token")).toBeNull();
  });

  it("an expired session is torn down without a server round trip", () => {
    const { api, store } = setup({ cookieAuth: true });

    store.getState().sessionExpired();

    expect(api.logout).not.toHaveBeenCalled();
    expect(store.getState().status).toBe("unauthenticated");
    expect(store.getState().expired).toBe(true);
  });
});
