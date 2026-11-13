import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

const mockUpdateWorkspace = vi.hoisted(() => vi.fn());
const mockInvalidateQueries = vi.hoisted(() => vi.fn());
const mockToastSuccess = vi.hoisted(() => vi.fn());
const workspaceRef = vi.hoisted(() => ({
  current: {
    id: "workspace-1",
    name: "Test Workspace",
    slug: "test-workspace",
    description: "",
    context: "",
    issue_prefix: "TES",
    repos: [] as { url: string }[],
    settings: {} as Record<string, unknown>,
  },
}));
const membersRef = vi.hoisted(() => ({
  current: [
    { user_id: "user-1", role: "owner" as "owner" | "admin" | "member", name: "Ada" },
  ] as { user_id: string; role: "owner" | "admin" | "member"; name?: string }[],
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: membersRef.current, isFetched: true }),
  useQueryClient: () => ({
    setQueryData: vi.fn(),
    getQueryData: vi.fn(() => []),
    invalidateQueries: mockInvalidateQueries,
  }),
}));

vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => workspaceRef.current,
  useHasOnboarded: () => true,
  resolvePostAuthDestination: () => "/",
}));

vi.mock("@multica/core/platform", () => ({
  setCurrentWorkspace: vi.fn(),
}));

vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"], queryFn: vi.fn() }),
  workspaceListOptions: () => ({ queryKey: ["workspaces"], queryFn: vi.fn() }),
  workspaceKeys: { list: () => ["workspaces"] },
}));

vi.mock("@multica/core/issues/queries", () => ({
  issueKeys: { all: (workspaceId: string) => ["issues", workspaceId] },
}));

vi.mock("@multica/core/workspace/mutations", () => ({
  useLeaveWorkspace: () => ({ mutateAsync: vi.fn() }),
  useDeleteWorkspace: () => ({ mutateAsync: vi.fn() }),
  useSetWorkspaceScheduling: () => ({ mutate: vi.fn(), isPending: false }),
}));

vi.mock("@multica/core/api", () => ({
  api: {
    updateWorkspace: mockUpdateWorkspace,
    getBaseUrl: () => "http://127.0.0.1:8080",
  },
}));

vi.mock("@multica/core/auth", () => {
  const useAuthStore = Object.assign(
    (selector?: (state: { user: { id: string } }) => unknown) =>
      selector ? selector({ user: { id: "user-1" } }) : { user: { id: "user-1" } },
    { getState: () => ({ user: { id: "user-1" } }) },
  );
  return { useAuthStore };
});

vi.mock("../../navigation", () => ({
  useNavigation: () => ({ push: vi.fn() }),
}));

vi.mock("./agent-decisions-row", () => ({
  AgentDecisionsRow: () => <div data-testid="agent-decisions-row" />,
}));

vi.mock("./routing-agent-rows", () => ({
  RoutingAgentRows: ({ canEdit }: { canEdit: boolean }) => (
    <div data-testid="routing-agent-rows" data-can-edit={String(canEdit)} />
  ),
}));

vi.mock("./delegated-writes-section", () => ({
  DelegatedWritesSection: ({ canEdit }: { canEdit: boolean }) => (
    <div data-testid="delegated-writes-section" data-can-edit={String(canEdit)} />
  ),
}));

vi.mock("./delete-workspace-dialog", () => ({
  DeleteWorkspaceDialog: () => null,
}));

vi.mock("sonner", () => ({
  toast: { success: mockToastSuccess, error: vi.fn() },
}));

import { WorkspaceTab } from "./workspace-tab";

const TEST_RESOURCES = {
  en: { common: enCommon, settings: enSettings },
};

function I18nWrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

