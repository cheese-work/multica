import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { renderWithI18n } from "../../test/i18n";

// CHE-1839, first layer of the Jev / Governance tab (approved design:
// design/jev-governance-tab/rev-4/C.html, routing pane). These tests drive the
// real Settings page, with every other tab stubbed and the API faked, so they
// assert what a user sees and which request the tab sends.
const stub = vi.hoisted(() => (name: string) => () => ({
  [name]: () => <div>{name}</div>,
}));
vi.mock("./account-tab", stub("AccountTab"));
vi.mock("./preferences-tab", stub("PreferencesTab"));
vi.mock("./tokens-tab", stub("TokensTab"));
vi.mock("./workspace-tab", stub("WorkspaceTab"));
vi.mock("./members-tab", stub("MembersTab"));
vi.mock("./code-tab", stub("CodeTab"));
vi.mock("./channels-tab", stub("ChannelsTab"));
vi.mock("./notifications-tab", stub("NotificationsTab"));
vi.mock("./labels-tab", stub("LabelsTab"));
vi.mock("./issue-statuses-tab", stub("IssueStatusesTab"));
vi.mock("./wakeups-tab", stub("WakeupsTab"));
vi.mock("./properties-tab", stub("PropertiesTab"));
vi.mock("./quick-actions-tab", stub("QuickActionsTab"));
vi.mock("./keyboard-shortcuts-tab", stub("KeyboardShortcutsTab"));
vi.mock("./plugins-tab", stub("PluginsTab"));
vi.mock("./mcp-tab", stub("McpTab"));
vi.mock("./billing-tab", stub("BillingTab"));
vi.mock("./export-privacy-tab", stub("ExportPrivacyTab"));
vi.mock("./connected-apps-tab", () => ({
  ConnectedAppsTab: () => <div>ConnectedAppsTab</div>,
  useComposioAvailable: () => false,
}));

const fake = vi.hoisted(() => {
  class FakeApiError extends Error {
    readonly status: number;
    readonly statusText: string;
    readonly body?: unknown;
    constructor(message: string, status: number, statusText: string, body?: unknown) {
      super(message);
      this.name = "ApiError";
      this.status = status;
      this.statusText = statusText;
      this.body = body;
    }
  }
  return {
    FakeApiError,
    role: "admin" as "owner" | "admin" | "member",
    getGovernanceConfig: vi.fn(),
    patchGovernanceConfig: vi.fn(),
  };
});
vi.mock("@multica/core/api", () => ({
  ApiError: fake.FakeApiError,
  api: {
    getGovernanceConfig: fake.getGovernanceConfig,
    patchGovernanceConfig: fake.patchGovernanceConfig,
  },
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "ws-1", name: "Acme" }),
}));
vi.mock("@multica/core/workspace/avatar-url", () => ({
  resolvePublicFileUrl: (url: string | null | undefined) => url ?? null,
}));
vi.mock("@multica/core/permissions", () => ({
  useCurrentMember: () => ({ role: fake.role, isLoading: false }),
}));
vi.mock("@multica/core/auth", () => {
  const state = { user: { id: "user-1", name: "Ada Lovelace", avatar_url: null } };
  const useAuthStore = Object.assign(
    (selector?: (s: typeof state) => unknown) => (selector ? selector(state) : state),
    { getState: () => state },
  );
  return { useAuthStore };
});

const navigationState = { search: "tab=jev-governance" };
const push = vi.fn();
vi.mock("../../navigation/context", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../navigation/context")>()),
  useNavigation: () => ({
    searchParams: new URLSearchParams(navigationState.search),
    hash: "",
    pathname: "/acme/settings",
    replace: vi.fn(),
    push,
  }),
}));
vi.mock("@multica/ui/hooks/use-mobile", () => ({
  useIsMobile: () => false,
  useIsCompact: () => false,
}));

import { SettingsPage } from "./settings-page";

const REASON = "Needs shadow data: ≤ 2% confident-wrong over 200+ verified items.";

function config(version: number, settings: Record<string, unknown> = {}) {
  return {
    version,
    control_epoch: 1,
    settings: { jev_squad_routing_mode: "shadow", jev_min_confidence: 0.8, ...settings },
    missing_requirements: [],
    operating_limits: [],
    audit_history: [],
    credential_present: false,
  };
}

function mount() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <SettingsPage />
    </QueryClientProvider>,
  );
}

