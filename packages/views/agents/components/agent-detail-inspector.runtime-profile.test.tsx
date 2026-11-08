// @vitest-environment jsdom

import { afterEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Agent, AgentRuntime } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";

vi.mock("@multica/core/api", () => ({
  api: { initiateListModels: vi.fn(), getListModelsResult: vi.fn() },
}));
vi.mock("../../common/avatar-upload-control", () => ({
  AvatarUploadControl: () => <div data-testid="avatar-upload" />,
}));
vi.mock("./inspector/runtime-picker", () => ({
  RuntimePicker: () => <div data-testid="runtime-picker" />,
}));

import { AgentDetailInspector } from "./agent-detail-inspector";

function runtime(provider: string): AgentRuntime {
  return {
    id: `rt-${provider}`,
    workspace_id: "workspace-1",
    daemon_id: null,
    name: provider,
    runtime_mode: provider === "qoder_cloud" ? "cloud" : "local",
    provider,
    launch_header: "",
    status: "online",
    device_info: "",
    metadata: {},
    owner_id: "user-1",
    visibility: "public",
    last_seen_at: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}

function renderInspector(opts: {
  runtimeConfig?: Record<string, unknown>;
  canEdit?: boolean;
  provider?: string;
}) {
  const bound = runtime(opts.provider ?? "claude");
  const onUpdate = vi.fn(async () => {});
  const agent = {
    id: "agent-1",
    workspace_id: "workspace-1",
    name: "Lambda",
    description: "",
    runtime_id: bound.id,
    model: "",
    max_concurrent_tasks: 6,
    runtime_config: opts.runtimeConfig ?? {},
  } as Agent;
  renderWithI18n(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <AgentDetailInspector
        agent={agent}
        runtime={bound}
        runtimes={[bound]}
        members={[]}
        currentUserId="user-1"
        canEdit={opts.canEdit ?? true}
        onUpdate={onUpdate}
      />
    </QueryClientProvider>,
  );
  return onUpdate;
}

// CHE-3706: agent override of the workspace Runtime profile, stored in
// runtime_config.harness_profile. runtime_config is replaced as a whole on the
// server, so every write must keep the other keys.
describe("AgentDetailInspector runtime profile", () => {
  afterEach(cleanup);

  it("inherits the workspace default when nothing is set", () => {
    renderInspector({});
    expect(screen.getByRole("combobox", { name: "Runtime profile" })).toHaveTextContent(
      "Workspace default",
    );
  });

  it("shows a saved override", () => {
    renderInspector({ runtimeConfig: { harness_profile: "managed" } });
    expect(screen.getByRole("combobox", { name: "Runtime profile" })).toHaveTextContent("Managed");
  });

  it("sets the override and keeps the other runtime_config keys", async () => {
    const user = userEvent.setup();
    const onUpdate = renderInspector({ runtimeConfig: { auto_compact: { enabled: false } } });

    await user.click(screen.getByRole("combobox", { name: "Runtime profile" }));
    await user.click(await screen.findByRole("option", { name: "Managed" }));

    await waitFor(() =>
      expect(onUpdate).toHaveBeenCalledWith("agent-1", {
        runtime_config: { auto_compact: { enabled: false }, harness_profile: "managed" },
      }),
    );
  });

  it("clears the override back to the workspace default", async () => {
    const user = userEvent.setup();
    const onUpdate = renderInspector({
      runtimeConfig: { harness_profile: "managed", keep: 1 },
    });

    await user.click(screen.getByRole("combobox", { name: "Runtime profile" }));
    await user.click(await screen.findByRole("option", { name: "Workspace default" }));

    await waitFor(() =>
      expect(onUpdate).toHaveBeenCalledWith("agent-1", { runtime_config: { keep: 1 } }),
    );
  });

  it("is read-only without edit access", () => {
    renderInspector({ runtimeConfig: { harness_profile: "managed" }, canEdit: false });
    expect(screen.queryByRole("combobox", { name: "Runtime profile" })).toBeNull();
    expect(screen.getByText("Managed")).toBeInTheDocument();
  });

  it("is hidden for a Qoder cloud agent", () => {
    renderInspector({ provider: "qoder_cloud" });
    expect(screen.queryByRole("combobox", { name: "Runtime profile" })).toBeNull();
  });
});