describe("WorkspaceTab — automatic updates", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    workspaceRef.current = {
      id: "workspace-1",
      name: "Test Workspace",
      slug: "test-workspace",
      description: "",
      context: "",
      issue_prefix: "TES",
      repos: [],
      settings: {},
    };
    membersRef.current = [{ user_id: "user-1", role: "owner", name: "Ada" }];
    mockUpdateWorkspace.mockImplementation(
      async (_id: string, payload: Record<string, unknown>) => ({
        ...workspaceRef.current,
        ...payload,
        issue_prefix:
          (payload.issue_prefix as string | undefined) ?? workspaceRef.current.issue_prefix,
      }),
    );
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  function setupUser() {
    return userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
  }

  it("shows the prefix and slug as values, not editable fields", () => {
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByText("TES")).toBeInTheDocument();
    expect(screen.getByText("test-workspace")).toBeInTheDocument();
    expect(screen.queryByRole("textbox", { name: "Slug" })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Save$/ })).toBeNull();
  });

  it("auto-saves ordinary workspace fields silently, without invalidating issue caches", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });
    const nameInput = screen.getByDisplayValue("Test Workspace");

    await user.clear(nameInput);
    await user.type(nameInput, "Renamed Workspace");
    await user.tab();

    await waitFor(() => {
      expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", {
        name: "Renamed Workspace",
        description: "",
        context: "",
      });
    });
    // The inline save state reports success; a toast would repeat it.
    expect(mockToastSuccess).not.toHaveBeenCalled();
    expect(mockInvalidateQueries).not.toHaveBeenCalled();
  });

  it("changes the prefix only from its own dialog, after previewing the result", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    await user.click(screen.getByRole("button", { name: "Change prefix..." }));
    const dialog = await screen.findByRole("dialog", { name: "Change issue prefix" });
    const input = within(dialog).getByRole("textbox", { name: "New prefix" });

    await user.clear(input);
    await user.type(input, "ab-12!cd");
    expect(input).toHaveValue("AB12CD");
    expect(within(dialog).getByText("TES-123")).toBeInTheDocument();
    expect(within(dialog).getByText("AB12CD-123")).toBeInTheDocument();
    expect(mockUpdateWorkspace).not.toHaveBeenCalled();

    await user.click(within(dialog).getByRole("button", { name: "Change to AB12CD" }));

    await waitFor(() => {
      expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", {
        issue_prefix: "AB12CD",
      });
    });
    expect(mockInvalidateQueries).toHaveBeenCalledWith({
      queryKey: ["issues", "workspace-1"],
    });
    await waitFor(() => {
      expect(screen.queryByRole("dialog", { name: "Change issue prefix" })).toBeNull();
    });
  });

  it("does not persist a prefix when the dialog is cancelled", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    await user.click(screen.getByRole("button", { name: "Change prefix..." }));
    const dialog = await screen.findByRole("dialog", { name: "Change issue prefix" });
    await user.type(within(dialog).getByRole("textbox", { name: "New prefix" }), "X");
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));

    expect(mockUpdateWorkspace).not.toHaveBeenCalled();
  });

  it("cannot confirm an empty or unchanged prefix", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    await user.click(screen.getByRole("button", { name: "Change prefix..." }));
    const dialog = await screen.findByRole("dialog", { name: "Change issue prefix" });
    const input = within(dialog).getByRole("textbox", { name: "New prefix" });
    expect(within(dialog).getByRole("button", { name: "Change to TES" })).toBeDisabled();

    await user.clear(input);
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(within(dialog).getByRole("button", { name: /^Change to/ })).toBeDisabled();
  });

  it("shows regular members the values read-only, with who to ask", () => {
    membersRef.current = [
      { user_id: "user-1", role: "member" },
      { user_id: "user-2", role: "owner", name: "Grace Hopper" },
    ];
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByRole("note")).toHaveTextContent("Ask Grace Hopper");
    expect(screen.queryByDisplayValue("Test Workspace")).toBeNull();
    expect(screen.getByText("Test Workspace")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Change prefix..." })).toBeNull();
  });

  // CHE-1612: the compact prompt saves through its own field, never the
  // settings blob, so other workspace settings cannot be dropped.
  it("saves only the compact prompt and shows the built-in default as its source", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });
    expect(screen.getByText("Source: built-in default")).toBeInTheDocument();

    await user.type(screen.getByRole("textbox", { name: "Compact prompt" }), "Keep SHAs.");
    await user.tab();

    await waitFor(() => {
      expect(mockUpdateWorkspace).toHaveBeenLastCalledWith("workspace-1", {
        compact_prompt: "Keep SHAs.",
      });
    });
    for (const [, payload] of mockUpdateWorkspace.mock.calls) {
      expect(Object.keys(payload)).toEqual(["compact_prompt"]);
    }
  });

  it("resets a cleared compact prompt to the built-in default", async () => {
    workspaceRef.current = {
      ...workspaceRef.current,
      settings: { compact_prompt: "Old criteria.", other: true },
    };
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });
    expect(screen.getByText("Source: workspace")).toBeInTheDocument();

    await user.clear(screen.getByRole("textbox", { name: "Compact prompt" }));
    await user.tab();

    await waitFor(() => {
      expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", { compact_prompt: null });
    });
  });

  // CHE-1612 review: an open, untouched page must follow a prompt saved
  // elsewhere (the CLI), never write its stale draft back over it.
  it("shows a compact prompt saved elsewhere without writing the untouched field back", async () => {
    const view = render(<WorkspaceTab />, { wrapper: I18nWrapper });
    expect(screen.getByRole("textbox", { name: "Compact prompt" })).toHaveValue("");

    workspaceRef.current = {
      ...workspaceRef.current,
      settings: { compact_prompt: "Saved from CLI." },
    };
    view.rerender(<WorkspaceTab />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(700);
    });

    expect(mockUpdateWorkspace).not.toHaveBeenCalled();
    expect(screen.getByRole("textbox", { name: "Compact prompt" })).toHaveValue("Saved from CLI.");
    expect(screen.getByText("Source: workspace")).toBeInTheDocument();
  });

  it.each([
    ["owner", true],
    ["admin", false],
    ["member", false],
  ] as const)("shows the agent decisions row to a %s: %s", (role, shown) => {
    membersRef.current = [{ user_id: "user-1", role, name: "Ada" }];
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.queryByTestId("agent-decisions-row") !== null).toBe(shown);
  });

  it.each([
    ["owner", "true"],
    ["admin", "true"],
    ["member", "false"],
  ] as const)("lets a %s edit delegated governed writes: %s", (role, canEdit) => {
    membersRef.current = [{ user_id: "user-1", role, name: "Ada" }];
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByTestId("delegated-writes-section")).toHaveAttribute("data-can-edit", canEdit);
  });

  it.each([
    ["owner", "true"],
    ["admin", "true"],
    ["member", "false"],
  ] as const)("lets a %s edit the routing agent: %s", (role, canEdit) => {
    membersRef.current = [{ user_id: "user-1", role, name: "Ada" }];
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByTestId("routing-agent-rows")).toHaveAttribute("data-can-edit", canEdit);
  });

  // CHE-1806: settings.auto_compact saves through its own request field.
  it("shows the auto-compact defaults when nothing is saved", () => {
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByRole("switch", { name: "Auto-compact" })).toBeChecked();
    expect(screen.getByRole("spinbutton", { name: "Threshold (%)" })).toHaveValue(80);
  });

  it("shows a saved auto-compact setting", () => {
    workspaceRef.current = {
      ...workspaceRef.current,
      settings: { auto_compact: { enabled: false, threshold_pct: 60 } },
    };
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByRole("switch", { name: "Auto-compact" })).not.toBeChecked();
    expect(screen.getByRole("spinbutton", { name: "Threshold (%)" })).toHaveValue(60);
  });

  it("falls back to the defaults for a malformed saved auto-compact setting", () => {
    workspaceRef.current = {
      ...workspaceRef.current,
      settings: { auto_compact: { enabled: "yes", threshold_pct: 12 } },
    };
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByRole("switch", { name: "Auto-compact" })).toBeChecked();
    expect(screen.getByRole("spinbutton", { name: "Threshold (%)" })).toHaveValue(80);
  });

  it("saves only the Enabled switch, without waiting for a blur", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    await user.click(screen.getByRole("switch", { name: "Auto-compact" }));

    await waitFor(() => {
      expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", {
        auto_compact: { enabled: false },
      });
    });
    for (const [, payload] of mockUpdateWorkspace.mock.calls) {
      expect(Object.keys(payload)).toEqual(["auto_compact"]);
    }
  });

  it("saves only the threshold after the field loses focus", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });
    const input = screen.getByRole("spinbutton", { name: "Threshold (%)" });

    await user.clear(input);
    await user.type(input, "70");
    await user.tab();

    await waitFor(() => {
      expect(mockUpdateWorkspace).toHaveBeenLastCalledWith("workspace-1", {
        auto_compact: { threshold_pct: 70 },
      });
    });
    for (const [, payload] of mockUpdateWorkspace.mock.calls) {
      expect(Object.keys(payload)).toEqual(["auto_compact"]);
    }
  });

  it("never sends a threshold outside 50 to 90", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });
    const input = screen.getByRole("spinbutton", { name: "Threshold (%)" });

    await user.clear(input);
    await user.type(input, "95");
    await user.tab();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(700);
    });

    expect(mockUpdateWorkspace).not.toHaveBeenCalled();
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText("Enter a whole number from 50 to 90.")).toBeInTheDocument();
  });

  // CHE-4331: per-window token thresholds, saved through the same field.
  it("shows the per-window token defaults when nothing is saved", () => {
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByRole("spinbutton", { name: "Threshold, 200k models (tokens)" })).toHaveValue(190000);
    expect(screen.getByRole("spinbutton", { name: "Threshold, 1M models (tokens)" })).toHaveValue(200000);
  });

  it("saves only the 1M token threshold after the field loses focus", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });
    const input = screen.getByRole("spinbutton", { name: "Threshold, 1M models (tokens)" });

    await user.clear(input);
    await user.type(input, "300000");
    await user.tab();

    await waitFor(() => {
      expect(mockUpdateWorkspace).toHaveBeenLastCalledWith("workspace-1", {
        auto_compact: { threshold_tokens_1m: 300000 },
      });
    });
  });

  it("never sends a 1M token threshold above 400000", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });
    const input = screen.getByRole("spinbutton", { name: "Threshold, 1M models (tokens)" });

    await user.clear(input);
    await user.type(input, "500000");
    await user.tab();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(700);
    });

    expect(mockUpdateWorkspace).not.toHaveBeenCalled();
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText("Enter a whole number from 100000 to 400000.")).toBeInTheDocument();
  });

  it("shows regular members the auto-compact setting read-only", () => {
    membersRef.current = [
      { user_id: "user-1", role: "member" },
      { user_id: "user-2", role: "owner", name: "Grace Hopper" },
    ];
    workspaceRef.current = {
      ...workspaceRef.current,
      settings: { auto_compact: { threshold_pct: 70 } },
    };
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.queryByRole("switch", { name: "Auto-compact" })).toBeNull();
    expect(screen.queryByRole("spinbutton", { name: "Threshold (%)" })).toBeNull();
    expect(screen.getByText("On, at 70%")).toBeInTheDocument();
  });

  // CHE-3706: settings.harness_profile (UI "Runtime profile") saves through its
  // own request field.
  it("shows the personal runtime profile when nothing is saved", () => {
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByRole("combobox", { name: "Runtime profile" })).toHaveTextContent("Personal");
  });

  it("shows a saved managed runtime profile", () => {
    workspaceRef.current = { ...workspaceRef.current, settings: { harness_profile: "managed" } };
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByRole("combobox", { name: "Runtime profile" })).toHaveTextContent("Managed");
  });

  it("falls back to personal for a malformed saved runtime profile", () => {
    workspaceRef.current = {
      ...workspaceRef.current,
      settings: { harness_profile: "managed_sandbox" },
    };
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByRole("combobox", { name: "Runtime profile" })).toHaveTextContent("Personal");
  });

  it("saves only the chosen runtime profile", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    await user.click(screen.getByRole("combobox", { name: "Runtime profile" }));
    await user.click(await screen.findByRole("option", { name: "Managed" }));

    await waitFor(() => {
      expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", {
        harness_profile: "managed",
      });
    });
    for (const [, payload] of mockUpdateWorkspace.mock.calls) {
      expect(Object.keys(payload)).toEqual(["harness_profile"]);
    }
  });

  it("shows regular members the runtime profile read-only", () => {
    membersRef.current = [
      { user_id: "user-1", role: "member" },
      { user_id: "user-2", role: "owner", name: "Grace Hopper" },
    ];
    workspaceRef.current = { ...workspaceRef.current, settings: { harness_profile: "managed" } };
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.queryByRole("combobox", { name: "Runtime profile" })).toBeNull();
    expect(screen.getByText("Managed")).toBeInTheDocument();
  });

  // CHE-3296: settings.idle_started_guard saves through its own request field,
  // for the owner only.
  it("shows the idle guard defaults when nothing is saved", () => {
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByRole("combobox", { name: "Idle-issue guard" })).toHaveTextContent("Off");
    expect(screen.getByRole("spinbutton", { name: "After (hours)" })).toHaveValue(4);
    expect(
      screen.getByText(
        "Moves started issues with no run and no wakeup after N hours. Shadow only logs.",
      ),
    ).toBeInTheDocument();
  });

  it("keeps the chosen idle guard mode and hours after a reload, leaving other settings alone", async () => {
    const user = setupUser();
    const others = {
      compact_prompt: "Keep SHAs.",
      auto_compact: { enabled: false, threshold_pct: 60 },
      nested: { keep: "me" },
    };
    let server = { ...workspaceRef.current, settings: { ...others } as Record<string, unknown> };
    workspaceRef.current = server;
    // Stand-in for the server: the dedicated field merges into one settings
    // key, and a `settings` blob replaces them all.
    mockUpdateWorkspace.mockImplementation(async (_id: string, body: Record<string, unknown>) => {
      const saved = (server.settings.idle_started_guard ?? {}) as Record<string, unknown>;
      server = {
        ...server,
        settings: (body.settings as Record<string, unknown>) ?? {
          ...server.settings,
          idle_started_guard: { ...saved, ...(body.idle_started_guard as object) },
        },
      };
      return server;
    });
    const first = render(<WorkspaceTab />, { wrapper: I18nWrapper });

    await user.click(screen.getByRole("combobox", { name: "Idle-issue guard" }));
    await user.click(await screen.findByRole("option", { name: "Shadow" }));
    await waitFor(() => expect(server.settings.idle_started_guard).toEqual({ mode: "shadow" }));
    const hours = screen.getByRole("spinbutton", { name: "After (hours)" });
    await user.clear(hours);
    await user.type(hours, "6");
    await user.tab();
    await waitFor(() =>
      expect(server.settings.idle_started_guard).toEqual({ mode: "shadow", after_hours: 6 }),
    );

    first.unmount();
    workspaceRef.current = server;
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByRole("combobox", { name: "Idle-issue guard" })).toHaveTextContent("Shadow");
    expect(screen.getByRole("spinbutton", { name: "After (hours)" })).toHaveValue(6);
    expect(server.settings).toMatchObject(others);
  });

  it("saves the idle guard one request at a time, in the order the owner made them", async () => {
    const user = setupUser();
    const releases: (() => void)[] = [];
    mockUpdateWorkspace.mockImplementation(
      (_id: string, body: unknown) =>
        new Promise((resolve) => releases.push(() => resolve({ ...workspaceRef.current, body }))),
    );
    render(<WorkspaceTab />, { wrapper: I18nWrapper });
    const hours = screen.getByRole("spinbutton", { name: "After (hours)" });

    await user.clear(hours);
    await user.type(hours, "8");
    await user.click(screen.getByRole("combobox", { name: "Idle-issue guard" }));
    await user.click(await screen.findByRole("option", { name: "Act" }));

    await waitFor(() => expect(mockUpdateWorkspace).toHaveBeenCalledTimes(1));
    expect(mockUpdateWorkspace).toHaveBeenLastCalledWith("workspace-1", {
      idle_started_guard: { after_hours: 8 },
    });
    releases[0]?.();
    await waitFor(() => expect(mockUpdateWorkspace).toHaveBeenCalledTimes(2));
    expect(mockUpdateWorkspace).toHaveBeenLastCalledWith("workspace-1", {
      idle_started_guard: { mode: "act" },
    });
    releases[1]?.();
  });

  it("shows the saved idle guard", () => {
    workspaceRef.current = {
      ...workspaceRef.current,
      settings: { idle_started_guard: { mode: "act", after_hours: 9 } },
    };
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.getByRole("combobox", { name: "Idle-issue guard" })).toHaveTextContent("Act");
    expect(screen.getByRole("spinbutton", { name: "After (hours)" })).toHaveValue(9);
  });

  it("saves only the chosen idle guard mode", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    await user.click(screen.getByRole("combobox", { name: "Idle-issue guard" }));
    await user.click(await screen.findByRole("option", { name: "Shadow" }));

    await waitFor(() => {
      expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", {
        idle_started_guard: { mode: "shadow" },
      });
    });
    for (const [, payload] of mockUpdateWorkspace.mock.calls) {
      expect(Object.keys(payload)).toEqual(["idle_started_guard"]);
    }
  });

  it("saves only the idle guard hours after the field loses focus", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });
    const input = screen.getByRole("spinbutton", { name: "After (hours)" });

    await user.clear(input);
    await user.type(input, "6");
    await user.tab();

    await waitFor(() => {
      expect(mockUpdateWorkspace).toHaveBeenLastCalledWith("workspace-1", {
        idle_started_guard: { after_hours: 6 },
      });
    });
    for (const [, payload] of mockUpdateWorkspace.mock.calls) {
      expect(Object.keys(payload)).toEqual(["idle_started_guard"]);
    }
  });

  it("never sends idle guard hours outside 1 to 72", async () => {
    const user = setupUser();
    render(<WorkspaceTab />, { wrapper: I18nWrapper });
    const input = screen.getByRole("spinbutton", { name: "After (hours)" });

    await user.clear(input);
    await user.type(input, "73");
    await user.tab();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(700);
    });

    expect(mockUpdateWorkspace).not.toHaveBeenCalled();
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText("Enter a whole number from 1 to 72.")).toBeInTheDocument();
  });

  it.each(["admin", "member"] as const)("shows a %s the idle guard read-only", (role) => {
    membersRef.current = [
      { user_id: "user-1", role },
      { user_id: "user-2", role: "owner", name: "Grace Hopper" },
    ];
    workspaceRef.current = {
      ...workspaceRef.current,
      settings: { idle_started_guard: { mode: "shadow", after_hours: 6 } },
    };
    render(<WorkspaceTab />, { wrapper: I18nWrapper });

    expect(screen.queryByRole("combobox", { name: "Idle-issue guard" })).toBeNull();
    expect(screen.queryByRole("spinbutton", { name: "After (hours)" })).toBeNull();
    expect(screen.getByText("Shadow, after 6 h")).toBeInTheDocument();
  });
  it("restores name, description, context and compact prompt after unmount", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    const first = render(<WorkspaceTab />, { wrapper: I18nWrapper });
    const nameInput = screen.getByDisplayValue("Test Workspace");
    await user.clear(nameInput);
    await user.type(nameInput, "Unsent name");
    // Leave before the auto-save debounce fires.
    first.unmount();

    render(<WorkspaceTab />, { wrapper: I18nWrapper });
    expect(screen.getByDisplayValue("Unsent name")).toBeInTheDocument();
    vi.useRealTimers();
  });

});


beforeEach(() => resetAllRegisteredDrafts());
