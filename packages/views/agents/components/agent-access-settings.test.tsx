// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import type { Agent, AgentBackgroundUse, MemberWithUser, WorkspaceFeatureState } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { AgentAccessSettings } from "./agent-access-settings";

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => null }));
vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<object>()),
  useWorkspacePaths: () => ({ settings: () => "/acme/settings" }),
}));
vi.mock("../../navigation", async (importOriginal) => ({
  ...(await importOriginal<object>()),
  AppLink: ({ href, children }: { href: string; children: React.ReactNode }) => <a href={href}>{children}</a>,
}));

const MEMBERS = [
  { user_id: "owner", name: "Owner", role: "owner" },
  { user_id: "u1", name: "Alice", role: "member" },
  { user_id: "u2", name: "Bob", role: "member" },
] as unknown as MemberWithUser[];

let feature: WorkspaceFeatureState;
let backgroundUse: AgentBackgroundUse[] = [];
const getAgent = vi.fn(async () => ({ background_use: backgroundUse }));

function agent(overrides: Partial<Agent>): Agent {
  return {
    id: "a1",
    workspace_id: "ws-1",
    owner_id: "owner",
    visibility: "private",
    permission_mode: "private",
    invocation_targets: [],
    composio_toolkit_allowlist: [],
    ...overrides,
  } as Agent;
}

function renderSettings(value: Agent) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={qc}>
      <AgentAccessSettings agent={value} members={MEMBERS} currentUserId="owner" onUpdate={vi.fn()} />
    </QueryClientProvider>,
  );
}

describe("AgentAccessSettings plan access", () => {
  beforeEach(() => {
    feature = "available";
    backgroundUse = [];
    getAgent.mockClear();
    setApiInstance({
      getWorkspaceFeatures: vi.fn(async () => ({ features: { agent_invocation_members: feature } })),
      getAgent,
    } as unknown as ApiClient);
  });
  afterEach(cleanup);

  it("offers Specific people when the plan includes it", async () => {
    renderSettings(agent({}));
    await waitFor(() => expect(screen.getByRole("radio", { name: /^Specific people/i })).toBeEnabled());
    expect(screen.queryByText("View plans")).not.toBeInTheDocument();
  });

  it("marks a locked plan unavailable for an agent without a member list", async () => {
    feature = "locked";
    renderSettings(agent({ permission_mode: "public_to", invocation_targets: [{ target_type: "workspace", target_id: null }] }));
    expect(await screen.findByText("Not included in this workspace's plan.")).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /^Specific people/i })).toBeDisabled();
  });

  it("marks a locked plan read-only for an agent that already has members", async () => {
    feature = "locked";
    renderSettings(agent({ permission_mode: "public_to", invocation_targets: [{ target_type: "member", target_id: "u1" }] }));
    expect(await screen.findByText("Read-only. Upgrade to edit.")).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: /Alice/i })).toBeChecked();
    expect(screen.queryByRole("checkbox", { name: /Bob/i })).not.toBeInTheDocument();
  });
});

const NOTE = /makes this agent ineligible for its workspace background jobs/;

describe("AgentAccessSettings background agent note (CHE-5439)", () => {
  beforeEach(() => {
    feature = "available";
    backgroundUse = [];
    getAgent.mockClear();
    setApiInstance({
      getWorkspaceFeatures: vi.fn(async () => ({ features: { agent_invocation_members: feature } })),
      getAgent,
    } as unknown as ApiClient);
  });
  afterEach(cleanup);

  it("warns that narrowing access makes a workspace background agent ineligible", async () => {
    backgroundUse = [{ scope: "workspace", job: "dm_title" }];
    renderSettings(agent({}));
    expect(await screen.findByText(NOTE)).toBeInTheDocument();
    expect(screen.getByText(/"Only me" or "Specific people"/)).toBeInTheDocument();
  });

  it("shows no note when the agent has only a personal background use", async () => {
    backgroundUse = [{ scope: "personal", job: "quick_actions" }];
    renderSettings(agent({}));
    await waitFor(() => expect(getAgent).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText(NOTE)).toBeNull();
  });

  it("shows no note when the agent has no background use", async () => {
    renderSettings(agent({}));
    await waitFor(() => expect(getAgent).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText(NOTE)).toBeNull();
  });
});
