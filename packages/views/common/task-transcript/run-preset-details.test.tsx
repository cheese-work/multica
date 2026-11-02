import { afterEach, describe, expect, it } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import type { TaskPresetRun } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import enAgents from "../../locales/en/agents.json";
import { RunPresetDetails } from "./run-preset-details";

const run = (over: Partial<TaskPresetRun> = {}): TaskPresetRun => ({
  name: "fast",
  source: "dm",
  skips: [],
  claimed: { model: "gpt-6.1-sol", thinking_level: "low", service_tier: null, context_args: ["--autocompact", "200k"] },
  applied: null,
  ...over,
});

const t = enAgents.transcript;

describe("RunPresetDetails", () => {
  afterEach(cleanup);

  it("shows the preset, its source and the claimed settings", () => {
    renderWithI18n(<RunPresetDetails run={run()} />);
    expect(screen.getByText(t.details_preset)).toBeInTheDocument();
    expect(screen.getByText("fast · dm")).toBeInTheDocument();
    expect(screen.getByText("gpt-6.1-sol · low · --autocompact 200k")).toBeInTheDocument();
  });

  it("says applied settings were not reported until the daemon reports them", () => {
    renderWithI18n(<RunPresetDetails run={run()} />);
    expect(screen.getByText(t.details_preset_applied)).toBeInTheDocument();
    expect(screen.getByText(t.details_preset_not_reported)).toBeInTheDocument();
  });

  it("shows applied settings apart from claimed when reported", () => {
    renderWithI18n(
      <RunPresetDetails
        run={run({
          applied: { model: "gpt-6.1-sol", thinking_level: "medium", service_tier: null, context_args: [], reported_at: "2026-11-01T05:00:00Z" },
        })}
      />,
    );
    expect(screen.getByText("gpt-6.1-sol · low · --autocompact 200k")).toBeInTheDocument();
    expect(screen.getByText("gpt-6.1-sol · medium")).toBeInTheDocument();
    expect(screen.queryByText(t.details_preset_not_reported)).toBeNull();
  });

  it("lists each preset the resolution skipped, with its reason", () => {
    renderWithI18n(
      <RunPresetDetails run={run({ skips: [{ level: "issue_label", name: "old", reason: "lab_lock" }] })} />,
    );
    expect(screen.getByText(t.details_preset_skipped)).toBeInTheDocument();
    expect(screen.getByText("issue_label: old (lab_lock)")).toBeInTheDocument();
  });

  it("names agent defaults when a preset sets nothing", () => {
    renderWithI18n(
      <RunPresetDetails run={run({ claimed: { model: null, thinking_level: null, service_tier: null, context_args: null as unknown as string[] } })} />,
    );
    expect(screen.getByText(t.details_preset_agent_defaults)).toBeInTheDocument();
  });
});