function setOnline(online: boolean) {
  Object.defineProperty(window.navigator, "onLine", { value: online, configurable: true });
  window.dispatchEvent(new Event(online ? "online" : "offline"));
}

beforeEach(() => {
  fake.role = "admin";
  navigationState.search = "tab=jev-governance";
  push.mockClear();
  fake.getGovernanceConfig.mockReset().mockResolvedValue(config(14));
  fake.patchGovernanceConfig.mockReset().mockResolvedValue(config(15, { jev_squad_routing_mode: "off" }));
  setOnline(true);
});

afterEach(() => setOnline(true));

describe("Jev / Governance settings entry", () => {
  it("is listed in the workspace navigation for every role", async () => {
    fake.role = "member";
    navigationState.search = "";
    mount();
    expect(await screen.findByRole("link", { name: "Jev / Governance" })).toBeInTheDocument();
  });

  it("is found by searching for its settings and opens the tab", async () => {
    navigationState.search = "";
    mount();
    fireEvent.change(screen.getByRole("searchbox", { name: "Search settings" }), {
      target: { value: "confidence" },
    });
    const results = await screen.findByRole("listbox", { name: "Search settings" });
    fireEvent.click(within(results).getByRole("option", { name: /Jev \/ Governance/ }));
    expect(push).toHaveBeenCalledWith(expect.stringContaining("tab=jev-governance"));
  });
});

