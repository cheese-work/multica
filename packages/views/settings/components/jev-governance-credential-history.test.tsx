import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { renderWithI18n } from "../../test/i18n";

// CHE-1853, second layer of the Jev / Governance tab (approved design:
// design/jev-governance-tab/rev-4/C.html, Credential and History panes). Like
// the first layer's tests, these drive the real Settings page with every other
// tab stubbed and the API faked.
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
    getJevCredential: vi.fn(),
    putJevCredential: vi.fn(),
    deleteJevCredential: vi.fn(),
    listMembers: vi.fn(),
  };
});
vi.mock("@multica/core/api", () => ({
  ApiError: fake.FakeApiError,
  api: {
    getGovernanceConfig: fake.getGovernanceConfig,
    patchGovernanceConfig: fake.patchGovernanceConfig,
    getJevCredential: fake.getJevCredential,
    putJevCredential: fake.putJevCredential,
    deleteJevCredential: fake.deleteJevCredential,
    listMembers: fake.listMembers,
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
vi.mock("../../navigation/context", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../navigation/context")>()),
  useNavigation: () => ({
    searchParams: new URLSearchParams(navigationState.search),
    hash: "",
    pathname: "/acme/settings",
    replace: vi.fn(),
    push: vi.fn(),
  }),
}));
vi.mock("@multica/ui/hooks/use-mobile", () => ({
  useIsMobile: () => false,
  useIsCompact: () => false,
}));

import { SettingsPage } from "./settings-page";

const SECRET = "sk-live-0123456789abcdef";
const DANA = "00000000-0000-4000-8000-0000000000d3";
const CHEESE = "00000000-0000-4000-8000-0000000000c2";

const audit = [
  {
    version: 14,
    request_id: "r14",
    actor_id: DANA,
    previous: { jev_squad_routing_mode: "shadow", jev_min_confidence: 0.7, rule_mode: "off", jev_governance_enabled: false },
    effective: { jev_squad_routing_mode: "shadow", jev_min_confidence: 0.8, rule_mode: "off", jev_governance_enabled: false },
    changed_at: "2026-10-15T15:40:00Z",
  },
  {
    version: 13,
    request_id: "r13",
    actor_id: CHEESE,
    previous: { jev_squad_routing_mode: "off", jev_min_confidence: 0.7, rule_mode: "off", jev_governance_enabled: false },
    effective: { jev_squad_routing_mode: "shadow", jev_min_confidence: 0.7, rule_mode: "off", jev_governance_enabled: false },
    changed_at: "2026-10-15T13:10:00Z",
  },
  {
    version: 12,
    request_id: "r12",
    actor_id: CHEESE,
    previous: { jev_squad_routing_mode: "on", jev_min_confidence: 0.7, rule_mode: "off", jev_governance_enabled: false },
    effective: { jev_squad_routing_mode: "off", jev_min_confidence: 0.7, rule_mode: "off", jev_governance_enabled: false },
    changed_at: "2026-10-14T11:05:00Z",
  },
  {
    version: 10,
    request_id: "r10",
    actor_id: DANA,
    previous: { jev_squad_routing_mode: "off", jev_min_confidence: 0.7, rule_mode: "off", jev_governance_enabled: false },
    effective: { jev_squad_routing_mode: "on", jev_min_confidence: 0.7, rule_mode: "off", jev_governance_enabled: false },
    changed_at: "2026-10-12T09:12:00Z",
  },
];

function config(version: number, settings: Record<string, unknown> = {}, history: unknown[] = audit) {
  return {
    version,
    control_epoch: 1,
    settings: { jev_squad_routing_mode: "shadow", jev_min_confidence: 0.8, ...settings },
    missing_requirements: [],
    operating_limits: [],
    audit_history: history,
    credential_present: false,
  };
}

let client: QueryClient;
function mount() {
  client = new QueryClient({
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

const problem = (status: number, problemName: string) =>
  new fake.FakeApiError("failed", status, "failed", { problem: problemName });

async function openTab(name: RegExp) {
  const tab = await screen.findByRole("tab", { name });
  fireEvent.click(tab);
  return tab;
}

async function openHistoryRow(version: number) {
  await openTab(/^History/);
  const row = await screen.findByRole("button", { name: new RegExp(`^v${version}\\b`) });
  fireEvent.click(row);
  return row;
}

beforeEach(() => {
  fake.role = "admin";
  navigationState.search = "tab=jev-governance";
  fake.getGovernanceConfig.mockReset().mockResolvedValue(config(14));
  fake.patchGovernanceConfig.mockReset();
  fake.getJevCredential.mockReset().mockResolvedValue({ present: true });
  fake.putJevCredential.mockReset().mockResolvedValue({ present: true });
  fake.deleteJevCredential.mockReset().mockResolvedValue(undefined);
  fake.listMembers.mockReset().mockResolvedValue([
    { user_id: DANA, role: "admin", name: "Dana Reyes" },
    { user_id: CHEESE, role: "owner", name: "Cheese" },
  ]);
  setOnline(true);
});

afterEach(() => setOnline(true));

describe("Jev / Governance tabs", () => {
  it("offers Routing, Credential and History as one tab group with linked panels", async () => {
    mount();
    const routing = await screen.findByRole("tab", { name: "Routing" });
    expect(routing).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tab", { name: /^Credential/ })).toHaveAccessibleName(/key set/i);
    expect(screen.getByRole("tab", { name: "History 4" })).toBeInTheDocument();
    expect(screen.getByRole("tabpanel", { name: "Routing" })).toBeVisible();
    fireEvent.keyDown(routing, { key: "ArrowRight" });
    expect(screen.getByRole("tab", { name: /^Credential/ })).toHaveFocus();
  });

  it("keeps a pending routing edit when the user looks at another tab and comes back", async () => {
    mount();
    fireEvent.click(await screen.findByRole("radio", { name: "Off" }));
    await openTab(/^History/);
    await openTab(/^Routing/);
    expect(screen.getByRole("radio", { name: "Off" })).toBeChecked();
    expect(screen.getByText("Based on v14")).toBeVisible();
  });
});

describe("Jev / Governance credential pane", () => {
  it("shows only whether a key is set, never a value", async () => {
    mount();
    await openTab(/^Credential/);
    expect(await screen.findByText("Set")).toBeVisible();
    expect(screen.getByText("Stored encrypted. It cannot be viewed after saving.")).toBeVisible();
    expect(screen.getByRole("button", { name: "Replace key" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Remove" })).toBeEnabled();
    expect(fake.getJevCredential).toHaveBeenCalledWith("ws-1");
  });

  it("replaces the key without keeping it anywhere after the save", async () => {
    mount();
    await openTab(/^Credential/);
    fireEvent.click(await screen.findByRole("button", { name: "Replace key" }));
    const dialog = await screen.findByRole("dialog", { name: "Replace Jev key" });
    const input = within(dialog).getByLabelText("API key");
    expect(input).toHaveAttribute("type", "password");
    expect(input).toHaveAttribute("autocomplete", "off");
    expect(within(dialog).getByText("Saved encrypted. The old key is replaced at once and cannot be viewed again.")).toBeVisible();
    expect(within(dialog).getByRole("button", { name: "Save key" })).toBeDisabled();
    fireEvent.change(input, { target: { value: `  ${SECRET}  ` } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save key" }));

    await waitFor(() => expect(fake.putJevCredential).toHaveBeenCalledWith("ws-1", SECRET));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(await screen.findByText("Key saved")).toBeVisible();
    expect(document.body.textContent).not.toContain(SECRET);

    fireEvent.click(screen.getByRole("button", { name: "Replace key" }));
    const reopened = await screen.findByRole("dialog", { name: "Replace Jev key" });
    expect(within(reopened).getByLabelText("API key")).toHaveValue("");

    const held = JSON.stringify([
      ...client.getQueryCache().getAll().map((query) => query.state.data),
      ...client.getMutationCache().getAll().map((mutation) => mutation.state.variables),
    ]);
    expect(held).not.toContain(SECRET);
  });

  it("keeps the dialog and the typed key when saving fails, and does not claim a key was saved", async () => {
    fake.getJevCredential.mockResolvedValue({ present: false });
    fake.putJevCredential.mockRejectedValueOnce(problem(503, "configuration_unavailable"));
    mount();
    await openTab(/^Credential/);
    expect(await screen.findByText("Not set")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "Set key" }));
    const dialog = await screen.findByRole("dialog", { name: "Set Jev key" });
    fireEvent.change(within(dialog).getByLabelText("API key"), { target: { value: SECRET } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save key" }));

    expect(await within(dialog).findByRole("alert")).toHaveTextContent("Couldn't save the key. Nothing was changed.");
    expect(within(dialog).getByLabelText("API key")).toHaveValue(SECRET);
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.getByText("Not set")).toBeVisible();
    expect(screen.queryByText("Key saved")).toBeNull();
  });

  it("explains a server without a credential encryption key and stops offering to set one", async () => {
    fake.getJevCredential.mockResolvedValue({ present: false });
    fake.putJevCredential.mockRejectedValueOnce(problem(403, "jev_credential_configuration_required"));
    mount();
    await openTab(/^Credential/);
    fireEvent.click(await screen.findByRole("button", { name: "Set key" }));
    const dialog = await screen.findByRole("dialog", { name: "Set Jev key" });
    fireEvent.change(within(dialog).getByLabelText("API key"), { target: { value: SECRET } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Save key" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(
      await screen.findByText("This server has no credential encryption key. Ask a deployment admin to configure one."),
    ).toBeVisible();
    expect(screen.getByRole("button", { name: "Set key" })).toBeDisabled();
  });

  it("warns on load and disables Set key when the server reports it cannot store a key", async () => {
    fake.getJevCredential.mockResolvedValue({ present: false, configurable: false });
    mount();
    await openTab(/^Credential/);
    expect(
      await screen.findByText("This server has no credential encryption key. Ask a deployment admin to configure one."),
    ).toBeVisible();
    expect(screen.getByTestId("jev-credential-unconfigured")).toBeVisible();
    expect(screen.getByRole("button", { name: "Set key" })).toBeDisabled();
    expect(fake.putJevCredential).not.toHaveBeenCalled();
  });

  it("keeps Remove available when a stored key exists but the server cannot store a new one", async () => {
    fake.getJevCredential.mockResolvedValue({ present: true, configurable: false });
    mount();
    await openTab(/^Credential/);
    expect(
      await screen.findByText("This server has no credential encryption key. Ask a deployment admin to configure one."),
    ).toBeVisible();
    expect(screen.getByRole("button", { name: "Replace key" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove" })).toBeEnabled();
  });

  it("shows the note to a member, who has no key actions", async () => {
    fake.role = "member";
    fake.getJevCredential.mockResolvedValue({ present: false, configurable: false });
    mount();
    await openTab(/^Credential/);
    expect(
      await screen.findByText("This server has no credential encryption key. Ask a deployment admin to configure one."),
    ).toBeVisible();
    expect(screen.queryByRole("button", { name: "Set key" })).toBeNull();
  });

  it("offers Set key without a warning when the server can store a key", async () => {
    fake.getJevCredential.mockResolvedValue({ present: false, configurable: true });
    mount();
    await openTab(/^Credential/);
    expect(await screen.findByRole("button", { name: "Set key" })).toBeEnabled();
    expect(screen.queryByText(/no credential encryption key/)).toBeNull();
  });

  it("keeps Set key enabled when an older server does not say whether it can store a key", async () => {
    fake.getJevCredential.mockResolvedValue({ present: false });
    mount();
    await openTab(/^Credential/);
    expect(await screen.findByRole("button", { name: "Set key" })).toBeEnabled();
    expect(screen.queryByText(/no credential encryption key/)).toBeNull();
  });

  it("removes the key only after a confirmation that says what stops working", async () => {
    mount();
    await openTab(/^Credential/);
    fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Remove Jev key?" });
    expect(
      within(dialog).getByText("Jev cannot make picks until a new key is set. In shadow mode, no picks are logged meanwhile."),
    ).toBeVisible();
    expect(fake.deleteJevCredential).not.toHaveBeenCalled();
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove key" }));
    await waitFor(() => expect(fake.deleteJevCredential).toHaveBeenCalledWith("ws-1"));
    expect(await screen.findByText("Key removed")).toBeVisible();
    expect(await screen.findByText("Not set")).toBeVisible();
  });

  it("keeps showing a key as set when removing it fails", async () => {
    fake.deleteJevCredential.mockRejectedValueOnce(problem(503, "configuration_unavailable"));
    mount();
    await openTab(/^Credential/);
    fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Remove Jev key?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Remove key" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("Couldn't remove the key. Nothing was changed.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.getByText("Set")).toBeVisible();
  });

  it("is read-only for a member, who still sees whether a key is set", async () => {
    fake.role = "member";
    mount();
    await openTab(/^Credential/);
    expect(await screen.findByText("Set")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Replace key" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Set key" })).toBeNull();
  });

  it("says it cannot read the key status, instead of showing Not set, and can retry", async () => {
    fake.getJevCredential.mockRejectedValueOnce(problem(503, "configuration_unavailable"));
    mount();
    await openTab(/^Credential/);
    expect(await screen.findByText("Can't load the key status")).toBeVisible();
    expect(screen.queryByText("Not set")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByText("Set")).toBeVisible();
  });

  it("pauses key changes while offline", async () => {
    mount();
    await openTab(/^Credential/);
    expect(await screen.findByText("Set")).toBeVisible();
    setOnline(false);
    await waitFor(() => expect(screen.getByRole("button", { name: "Replace key" })).toBeDisabled());
    expect(screen.getByRole("button", { name: "Remove" })).toBeDisabled();
  });
});

describe("Jev / Governance history pane", () => {
  it("lists versions newest first with who, when and what changed", async () => {
    mount();
    await openTab(/^History/);
    const rows = await screen.findAllByRole("button", { name: /^v\d+/ });
    expect(rows.map((row) => /^v\d+/.exec(row.textContent ?? "")?.[0])).toEqual(["v14", "v13", "v12", "v10"]);
    expect(within(rows[0]!).getByText("Current")).toBeVisible();
    expect(within(rows[0]!).getByText("Min confidence: 0.70 → 0.80")).toBeVisible();
    expect(within(rows[0]!).getByText(/Dana Reyes/)).toBeVisible();
    expect(within(rows[0]!).getByText(/22:40 ICT|10:40 PM ICT/)).toBeVisible();
    expect(within(rows[1]!).getByText("Routing mode: Off → Shadow")).toBeVisible();
    expect(screen.queryByText("Latest 50 versions.")).toBeNull();
  });

  it("shows the empty state of a workspace that never saved", async () => {
    fake.getGovernanceConfig.mockResolvedValue(config(0, {}, []));
    mount();
    await openTab(/^History/);
    expect(await screen.findByText("No changes yet")).toBeVisible();
    expect(screen.getByText("Settings are at their defaults (v0). Saved changes appear here.")).toBeVisible();
  });

  it("offers a rollback only for an older version, never for the current one", async () => {
    mount();
    await openHistoryRow(14);
    expect(screen.queryByRole("button", { name: /^Roll back to v14/ })).toBeNull();
    fireEvent.click(await screen.findByRole("button", { name: /^v12\b/ }));
    expect(screen.getByRole("button", { name: "Roll back to v12" })).toBeEnabled();
  });

  it("shows what a rollback changes before it asks, and does nothing on Cancel", async () => {
    mount();
    await openHistoryRow(12);
    fireEvent.click(screen.getByRole("button", { name: "Roll back to v12" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Roll back to v12?" });
    expect(
      within(dialog).getByText("This restores every governance setting to how it was after v12. It saves as a new version, v15. History is kept."),
    ).toBeVisible();
    const table = within(dialog).getByRole("table");
    expect(within(table).getByRole("columnheader", { name: "Now (v14)" })).toBeVisible();
    expect(within(table).getByRole("columnheader", { name: "After (v12)" })).toBeVisible();
    expect(within(table).getByRole("row", { name: /Routing mode\s+Shadow\s+Off/ })).toBeVisible();
    expect(within(table).getByRole("row", { name: /Min confidence\s+0\.80\s+0\.70/ })).toBeVisible();
    expect(within(dialog).getByText("2 other settings stay the same.")).toBeVisible();
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(fake.patchGovernanceConfig).not.toHaveBeenCalled();
  });

  it("rolls back with the version it showed, then refreshes from the confirmed result", async () => {
    const after = config(15, { jev_squad_routing_mode: "off", jev_min_confidence: 0.7 }, [
      {
        version: 15,
        request_id: "r15",
        actor_id: DANA,
        previous: audit[0]!.effective,
        effective: audit[2]!.effective,
        changed_at: "2026-10-16T01:00:00Z",
      },
      ...audit,
    ]);
    fake.patchGovernanceConfig.mockResolvedValue(after);
    mount();
    await openHistoryRow(12);
    fireEvent.click(screen.getByRole("button", { name: "Roll back to v12" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Roll back to v12?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Roll back to v12" }));

    await waitFor(() =>
      expect(fake.patchGovernanceConfig).toHaveBeenCalledWith("ws-1", {
        expected_version: 14,
        rollback_to_version: 12,
        request_id: expect.stringMatching(/^[0-9a-f-]{36}$/),
      }),
    );
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(await screen.findByText("Rolled back to v12")).toBeVisible();
    expect(screen.getByRole("tab", { name: "History 5" })).toBeInTheDocument();
    await openTab(/^Routing/);
    expect(await screen.findByText("v15 · Off")).toBeVisible();
    expect(screen.getByRole("textbox", { name: "Min confidence" })).toHaveValue("0.70");
  });

  it("does not offer to restore a version that would turn routing on", async () => {
    mount();
    await openHistoryRow(10);
    const button = screen.getByRole("button", { name: "Roll back to v10" });
    expect(button).toBeDisabled();
    expect(screen.getByText("Can't be restored: it would turn routing on.")).toBeVisible();
    fireEvent.click(button);
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(fake.patchGovernanceConfig).not.toHaveBeenCalled();
  });

  it("says the settings changed when the version moved, and reloads before another try", async () => {
    fake.patchGovernanceConfig.mockRejectedValue(problem(409, "configuration_revision_conflict"));
    mount();
    await openHistoryRow(12);
    fireEvent.click(screen.getByRole("button", { name: "Roll back to v12" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Roll back to v12?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Roll back to v12" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("Not rolled back: settings changed");

    fake.getGovernanceConfig.mockResolvedValue(config(15, { jev_min_confidence: 0.6 }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Reload and review" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(await screen.findByRole("tab", { name: "History 4" })).toBeInTheDocument();
    await openTab(/^Routing/);
    expect(await screen.findByText("v15 · Shadow")).toBeVisible();
  });

  it("keeps the dialog when the reload after a conflict fails", async () => {
    fake.patchGovernanceConfig.mockRejectedValue(problem(409, "configuration_revision_conflict"));
    mount();
    await openHistoryRow(12);
    fireEvent.click(screen.getByRole("button", { name: "Roll back to v12" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Roll back to v12?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Roll back to v12" }));
    await within(dialog).findByRole("alert");
    fake.getGovernanceConfig.mockRejectedValue(problem(503, "configuration_unavailable"));
    fireEvent.click(within(dialog).getByRole("button", { name: "Reload and review" }));
    await waitFor(() => expect(fake.getGovernanceConfig).toHaveBeenCalledTimes(2));
    expect(screen.getByRole("alertdialog", { name: "Roll back to v12?" })).toBeVisible();
    expect(within(dialog).getByRole("alert")).toBeVisible();
  });

  it("reports another failure plainly and changes nothing", async () => {
    fake.patchGovernanceConfig.mockRejectedValue(problem(409, "configuration_rollback_unavailable"));
    mount();
    await openHistoryRow(12);
    fireEvent.click(screen.getByRole("button", { name: "Roll back to v12" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Roll back to v12?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Roll back to v12" }));
    expect(await within(dialog).findByRole("alert")).toHaveTextContent("Couldn't roll back. Nothing was changed.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
    await openTab(/^Routing/);
    expect(await screen.findByText("v14 · Shadow")).toBeVisible();
  });

  it("is read-only for a member, who still reads the history", async () => {
    fake.role = "member";
    mount();
    await openHistoryRow(12);
    expect(screen.getByRole("region", { name: "Version v12" })).toBeVisible();
    expect(screen.queryByRole("button", { name: /^Roll back/ })).toBeNull();
  });

  it("pauses rollbacks while offline", async () => {
    mount();
    await openHistoryRow(12);
    setOnline(false);
    await waitFor(() => expect(screen.getByRole("button", { name: "Roll back to v12" })).toBeDisabled());
  });
});
