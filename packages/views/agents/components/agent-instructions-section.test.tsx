// @vitest-environment jsdom

import { useEffect } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { useAgentInstructionsDraftStore } from "@multica/core/agents";
import type { Agent } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enAgents from "../../locales/en/agents.json";

vi.mock("./tabs/instructions-tab", () => ({
  InstructionsTab: ({ onDirtyChange }: { onDirtyChange?: (dirty: boolean) => void }) => {
    useEffect(() => onDirtyChange?.(true), [onDirtyChange]);
    return <div>instructions-editor</div>;
  },
}));
vi.mock("../editor/readonly-content", () => ({
  ReadonlyContent: ({ content }: { content: string }) => <div>{content}</div>,
}));

import { AgentInstructionsSection } from "./agent-instructions-section";

const agent = { id: "agent-1", instructions: "saved" } as Agent;

afterEach(() => {
  cleanup();
  resetAllRegisteredDrafts();
});

describe("AgentInstructionsSection", () => {
  it("drops the stored draft when the user confirms the discard", () => {
    useAgentInstructionsDraftStore.getState().setDraft("agent-1", { value: "typed" });
    render(
      <I18nProvider locale="en" resources={{ en: { common: enCommon, agents: enAgents } }}>
        <AgentInstructionsSection agent={agent} canEdit onUpdate={vi.fn()} />
      </I18nProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: /edit/i }));
    fireEvent.click(screen.getByRole("button", { name: /cancel/i }));
    fireEvent.click(screen.getByRole("button", { name: "Discard changes" }));

    expect(useAgentInstructionsDraftStore.getState().getDraft("agent-1")).toBeUndefined();
  });
});
