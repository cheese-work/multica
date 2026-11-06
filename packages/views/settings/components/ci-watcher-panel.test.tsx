import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { useCIWatchDraftStore } from "@multica/core/settings/draft-stores";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api, ApiError } from "@multica/core/api";
import type { CIWatchConfig, CIWatchEvent } from "@multica/core/ci-watch";
import { renderWithI18n } from "../../test/i18n";
import { CIWatcherPanel } from "./ci-watcher-panel";

vi.mock("@multica/core/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/api")>()),
  api: {
    getCIWatchConfig: vi.fn(),
    updateCIWatchConfig: vi.fn(),
    listCIWatchEvents: vi.fn(),
    listMembers: vi.fn(),
    listAgents: vi.fn(),
    listSquads: vi.fn(),
  },
}));
const role = vi.hoisted(() => ({ current: "admin" as string | null }));
vi.mock("@multica/core/permissions", () => ({
  useCurrentMember: () => ({ role: role.current, userId: "u1", member: null, isLoading: false }),
}));

const getConfig = vi.mocked(api.getCIWatchConfig);
const putConfig = vi.mocked(api.updateCIWatchConfig);
const listEvents = vi.mocked(api.listCIWatchEvents);

const REPO = "cheese-work/multica";
const saved: CIWatchConfig = {
  repo: REPO, mode: "shadow", branches: ["main"], events: ["push", "schedule"],
  workflows_include: ["ci.yml"], workflows_exclude: ["nightly.yml"], p1_mode: "shadow", p2_mode: "shadow",
  p3_mode: "act", p4_mode: "shadow", fix_owner_kind: "squad", fix_owner_id: "sq1",
  notify_member_id: "u1", max_reruns_per_original_run: 1, max_new_issues_per_day: 5,
  max_owner_wakes_per_hour: 6, updated_at: "2026-11-06T01:00:00Z",
};
const failure: CIWatchEvent = {
  at: "2026-11-06T01:00:00Z", kind: "ci_failure", workflow: "ci.yml", branch: "main", run_id: 7, detail: "",
  class: "flaky", protocol: "P1", action: "rerun", state: "done", outcome: "rerun_green", would_do: "",
};
const shadowRow: CIWatchEvent = { ...failure, run_id: 8, class: "unknown", protocol: "", action: "", state: "", outcome: "", would_do: "P2 triage" };
const breaker: CIWatchEvent = {
  at: "2026-11-06T03:30:00Z", kind: "breaker_open", workflow: "", branch: "", run_id: 0, detail: "cap hit",
  class: "", protocol: "", action: "", state: "", outcome: "", would_do: "",
};

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const view = renderWithI18n(
    <QueryClientProvider client={client}>
      <CIWatcherPanel wsId="ws1" repo={REPO} />
    </QueryClientProvider>,
  );
  return { ...view, client };
}
const field = (name: string) => screen.findByLabelText(name);

beforeEach(() => {
  resetAllRegisteredDrafts();
  role.current = "admin";
  getConfig.mockReset().mockResolvedValue(saved);
  putConfig.mockReset().mockImplementation(async (_ws, _repo, patch) => ({ ...saved, ...(patch as object) }) as CIWatchConfig);
  listEvents.mockReset().mockResolvedValue([failure, shadowRow, breaker]);
  vi.mocked(api.listMembers).mockReset().mockResolvedValue([
    { id: "m1", workspace_id: "ws1", user_id: "u1", role: "owner", created_at: "", name: "Cheese", email: "c@x", avatar_url: null },
    { id: "m2", workspace_id: "ws1", user_id: "u2", role: "member", created_at: "", name: "Lan", email: "l@x", avatar_url: null },
  ] as never);
  vi.mocked(api.listAgents).mockReset().mockResolvedValue([
    { id: "ag1", name: "Fixer", archived_at: null },
    { id: "ag2", name: "Old", archived_at: "2026-01-01T00:00:00Z" },
  ] as never);
  vi.mocked(api.listSquads).mockReset().mockResolvedValue([{ id: "sq1", name: "Dev team" }] as never);
});

