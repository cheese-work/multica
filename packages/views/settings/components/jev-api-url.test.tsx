import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { renderWithI18n } from "../../test/i18n";

// CHE-1880: the Jev API URL beside the provider key on the Credential pane
// (design: design/jev-governance-tab/rev-4/C.html, Credential pane). Drives the
// real Settings page with every other tab stubbed and the API faked.
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

const DANA = "00000000-0000-4000-8000-0000000000d3";
const HINT = "https://api.typesafe.ai/v1";

function config(version: number, settings: Record<string, unknown> = {}, history: unknown[] = []) {
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

async function openCredential() {
  fireEvent.click(await screen.findByRole("tab", { name: /^Credential/ }));
  return screen.findByRole("textbox", { name: "API URL" });
}

beforeEach(() => {
  fake.role = "admin";
  navigationState.search = "tab=jev-governance";
  fake.getGovernanceConfig.mockReset().mockResolvedValue(config(14));
  fake.patchGovernanceConfig.mockReset();
  fake.getJevCredential.mockReset().mockResolvedValue({ present: true });
  fake.putJevCredential.mockReset();
  fake.deleteJevCredential.mockReset();
  fake.listMembers.mockReset().mockResolvedValue([{ user_id: DANA, role: "admin", name: "Dana Reyes" }]);
  setOnline(true);
});

afterEach(() => setOnline(true));

describe("Jev API URL on the Credential pane", () => {
  it("shows the default endpoint and the backend-reachability hint when no URL is set", async () => {
    mount();
    const input = await openCredential();
    expect(input).toHaveValue("");
    expect(input).toHaveAttribute("placeholder", HINT);
    expect(screen.getByText(/reachable from the backend container/i)).toHaveTextContent(`Empty uses ${HINT}.`);
    expect(screen.getByText(/reachable from the backend container/i)).toHaveTextContent("http://sub2api:8080/v1");
    expect(screen.getByRole("button", { name: "Save URL" })).toBeDisabled();
    expect(screen.queryByRole("button", { name: "Clear URL" })).toBeNull();
  });

  it("saves a trimmed URL against the version it read, then shows the confirmed value", async () => {
    fake.patchGovernanceConfig.mockResolvedValue(config(15, { jev_api_url: "http://sub2api:8080/v1" }));
    mount();
    const input = (await openCredential())!;
    fireEvent.change(input, { target: { value: "  http://sub2api:8080/v1  " } });
    fireEvent.click(screen.getByRole("button", { name: "Save URL" }));

    await waitFor(() =>
      expect(fake.patchGovernanceConfig).toHaveBeenCalledWith("ws-1", {
        expected_version: 14,
        request_id: expect.stringMatching(/^[0-9a-f-]{36}$/),
        jev_api_url: "http://sub2api:8080/v1",
      }),
    );
    expect(await screen.findByText("URL saved")).toBeVisible();
    expect(screen.getByRole("textbox", { name: "API URL" })).toHaveValue("http://sub2api:8080/v1");
    expect(screen.getByRole("button", { name: "Clear URL" })).toBeEnabled();
  });

  it("clears a saved URL by sending an empty one", async () => {
    fake.getGovernanceConfig.mockResolvedValue(config(14, { jev_api_url: "http://sub2api:8080/v1" }));
    fake.patchGovernanceConfig.mockResolvedValue(config(15));
    mount();
    expect(await openCredential()).toHaveValue("http://sub2api:8080/v1");
    fireEvent.click(screen.getByRole("button", { name: "Clear URL" }));

    await waitFor(() =>
      expect(fake.patchGovernanceConfig).toHaveBeenCalledWith("ws-1", {
        expected_version: 14,
        request_id: expect.stringMatching(/^[0-9a-f-]{36}$/),
        jev_api_url: "",
      }),
    );
    expect(await screen.findByText("URL cleared")).toBeVisible();
    expect(screen.getByRole("textbox", { name: "API URL" })).toHaveValue("");
  });

  it.each(["ftp://jev.example/v1", "http://", "jev.example/v1", "not a url"])(
    "rejects %s before sending: only http or https with a host",
    async (bad) => {
      mount();
      const input = (await openCredential())!;
      fireEvent.change(input, { target: { value: bad } });
      expect(await screen.findByText("Enter an http:// or https:// URL with a host.")).toBeVisible();
      expect(input).toHaveAttribute("aria-invalid", "true");
      expect(screen.getByRole("button", { name: "Save URL" })).toBeDisabled();
      expect(fake.patchGovernanceConfig).not.toHaveBeenCalled();
    },
  );

  it("tells the user to reload when the settings changed elsewhere", async () => {
    fake.patchGovernanceConfig.mockRejectedValue(
      new fake.FakeApiError("failed", 409, "conflict", { problem: "revision_conflict" }),
    );
    mount();
    const input = (await openCredential())!;
    fireEvent.change(input, { target: { value: "https://jev.example/v1" } });
    fireEvent.click(screen.getByRole("button", { name: "Save URL" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("Settings changed elsewhere. Reload the page, then try again.");
    expect(screen.getByRole("textbox", { name: "API URL" })).toHaveValue("https://jev.example/v1");
  });

  it("surfaces a server failure and keeps the typed URL", async () => {
    fake.patchGovernanceConfig.mockRejectedValue(
      new fake.FakeApiError("failed", 400, "bad request", { problem: "invalid_request" }),
    );
    mount();
    const input = (await openCredential())!;
    fireEvent.change(input, { target: { value: "https://jev.example/v1" } });
    fireEvent.click(screen.getByRole("button", { name: "Save URL" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't save the URL. Nothing was changed.");
    expect(screen.getByRole("textbox", { name: "API URL" })).toHaveValue("https://jev.example/v1");
    expect(screen.getByRole("button", { name: "Save URL" })).toBeEnabled();
  });

  it("shows members the value read-only, or the default when none is set", async () => {
    fake.role = "member";
    fake.getGovernanceConfig.mockResolvedValue(config(14, { jev_api_url: "http://sub2api:8080/v1" }));
    const view = mount();
    fireEvent.click(await screen.findByRole("tab", { name: /^Credential/ }));
    expect(await screen.findByText("http://sub2api:8080/v1")).toBeVisible();
    expect(screen.queryByRole("textbox", { name: "API URL" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Save URL" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Clear URL" })).toBeNull();
    view.unmount();

    fake.getGovernanceConfig.mockResolvedValue(config(14));
    mount();
    fireEvent.click(await screen.findByRole("tab", { name: /^Credential/ }));
    expect(await screen.findByText(`${HINT} (default)`)).toBeVisible();
  });

  it("pauses saving while offline", async () => {
    mount();
    const input = (await openCredential())!;
    fireEvent.change(input, { target: { value: "https://jev.example/v1" } });
    setOnline(false);
    await waitFor(() => expect(screen.getByRole("button", { name: "Save URL" })).toBeDisabled());
  });
});

describe("Jev API URL in History", () => {
  const withUrl = { jev_squad_routing_mode: "shadow", jev_min_confidence: 0.8, jev_api_url: "http://sub2api:8080/v1" };
  const without = { jev_squad_routing_mode: "shadow", jev_min_confidence: 0.8 };
  const audit = [
    { version: 15, request_id: "r15", actor_id: DANA, previous: without, effective: withUrl, changed_at: "2026-10-20T01:00:00Z" },
    { version: 14, request_id: "r14", actor_id: DANA, previous: null, effective: without, changed_at: "2026-10-19T01:00:00Z" },
  ];

  it("lists a URL save and rolls it back through the existing rollback", async () => {
    fake.getGovernanceConfig.mockResolvedValue(config(15, { jev_api_url: "http://sub2api:8080/v1" }, audit));
    fake.patchGovernanceConfig.mockResolvedValue(config(16, {}, audit));
    mount();
    fireEvent.click(await screen.findByRole("tab", { name: /^History/ }));
    fireEvent.click(await screen.findByRole("button", { name: /^v14\b/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Roll back to v14" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Roll back to v14?" });
    expect(within(dialog).getByRole("row", { name: /Jev API address/ })).toBeVisible();
    fireEvent.click(within(dialog).getByRole("button", { name: "Roll back to v14" }));

    await waitFor(() =>
      expect(fake.patchGovernanceConfig).toHaveBeenCalledWith("ws-1", {
        expected_version: 15,
        rollback_to_version: 14,
        request_id: expect.stringMatching(/^[0-9a-f-]{36}$/),
      }),
    );
  });
});
