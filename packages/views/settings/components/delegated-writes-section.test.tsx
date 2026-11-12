import { beforeEach, describe, expect, it, vi } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { api } from "@multica/core/api";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { useWorkspaceSettingsDraftStore } from "@multica/core/settings/draft-stores";
import { renderWithI18n } from "../../test/i18n";
import { DelegatedWritesSection } from "./delegated-writes-section";

vi.mock("@multica/core/api", () => ({
  api: { getDelegatedWrites: vi.fn(), updateDelegatedWrites: vi.fn(), listAgents: vi.fn() },
}));

const read = vi.mocked(api.getDelegatedWrites);
const write = vi.mocked(api.updateDelegatedWrites);

const agent = (id: string, name: string, archived_at: string | null = null) =>
  ({ id, name, archived_at }) as never;
const SAVED = {
  allowed_agents: ["a1", "a2"],
  allowed_fields: ["agent.instructions"],
  requires_human_decision: true,
};

function mount(canEdit: boolean) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const ui = () => (
    <QueryClientProvider client={client}>
      <DelegatedWritesSection wsId="ws" canEdit={canEdit} />
    </QueryClientProvider>
  );
  const view = renderWithI18n(ui());
  return { ...view, remount: () => { view.unmount(); return renderWithI18n(ui()); } };
}

beforeEach(() => {
  localStorage.clear();
  resetAllRegisteredDrafts();
  read.mockReset().mockResolvedValue(SAVED);
  write.mockReset();
  vi.mocked(api.listAgents).mockReset().mockResolvedValue([
    agent("a1", "x99-claude-sonnet-200k"),
    agent("a2", "x99-codex-terra"),
    agent("a3", "x99-agy-gemini-ux"),
    agent("a4", "x99-old-runner", "2026-11-01T00:00:00Z"),
  ]);
});

describe("DelegatedWritesSection", () => {
  it("shows a member the saved rule with a lock note and no way to edit or save", async () => {
    mount(false);

    expect(await screen.findByText(/2 agents may write 1 field/)).toBeInTheDocument();
    expect(screen.getByText(/Only workspace owners and admins can change this/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Edit rule" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save changes" })).not.toBeInTheDocument();
  });

  it("lets an owner save agents, fields and the human-decision switch, and shows them after a reload", async () => {
    const next = {
      allowed_agents: ["a1", "a2", "a3"],
      allowed_fields: ["agent.instructions", "workflow.steps"],
      requires_human_decision: false,
    };
    write.mockResolvedValue(next);
    const user = userEvent.setup();
    const view = mount(true);

    await user.click(await screen.findByRole("button", { name: "Edit rule" }));
    await user.selectOptions(screen.getByRole("combobox", { name: "Add agent" }), "a3");
    await user.click(screen.getByRole("checkbox", { name: "workflow.steps" }));
    await user.click(screen.getByRole("switch", { name: "Requires human decision" }));
    // Turning the human decision off needs a confirmation before Save unlocks.
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
    await user.click(screen.getByRole("checkbox", { name: /I understand agents will write/ }));
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(write).toHaveBeenCalledWith("ws", next));

    read.mockResolvedValue(next);
    view.remount();
    expect(await screen.findByText(/3 agents may write 2 fields, directly/)).toBeInTheDocument();
  });

  it("keeps the unsent input when a save fails, across a remount", async () => {
    write.mockRejectedValue(new Error("x99-old-runner was archived and cannot be allowed."));
    const user = userEvent.setup();
    const view = mount(true);

    await user.click(await screen.findByRole("button", { name: "Edit rule" }));
    await user.click(screen.getByRole("checkbox", { name: "workflow.steps" }));
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    expect(await screen.findByRole("alert")).toHaveTextContent("archived and cannot be allowed");
    expect(screen.getByRole("checkbox", { name: "workflow.steps" })).toBeChecked();

    view.remount();
    const box = await screen.findByRole("checkbox", { name: "workflow.steps" });
    expect(box).toBeChecked();
    expect(within(screen.getByRole("region", { name: "Delegated governed writes" })).getByText("Unsaved changes")).toBeInTheDocument();
  });

  it("offers setup when nothing is delegated", async () => {
    read.mockResolvedValue({ allowed_agents: [], allowed_fields: [], requires_human_decision: true });
    mount(true);

    expect(await screen.findByText("No agent may write a governed field.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Set up delegation" })).toBeInTheDocument();
  });

  it("reads a persisted draft through the schema: a missing list is empty, not a render crash", async () => {
    useWorkspaceSettingsDraftStore.getState().setDraft("ws:delegated-writes", {
      rule: JSON.stringify({ allowed_agents: ["a1"], requires_human_decision: false }),
    });
    mount(true);

    // Missing allowed_fields in the draft must not throw during render.
    expect(await screen.findByText(/1 agent may write 0 fields, directly/)).toBeInTheDocument();
  });
});