describe("fields", () => {
  it("shows the saved config in every field", async () => {
    mount();
    expect(await field("Mode")).toHaveValue("shadow");
    expect(screen.getByLabelText("Branches")).toHaveValue("main");
    expect(screen.getByRole("checkbox", { name: "push" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "schedule" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "workflow_run" })).not.toBeChecked();
    expect(screen.getByRole("checkbox", { name: "workflow_dispatch" })).not.toBeChecked();
    expect(screen.getByLabelText("Include workflows")).toHaveValue("ci.yml");
    expect(screen.getByLabelText("Exclude workflows")).toHaveValue("nightly.yml");
    expect(screen.getByLabelText("P1 mode")).toHaveValue("shadow");
    expect(screen.getByLabelText("P3 mode")).toHaveValue("act");
    await waitFor(() => expect(screen.getByLabelText("Fix owner")).toHaveValue("squad:sq1"));
    await waitFor(() => expect(screen.getByLabelText("Notify member")).toHaveValue("u1"));
    expect(screen.getByLabelText("Max reruns per original run")).toHaveValue(1);
    expect(screen.getByLabelText("Max new issues per day")).toHaveValue(5);
    expect(screen.getByLabelText("Max owner wakes per hour")).toHaveValue(6);
  });

  it("lists merge_group as observed only, without a switch", async () => {
    mount();
    await field("Mode");
    expect(screen.getByText("merge_group (observed only)")).toBeInTheDocument();
    expect(screen.queryByRole("checkbox", { name: /merge_group/ })).toBeNull();
  });

  it("notes that unknown failures stay in Shadow in phase 1", async () => {
    mount();
    await field("Mode");
    expect(screen.getByText(/Unknown failures stay in Shadow/)).toBeInTheDocument();
  });

  it("offers squads, live agents and members in the pickers", async () => {
    mount();
    const owner = await field("Fix owner");
    await waitFor(() => expect(within(owner).getByRole("option", { name: "Fixer" })).toBeInTheDocument());
    expect(within(owner).getByRole("option", { name: "Dev team" })).toBeInTheDocument();
    expect(within(owner).queryByRole("option", { name: "Old" })).toBeNull();
    expect(within(screen.getByLabelText("Notify member")).getByRole("option", { name: "Lan" })).toBeInTheDocument();
  });

  it("saves only the changed fields", async () => {
    mount();
    fireEvent.change(await field("Mode"), { target: { value: "act" } });
    fireEvent.change(screen.getByLabelText("P2 mode"), { target: { value: "act" } });
    fireEvent.change(screen.getByLabelText("Max new issues per day"), { target: { value: "9" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(putConfig).toHaveBeenCalledWith("ws1", REPO, {
      mode: "act", p2_mode: "act", max_new_issues_per_day: 9,
    }));
  });

  it("saves branches, workflow filters and events", async () => {
    mount();
    fireEvent.change(await field("Branches"), { target: { value: "main\nrelease/*" } });
    fireEvent.change(screen.getByLabelText("Include workflows"), { target: { value: "" } });
    fireEvent.change(screen.getByLabelText("Exclude workflows"), { target: { value: "a.yml, b.yml" } });
    fireEvent.click(screen.getByRole("checkbox", { name: "workflow_run" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "schedule" }));
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(putConfig).toHaveBeenCalledWith("ws1", REPO, {
      branches: ["main", "release/*"], workflows_include: [], workflows_exclude: ["a.yml", "b.yml"],
      events: ["push", "workflow_run"],
    }));
  });

  it("sends the fix owner kind and id together, and null for both when cleared", async () => {
    mount();
    const owner = await field("Fix owner");
    await waitFor(() => within(owner).getByRole("option", { name: "Fixer" }));
    fireEvent.change(owner, { target: { value: "agent:ag1" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(putConfig).toHaveBeenLastCalledWith("ws1", REPO, { fix_owner_kind: "agent", fix_owner_id: "ag1" }));
    fireEvent.change(owner, { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(putConfig).toHaveBeenLastCalledWith("ws1", REPO, { fix_owner_kind: null, fix_owner_id: null }));
  });

  it("saves the notify member", async () => {
    mount();
    const member = await field("Notify member");
    await waitFor(() => within(member).getByRole("option", { name: "Lan" }));
    fireEvent.change(member, { target: { value: "u2" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(putConfig).toHaveBeenCalledWith("ws1", REPO, { notify_member_id: "u2" }));
  });

  it("blocks Save on an invalid cap and says why", async () => {
    mount();
    fireEvent.change(await field("Max reruns per original run"), { target: { value: "-3" } });
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
    expect(screen.getByText("Enter a whole number from 0 to 100000.")).toBeInTheDocument();
  });

  it("keeps Save off until something changed", async () => {
    mount();
    await field("Mode");
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
  });

  it("shows the server's message when the save is refused", async () => {
    putConfig.mockRejectedValue(new ApiError("fix_owner_id: not found in this workspace", 400, "Bad Request", { field: "fix_owner_id" }));
    mount();
    fireEvent.change(await field("Mode"), { target: { value: "act" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("fix_owner_id: not found in this workspace");
  });
});

describe("events table", () => {
  it("shows the last 50 events with ICT time, columns and run link", async () => {
    mount();
    expect(await screen.findByText("Last 50 events")).toBeInTheDocument();
    expect(listEvents).toHaveBeenCalledWith("ws1", REPO, 50);
    const table = await screen.findByRole("table", { name: "Last 50 events" });
    const headers = within(table).getAllByRole("columnheader").map((h) => h.textContent);
    expect(headers).toEqual(["Time", "Workflow", "Class", "Protocol", "Action", "State", "Outcome", "Would have done", "Run"]);
    const row = within(table).getAllByRole("row")[1]!;
    const cells = within(row).getAllByRole("cell").map((c) => c.textContent);
    expect(cells.slice(0, 7)).toEqual(["2026-11-06 08:00 ICT", "ci.yml", "flaky", "P1", "rerun", "done", "rerun_green"]);
    expect(within(row).getByRole("link", { name: "Run 7" })).toHaveAttribute("href", `https://github.com/${REPO}/actions/runs/7`);
  });

  it("shows what Shadow would have done", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Last 50 events" });
    expect(within(table).getByText("P2 triage")).toBeInTheDocument();
  });

  it("hides the would-have-done column outside Shadow", async () => {
    getConfig.mockResolvedValue({ ...saved, mode: "act" });
    mount();
    const table = await screen.findByRole("table", { name: "Last 50 events" });
    expect(within(table).queryByRole("columnheader", { name: "Would have done" })).toBeNull();
  });

  it("shows a breaker row with its detail and no run link", async () => {
    mount();
    const table = await screen.findByRole("table", { name: "Last 50 events" });
    const row = within(table).getByText("cap hit").closest("tr")!;
    expect(within(row).getByText("2026-11-06 10:30 ICT")).toBeInTheDocument();
    expect(within(row).queryByRole("link")).toBeNull();
  });

  it("says so when there are no events", async () => {
    listEvents.mockResolvedValue([]);
    mount();
    expect(await screen.findByText("No events yet.")).toBeInTheDocument();
  });
});

describe("access", () => {
  it("gives a non-admin no controls and makes no request", async () => {
    role.current = "member";
    mount();
    expect(await screen.findByText("Only workspace owners and admins can change the CI watcher.")).toBeInTheDocument();
    expect(screen.queryByLabelText("Mode")).toBeNull();
    expect(screen.queryByRole("button", { name: "Save changes" })).toBeNull();
    expect(getConfig).not.toHaveBeenCalled();
    expect(listEvents).not.toHaveBeenCalled();
  });

  it("shows the disabled state when the server flag is off", async () => {
    getConfig.mockRejectedValue(new ApiError("disabled", 403, "Forbidden", { error: "CI watcher is disabled on this server", code: "ci_watcher_disabled" }));
    listEvents.mockRejectedValue(new ApiError("disabled", 403, "Forbidden", { code: "ci_watcher_disabled" }));
    mount();
    expect(await screen.findByText("CI watcher is disabled on this server")).toBeInTheDocument();
    expect(screen.queryByLabelText("Mode")).toBeNull();
    expect(screen.queryByRole("button", { name: "Save changes" })).toBeNull();
  });

  it("reports another load failure as an error, not as disabled", async () => {
    getConfig.mockRejectedValue(new ApiError("nope", 500, "Server Error"));
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not load the CI watcher settings.");
    expect(screen.queryByText("CI watcher is disabled on this server")).toBeNull();
  });
});

describe("unsent edits", () => {
  it("survive an unmount and remount until saved", async () => {
    const first = mount();
    fireEvent.change(await field("Branches"), { target: { value: "main\ndev" } });
    fireEvent.change(screen.getByLabelText("Mode"), { target: { value: "act" } });
    first.unmount();
    mount();
    expect(await field("Branches")).toHaveValue("main\ndev");
    expect(screen.getByLabelText("Mode")).toHaveValue("act");
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(putConfig).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByText("Unsaved changes")).toBeNull());
    expect(useCIWatchDraftStore.getState().getDraft(REPO)).toBeUndefined();
  });

  it("are read back from the store when it fills after mount", async () => {
    mount();
    expect(await field("Branches")).toHaveValue("main");
    act(() => useCIWatchDraftStore.getState().setDraft(REPO, { form: JSON.stringify({ branches: "late" }) }));
    expect(screen.getByLabelText("Branches")).toHaveValue("late");
  });

  it("stay when the save fails", async () => {
    putConfig.mockRejectedValue(new ApiError("boom", 500, "Server Error"));
    mount();
    fireEvent.change(await field("Branches"), { target: { value: "dev" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await screen.findByRole("alert");
    expect(screen.getByLabelText("Branches")).toHaveValue("dev");
    expect(useCIWatchDraftStore.getState().getDraft(REPO)).toBeDefined();
  });

  it("typed while a save is in flight survive the save response", async () => {
    let finish: (c: CIWatchConfig) => void = () => {};
    putConfig.mockImplementation(() => new Promise<CIWatchConfig>((r) => { finish = r; }));
    mount();
    fireEvent.change(await field("Branches"), { target: { value: "dev" } });
    fireEvent.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(putConfig).toHaveBeenCalled());
    fireEvent.change(screen.getByLabelText("Branches"), { target: { value: "dev\nrelease" } });
    await act(async () => finish({ ...saved, branches: ["dev"] } as CIWatchConfig));
    expect(screen.getByLabelText("Branches")).toHaveValue("dev\nrelease");
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
    expect(useCIWatchDraftStore.getState().getDraft(REPO)).toBeDefined();
  });

  it("are dropped on an explicit discard", async () => {
    mount();
    fireEvent.change(await field("Branches"), { target: { value: "dev" } });
    fireEvent.click(screen.getByRole("button", { name: "Discard changes" }));
    expect(screen.getByLabelText("Branches")).toHaveValue("main");
    expect(screen.queryByText("Unsaved changes")).toBeNull();
    expect(useCIWatchDraftStore.getState().getDraft(REPO)).toBeUndefined();
  });

  it("keep edits per repo", async () => {
    mount();
    fireEvent.change(await field("Branches"), { target: { value: "dev" } });
    expect(useCIWatchDraftStore.getState().getDraft("cheese-work/other")).toBeUndefined();
  });
});
