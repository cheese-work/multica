import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

const mockUpdateWorkspace = vi.hoisted(() => vi.fn());
const workspaceRef = vi.hoisted(() => ({
  current: { id: "workspace-1", settings: {} as Record<string, unknown> },
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey: unknown[] }) => ({
    data: JSON.stringify(opts.queryKey).includes("members")
      ? [{ user_id: "user-1", name: "Mina" }]
      : [{ id: "agent-1", name: "Builder", archived_at: null }],
  }),
  useQueryClient: () => ({ setQueryData: vi.fn() }),
}));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));
vi.mock("@multica/core/paths", () => ({ useCurrentWorkspace: () => workspaceRef.current }));
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"] }),
  agentListOptions: () => ({ queryKey: ["agents"] }),
  workspaceKeys: { list: () => ["workspaces"] },
}));
vi.mock("@multica/core/api", () => ({
  api: { updateWorkspace: (...args: unknown[]) => mockUpdateWorkspace(...args) },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));

import { GitHubReviewMappings } from "./github-review-mappings";

const renderMappings = (canEdit = true) =>
  render(
    <I18nProvider locale="en" resources={{ en: { common: enCommon, settings: enSettings } }}>
      <GitHubReviewMappings canEdit={canEdit} />
    </I18nProvider>,
  );

describe("GitHubReviewMappings", () => {
  beforeEach(() => {
    mockUpdateWorkspace.mockReset();
    mockUpdateWorkspace.mockResolvedValue({ id: "workspace-1", settings: {} });
    workspaceRef.current = { id: "workspace-1", settings: {} };
  });

  it("shows saved entries with member and agent names", () => {
    workspaceRef.current.settings = {
      github_review_members: { "4242": "user-1" },
      github_review_authors: { "acme/app#7": "agent-1" },
    };
    renderMappings();
    expect(screen.getByText("4242")).toBeTruthy();
    expect(screen.getByText("Mina")).toBeTruthy();
    expect(screen.getByText("acme/app#7")).toBeTruthy();
    expect(screen.getByText("Builder")).toBeTruthy();
  });

  it("removes an entry by sending the map without it", async () => {
    workspaceRef.current.settings = {
      github_review_members: { "4242": "user-1", "99": "user-1" },
    };
    // Integer-like keys list in ascending order, so "99" is the first row.
    renderMappings();
    await userEvent.click(screen.getAllByRole("button", { name: "Remove" })[0]!);
    await waitFor(() =>
      expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", {
        github_review_members: { "4242": "user-1" },
      }),
    );
  });

  it("adds a reviewer mapping with the trimmed GitHub user ID", async () => {
    renderMappings();
    await userEvent.type(screen.getByLabelText("GitHub user ID"), " 4242 ");
    await userEvent.click(screen.getByRole("combobox", { name: "Member" }));
    await userEvent.click(await screen.findByRole("option", { name: "Mina" }));
    await userEvent.click(screen.getAllByRole("button", { name: "Add" })[0]!);
    await waitFor(() =>
      expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", {
        github_review_members: { "4242": "user-1" },
      }),
    );
  });

  it("adds a pull request author mapping with a lowercased key", async () => {
    workspaceRef.current.settings = { github_review_authors: { "acme/app#7": "agent-1" } };
    renderMappings();
    await userEvent.type(screen.getByLabelText("Pull request"), "Acme/App#9");
    await userEvent.click(screen.getByRole("combobox", { name: "Agent" }));
    await userEvent.click(await screen.findByRole("option", { name: "Builder" }));
    await userEvent.click(screen.getAllByRole("button", { name: "Add" })[1]!);
    await waitFor(() =>
      expect(mockUpdateWorkspace).toHaveBeenCalledWith("workspace-1", {
        github_review_authors: { "acme/app#7": "agent-1", "acme/app#9": "agent-1" },
      }),
    );
  });

  it("keeps Add disabled until both the key and the value are set", async () => {
    renderMappings();
    const add = screen.getAllByRole("button", { name: "Add" })[0]!;
    expect((add as HTMLButtonElement).disabled).toBe(true);
    await userEvent.type(screen.getByLabelText("GitHub user ID"), "4242");
    expect((add as HTMLButtonElement).disabled).toBe(true);
    expect(mockUpdateWorkspace).not.toHaveBeenCalled();
  });

  it("offers no controls to a member who cannot edit", () => {
    workspaceRef.current.settings = { github_review_members: { "4242": "user-1" } };
    renderMappings(false);
    expect(screen.queryByRole("button", { name: "Remove" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Add" })).toBeNull();
  });
});
