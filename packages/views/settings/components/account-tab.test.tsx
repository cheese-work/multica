import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithI18n } from "../../test/i18n";

const mockUpdateMe = vi.hoisted(() => vi.fn());
const user = vi.hoisted(() => ({
  current: { id: "user-1", name: "Ada", profile_description: "", avatar_url: null },
}));

vi.mock("@multica/core/api", () => ({ api: { updateMe: mockUpdateMe } }));
vi.mock("@multica/core/auth", () => {
  const useAuthStore = (selector: (s: unknown) => unknown) =>
    selector({ user: user.current, setUser: vi.fn() });
  return { useAuthStore };
});
vi.mock("../../common/avatar-upload-control", () => ({ AvatarUploadControl: () => null }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { AccountTab } from "./account-tab";

describe("AccountTab — unsent edits survive a remount (CHE-3527)", () => {
  afterEach(() => vi.useRealTimers());

  it("restores the profile name and description after unmount", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    mockUpdateMe.mockRejectedValue(new Error("offline"));
    const typist = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    const first = renderWithI18n(<AccountTab />);
    await typist.type(screen.getByRole("textbox", { name: "About you" }), "Unsent bio");
    act(() => first.unmount());

    renderWithI18n(<AccountTab />);
    expect(screen.getByRole("textbox", { name: "About you" })).toHaveValue("Unsent bio");
  });
});

beforeEach(() => resetAllRegisteredDrafts());
