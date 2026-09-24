import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@multica/core/api";
import type { WorkspaceExportPrivacy } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

const mocks = vi.hoisted(() => ({
  useQuery: vi.fn(),
  updateMutateAsync: vi.fn(),
  updatePending: false,
  memberRole: "owner" as "owner" | "admin" | "member",
  membersFetched: true,
  privacyData: undefined as WorkspaceExportPrivacy | undefined,
  privacyPending: false,
  privacyError: null as unknown,
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: unknown) => mocks.useQuery(options),
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: (
    selector?: (state: { user: { id: string } }) => unknown,
  ) => {
    const state = { user: { id: "user-1" } };
    return selector ? selector(state) : state;
  },
}));

vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({
    id: "workspace-1",
    slug: "acme",
    name: "Acme",
  }),
}));

vi.mock("@multica/core/workspace", () => ({
  memberListOptions: (wsId: string) => ({
    queryKey: ["workspaces", wsId, "members"],
  }),
  workspaceExportPrivacyOptions: (wsId: string, enabled: boolean) => ({
    queryKey: ["workspaces", wsId, "export-privacy"],
    enabled,
  }),
  useUpdateWorkspaceExportPrivacy: () => ({
    mutateAsync: mocks.updateMutateAsync,
    isPending: mocks.updatePending,
  }),
}));

import { ExportPrivacyTab } from "./export-privacy-tab";

describe("ExportPrivacyTab", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mocks.memberRole = "owner";
    mocks.membersFetched = true;
    mocks.privacyData = {
      redaction_mode: "small",
      manifest_retention_days: 90,
    };
    mocks.privacyPending = false;
    mocks.privacyError = null;
    mocks.updatePending = false;
    mocks.useQuery.mockImplementation((options: unknown) => {
      const queryKey = (options as { queryKey?: readonly unknown[] })
        .queryKey;
      const last = queryKey?.[queryKey.length - 1];
      if (last === "members") {
        return {
          data: [{ user_id: "user-1", role: mocks.memberRole }],
          isFetched: mocks.membersFetched,
        };
      }
      if (last === "export-privacy") {
        return {
          data: mocks.privacyData,
          isPending: mocks.privacyPending,
          isError: !!mocks.privacyError,
          error: mocks.privacyError,
        };
      }
      throw new Error(`unexpected queryKey: ${JSON.stringify(queryKey)}`);
    });
  });

  it("renders nothing for a plain member", () => {
    mocks.memberRole = "member";
    const { container } = renderWithI18n(<ExportPrivacyTab />);
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the default small mode and 90-day retention for an admin", () => {
    renderWithI18n(<ExportPrivacyTab />);
    expect(
      screen.getByRole("combobox", { name: "Redaction mode" }),
    ).toBeTruthy();
    expect(
      screen.getByText("Small (secrets and credentials masked)"),
    ).toBeTruthy();
    const retentionInput = screen.getByRole("spinbutton", {
      name: "Manifest retention (days)",
    }) as HTMLInputElement;
    expect(retentionInput.value).toBe("90");
  });

  it("does not offer strict as a selectable list item", () => {
    renderWithI18n(<ExportPrivacyTab />);
    expect(screen.queryByRole("option", { name: /strict/i })).toBeNull();
  });

  it("renders an unrecognized stored mode read-only without offering it as a choice", () => {
    mocks.privacyData = {
      redaction_mode: "strict",
      manifest_retention_days: 90,
    };
    renderWithI18n(<ExportPrivacyTab />);
    expect(screen.getByText("strict")).toBeTruthy();
    expect(screen.queryByRole("option", { name: /strict/i })).toBeNull();
    const trigger = screen.getByRole("combobox", {
      name: "Redaction mode",
    });
    expect(trigger.hasAttribute("disabled")).toBe(true);
  });

  it("shows an unavailable state on a 503 kill-switch response", () => {
    mocks.privacyData = undefined;
    mocks.privacyError = new ApiError(
      "export privacy controls are currently disabled",
      503,
      "Service Unavailable",
    );
    renderWithI18n(<ExportPrivacyTab />);
    expect(
      screen.getByText("Export privacy controls are unavailable"),
    ).toBeTruthy();
  });

  it("shows a permission error on a 403 response after mount", () => {
    mocks.privacyData = undefined;
    mocks.privacyError = new ApiError("forbidden", 403, "Forbidden");
    renderWithI18n(<ExportPrivacyTab />);
    expect(screen.getByText("Permission required")).toBeTruthy();
  });

  it("rejects an out-of-range retention value without calling the API", async () => {
    const user = userEvent.setup();
    renderWithI18n(<ExportPrivacyTab />);
    const retentionInput = screen.getByRole("spinbutton", {
      name: "Manifest retention (days)",
    });

    await user.clear(retentionInput);
    await user.type(retentionInput, "4000");
    await user.tab();

    expect(
      screen.getByText("Enter a number between 1 and 3650."),
    ).toBeTruthy();
    expect(mocks.updateMutateAsync).not.toHaveBeenCalled();
  });

  it("saves a valid retention change on blur", async () => {
    const user = userEvent.setup();
    mocks.updateMutateAsync.mockResolvedValue({
      redaction_mode: "small",
      manifest_retention_days: 30,
    });
    renderWithI18n(<ExportPrivacyTab />);
    const retentionInput = screen.getByRole("spinbutton", {
      name: "Manifest retention (days)",
    });

    await user.clear(retentionInput);
    await user.type(retentionInput, "30");
    await user.tab();

    await waitFor(() => {
      expect(mocks.updateMutateAsync).toHaveBeenCalledWith({
        manifest_retention_days: 30,
      });
    });
  });

  it("rolls the input back and shows an error when the save fails", async () => {
    const user = userEvent.setup();
    mocks.updateMutateAsync.mockRejectedValue(
      new ApiError("manifest_retention_days must be between 1 and 3650", 400, "Bad Request"),
    );
    renderWithI18n(<ExportPrivacyTab />);
    const retentionInput = screen.getByRole("spinbutton", {
      name: "Manifest retention (days)",
    }) as HTMLInputElement;

    await user.clear(retentionInput);
    await user.type(retentionInput, "120");
    await user.tab();

    await waitFor(() => {
      expect(
        screen.getByText(
          "manifest_retention_days must be between 1 and 3650",
        ),
      ).toBeTruthy();
    });
    expect(retentionInput.value).toBe("90");
  });
});
