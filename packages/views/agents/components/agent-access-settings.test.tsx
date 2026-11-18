// @vitest-environment jsdom

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Agent, AgentBackgroundUse } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import { RESOURCES } from "../../locales";

const usesRef = vi.hoisted(() => ({ current: [] as AgentBackgroundUse[] }));

vi.mock("@multica/core/workspace/queries", () => ({
  agentBackgroundUseOptions: (wsId: string, agentId: string) => ({
    queryKey: ["agents", wsId, "detail", agentId, "background-use"],
    queryFn: () => Promise.resolve(usesRef.current),
  }),
}));
vi.mock("./inspector/access-picker", () => ({
  AccessPicker: () => <div>access-picker</div>,
}));
vi.mock("../../settings/components/settings-layout", () => ({
  SettingsSection: ({ title, children }: { title: string; children: React.ReactNode }) => (
    <section>
      <h2>{title}</h2>
      {children}
    </section>
  ),
  SettingsCard: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
}));

import { AgentAccessSettings } from "./agent-access-settings";

const agent = {
  id: "agent-1",
  workspace_id: "ws-1",
  permission_mode: "public_to",
  invocation_targets: [],
  visibility: "workspace",
  owner_id: "user-1",
} as unknown as Agent;

function renderSettings(currentUserId = "user-1") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <I18nProvider locale="en" resources={RESOURCES}>
      <QueryClientProvider client={queryClient}>
        <AgentAccessSettings
          agent={agent}
          members={[]}
          currentUserId={currentUserId}
          onUpdate={async () => {}}
        />
      </QueryClientProvider>
    </I18nProvider>,
  );
}

const NOTE = /makes this agent ineligible for its workspace background jobs/;

describe("AgentAccessSettings background agent note (CHE-5439)", () => {
  beforeEach(() => {
    usesRef.current = [];
  });

  it("warns that narrowing access makes a workspace background agent ineligible", async () => {
    usesRef.current = [{ scope: "workspace", job: "dm_title" }];
    renderSettings();
    expect(await screen.findByText(NOTE)).toBeInTheDocument();
    expect(screen.getByText(/"Only me" or "Specific people"/)).toBeInTheDocument();
  });

  it("shows no note when the agent has only a personal background use", async () => {
    usesRef.current = [{ scope: "personal", job: "quick_actions" }];
    renderSettings();
    await screen.findByText("access-picker");
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText(NOTE)).toBeNull();
  });

  it("shows no note when the agent has no background use", async () => {
    renderSettings();
    await screen.findByText("access-picker");
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByText(NOTE)).toBeNull();
  });
});