describe("Jev / Governance routing pane", () => {
  it("shows the current version, routing mode and confidence, with On locked and explained", async () => {
    mount();
    expect(await screen.findByText("v14 · Shadow")).toBeVisible();
    const mode = screen.getByRole("radiogroup", { name: "Mode" });
    expect(within(mode).getByRole("radio", { name: "Shadow" })).toBeChecked();
    expect(within(mode).getByRole("radio", { name: "Off" })).not.toBeChecked();
    const on = within(mode).getByRole("radio", { name: /^On/ });
    expect(on).toBeDisabled();
    expect(screen.getByText(REASON)).toBeVisible();
    expect(screen.getByRole("textbox", { name: "Min confidence" })).toHaveValue("0.80");
    expect(screen.getByText("Default 0.90.")).toBeVisible();
    // Nothing unfinished is offered: no save bar until something changes, and
    // no credential or history controls in this layer.
    expect(screen.queryByRole("button", { name: "Save changes" })).toBeNull();
    expect(screen.queryByText(/credential|provider key|roll back/i)).toBeNull();
  });

  it("saves a changed mode with the version it was based on", async () => {
    mount();
    const off = await screen.findByRole("radio", { name: "Off" });
    fireEvent.click(off);
    expect(screen.getByText("Based on v14")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(fake.patchGovernanceConfig).toHaveBeenCalledWith(
        "ws-1",
        expect.objectContaining({
          expected_version: 14,
          jev_squad_routing_mode: "off",
          request_id: expect.stringMatching(/^[0-9a-f-]{36}$/),
        }),
      ),
    );
    const body = fake.patchGovernanceConfig.mock.calls[0]![1] as Record<string, unknown>;
    expect(body).not.toHaveProperty("jev_min_confidence");
    expect(await screen.findByText("Saved")).toBeVisible();
    expect(screen.getByText("v15 · Off")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Save changes" })).toBeNull();
  });

  it("saves a valid confidence and rejects one outside 0 to 1 without sending it", async () => {
    mount();
    const input = await screen.findByRole("textbox", { name: "Min confidence" });
    fireEvent.change(input, { target: { value: "1.4" } });
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText("Enter a number from 0 to 1.")).toBeVisible();
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
    expect(input).toHaveFocus();
    expect(fake.patchGovernanceConfig).not.toHaveBeenCalled();

    fireEvent.change(input, { target: { value: "0.85" } });
    expect(input).not.toHaveAttribute("aria-invalid", "true");
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(fake.patchGovernanceConfig).toHaveBeenCalledWith(
        "ws-1",
        expect.objectContaining({ expected_version: 14, jev_min_confidence: 0.85 }),
      ),
    );
  });

  it("cancels an edit back to the saved values", async () => {
    mount();
    fireEvent.click(await screen.findByRole("radio", { name: "Off" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.getByRole("radio", { name: "Shadow" })).toBeChecked();
    expect(screen.queryByRole("button", { name: "Save changes" })).toBeNull();
  });

  it("shows the empty state of a workspace that never saved: v0, Off, default confidence", async () => {
    fake.getGovernanceConfig.mockResolvedValue({
      ...config(0),
      settings: {},
    });
    mount();
    expect(await screen.findByText("v0 · Off")).toBeVisible();
    expect(screen.getByRole("radio", { name: "Off" })).toBeChecked();
    expect(screen.getByRole("textbox", { name: "Min confidence" })).toHaveValue("0.90");
  });
});

describe("Jev / Governance states", () => {
  it("is read-only for a member, who still sees the data", async () => {
    fake.role = "member";
    mount();
    expect(await screen.findByText("v14 · Shadow")).toBeVisible();
    expect(screen.getByText("View only")).toBeVisible();
    expect(screen.getByText("Only workspace owners and admins can change these settings.")).toBeVisible();
    for (const radio of screen.getAllByRole("radio")) expect(radio).toBeDisabled();
    expect(screen.getByRole("textbox", { name: "Min confidence" })).toBeDisabled();
    expect(screen.queryByRole("button", { name: "Save changes" })).toBeNull();
  });

  it("keeps the edit and offers a recovery when another save moved the version (409)", async () => {
    fake.patchGovernanceConfig.mockRejectedValue(
      new fake.FakeApiError("conflict", 409, "Conflict", { problem: "revision_conflict" }),
    );
    mount();
    const input = await screen.findByRole("textbox", { name: "Min confidence" });
    fireEvent.change(input, { target: { value: "0.85" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));

    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("Not saved: settings changed")).toBeVisible();
    expect(input).toHaveValue("0.85");

    fake.getGovernanceConfig.mockResolvedValue(config(15, { jev_min_confidence: 0.7 }));
    fireEvent.click(within(alert).getByRole("button", { name: "Reload latest" }));
    await waitFor(() => expect(screen.getByText("v15 · Shadow")).toBeVisible());
    expect(screen.getByRole("textbox", { name: "Min confidence" })).toHaveValue("0.70");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("lets the user keep editing after a conflict", async () => {
    fake.patchGovernanceConfig.mockRejectedValue(new fake.FakeApiError("conflict", 409, "Conflict"));
    mount();
    const input = await screen.findByRole("textbox", { name: "Min confidence" });
    fireEvent.change(input, { target: { value: "0.85" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    const alert = await screen.findByRole("alert");
    fireEvent.click(within(alert).getByRole("button", { name: "Keep editing" }));
    expect(screen.queryByRole("alert")).toBeNull();
    expect(input).toHaveValue("0.85");
    expect(screen.getByRole("button", { name: "Save changes" })).toBeEnabled();
  });

  it("reports a failed save, keeps the edit and allows another try", async () => {
    fake.patchGovernanceConfig.mockRejectedValueOnce(
      new fake.FakeApiError("unavailable", 503, "Service Unavailable"),
    );
    mount();
    fireEvent.click(await screen.findByRole("radio", { name: "Off" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("Couldn't save. Your changes are kept.")).toBeVisible();
    expect(screen.getByRole("radio", { name: "Off" })).toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("Saved")).toBeVisible();
    expect(fake.patchGovernanceConfig).toHaveBeenCalledTimes(2);
  });

  it("shows a loading state before the configuration arrives", async () => {
    fake.getGovernanceConfig.mockReturnValue(new Promise(() => {}));
    mount();
    expect(await screen.findByRole("status", { name: "Loading governance settings" })).toBeVisible();
    expect(screen.queryByRole("radiogroup", { name: "Mode" })).toBeNull();
  });

  it("shows a load error with a retry", async () => {
    fake.getGovernanceConfig.mockRejectedValueOnce(
      new fake.FakeApiError("unavailable", 503, "Service Unavailable"),
    );
    mount();
    expect(await screen.findByText("Can't load governance settings")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("v14 · Shadow")).toBeVisible();
  });

  it("pauses editing while offline and says so", async () => {
    mount();
    expect(await screen.findByText("v14 · Shadow")).toBeVisible();
    setOnline(false);
    expect(await screen.findByText("You are offline")).toBeVisible();
    for (const radio of screen.getAllByRole("radio")) expect(radio).toBeDisabled();
    expect(screen.getByRole("textbox", { name: "Min confidence" })).toBeDisabled();
    setOnline(true);
    await waitFor(() => expect(screen.queryByText("You are offline")).toBeNull());
    expect(screen.getByRole("radio", { name: "Off" })).toBeEnabled();
  });
});
