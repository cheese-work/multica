import { vi, it, expect, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup, waitFor } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Agent } from "@multica/core/types";
import enAgents from "../../locales/en/agents.json";
import enSettings from "../../locales/en/settings.json";
import { AgentAutoCompactSection } from "./agent-auto-compact-section";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

afterEach(cleanup);

function mount(runtimeConfig: Record<string, unknown>, canEdit = true) {
  const onUpdate = vi.fn().mockResolvedValue(undefined);
  render(
    <I18nProvider locale="en" resources={{ en: { agents: enAgents, settings: enSettings } }}>
      <AgentAutoCompactSection
        agent={{ id: "agent-1", runtime_config: runtimeConfig } as unknown as Agent}
        canEdit={canEdit}
        onUpdate={onUpdate}
      />
    </I18nProvider>,
  );
  return onUpdate;
}

const oneM = () => screen.getByLabelText("Threshold, 1M models (tokens)");
const small = () => screen.getByLabelText("Threshold, 200k models (tokens)");

// CHE-4331: agent-level override of the workspace auto-compact thresholds.
it("saves the 1M threshold and keeps every other runtime_config key", async () => {
  const onUpdate = mount({ qoder_agent_id: "a", auto_compact: { enabled: false } });
  fireEvent.change(oneM(), { target: { value: "300000" } });
  fireEvent.blur(oneM());
  await waitFor(() =>
    expect(onUpdate).toHaveBeenCalledWith("agent-1", {
      runtime_config: {
        qoder_agent_id: "a",
        auto_compact: { enabled: false, threshold_tokens_1m: 300000 },
      },
    }),
  );
});

it("clearing a saved value falls back to the workspace and drops an empty auto_compact", async () => {
  const onUpdate = mount({ keep: 1, auto_compact: { threshold_tokens_200k: 150000 } });
  expect(small()).toHaveValue(150000);
  fireEvent.change(small(), { target: { value: "" } });
  fireEvent.blur(small());
  await waitFor(() =>
    expect(onUpdate).toHaveBeenCalledWith("agent-1", { runtime_config: { keep: 1 } }),
  );
});

it("never sends a 1M threshold above 400000", () => {
  const onUpdate = mount({});
  fireEvent.change(oneM(), { target: { value: "500000" } });
  fireEvent.blur(oneM());
  expect(onUpdate).not.toHaveBeenCalled();
  expect(oneM()).toHaveAttribute("aria-invalid", "true");
  expect(screen.getByText("Enter a whole number from 100000 to 400000.")).toBeInTheDocument();
});

it("is read-only without edit access", () => {
  mount({}, false);
  expect(oneM()).toBeDisabled();
  expect(small()).toBeDisabled();
});

it("a saved token limit replaces a percent saved earlier at this level", async () => {
  const onUpdate = mount({ auto_compact: { enabled: true, threshold_pct: 80 } });
  fireEvent.change(oneM(), { target: { value: "300000" } });
  fireEvent.blur(oneM());
  await waitFor(() =>
    expect(onUpdate).toHaveBeenCalledWith("agent-1", {
      runtime_config: { auto_compact: { enabled: true, threshold_tokens_1m: 300000 } },
    }),
  );
});
