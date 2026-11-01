import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { AgentPreset } from "@multica/core/types";
import { renderWithI18n } from "../../../test/i18n";
import enAgents from "../../../locales/en/agents.json";

const listAgentPresets = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", () => ({ api: { listAgentPresets } }));

import { AgentPresetsSection } from "./agent-presets-section";

const preset = (over: Partial<AgentPreset>): AgentPreset => ({
  name: "fast",
  model: null,
  thinking_level: null,
  service_tier: null,
  context_args: null,
  is_default: false,
  stale: "",
  created_at: "",
  updated_at: "",
  ...over,
});

function show(presets: AgentPreset[]) {
  listAgentPresets.mockResolvedValue(presets);
  return renderWithI18n(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <AgentPresetsSection wsId="ws-1" agentId="agent-1" />
    </QueryClientProvider>,
  );
}

describe("AgentPresetsSection", () => {
  afterEach(cleanup);

  it("lists each preset with its settings and marks the default", async () => {
    show([
      preset({ name: "fast", model: "gpt-6.1-sol", thinking_level: "low", is_default: true }),
      preset({ name: "big", context_args: ["--autocompact", "200k"] }),
    ]);
    expect(await screen.findByText("fast")).toBeInTheDocument();
    expect(screen.getByText("gpt-6.1-sol · low")).toBeInTheDocument();
    expect(screen.getByText("--autocompact 200k")).toBeInTheDocument();
    expect(screen.getAllByText(enAgents.inspector.preset_default)).toHaveLength(1);
    expect(screen.queryByText(enAgents.inspector.preset_stale)).toBeNull();
  });

  it("marks a stale preset with its reason", async () => {
    show([preset({ name: "old", stale: "lab_lock" })]);
    expect(await screen.findByText(enAgents.inspector.preset_stale)).toBeInTheDocument();
    expect(screen.getByText(enAgents.inspector.preset_stale_reason.lab_lock)).toBeInTheDocument();
  });

  it("shows an unknown stale reason as sent", async () => {
    show([preset({ name: "old", stale: "brand_new_reason" })]);
    expect(await screen.findByText(enAgents.inspector.preset_stale)).toBeInTheDocument();
    expect(screen.getByText("brand_new_reason")).toBeInTheDocument();
  });

  it("renders nothing for an agent without presets", async () => {
    const { container } = show([]);
    await vi.waitFor(() => expect(listAgentPresets).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });
});
