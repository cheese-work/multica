import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { I18nProvider } from "@multica/core/i18n/react";
import enChat from "../../locales/en/chat.json";
import enCommon from "../../locales/en/common.json";

const getAgentPresetCompletions = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", () => ({ api: { getAgentPresetCompletions } }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));

import { ChatPresetPicker } from "./chat-preset-picker";

function setup(current: string | null, names = ["base", "reset", "fast", "deep"]) {
  getAgentPresetCompletions.mockResolvedValue(names);
  const onSelect = vi.fn();
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <I18nProvider locale="en" resources={{ en: { chat: enChat, common: enCommon } }}>
        <ChatPresetPicker agentId="agent-1" current={current} onSelect={onSelect} />
      </I18nProvider>
    </QueryClientProvider>,
  );
  return onSelect;
}

const open = async () => {
  fireEvent.click(await screen.findByRole("button", { name: enChat.header.preset.label }));
  await screen.findByRole("menuitemradio", { name: enChat.header.preset.inherit });
};

describe("ChatPresetPicker", () => {
  it("lists Inherit, Base, then the agent's presets", async () => {
    setup(null);
    await open();
    expect(screen.getAllByRole("menuitemradio").map((i) => i.textContent?.trim())).toEqual([
      enChat.header.preset.inherit,
      enChat.header.preset.base,
      "fast",
      "deep",
    ]);
  });

  it("marks Inherit when the DM has no preset, Base for base, else the named preset", async () => {
    setup(null);
    await open();
    expect(screen.getByRole("menuitemradio", { name: enChat.header.preset.inherit })).toHaveAttribute("aria-checked", "true");
  });

  it("marks the named preset", async () => {
    setup("fast");
    await open();
    expect(screen.getByRole("menuitemradio", { name: "fast" })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("menuitemradio", { name: enChat.header.preset.inherit })).toHaveAttribute("aria-checked", "false");
  });

  it("sends reset, base or the name", async () => {
    const onSelect = setup("fast");
    await open();
    fireEvent.click(screen.getByRole("menuitemradio", { name: enChat.header.preset.inherit }));
    expect(onSelect).toHaveBeenLastCalledWith("reset");
    await open();
    fireEvent.click(screen.getByRole("menuitemradio", { name: enChat.header.preset.base }));
    expect(onSelect).toHaveBeenLastCalledWith("base");
    await open();
    fireEvent.click(screen.getByRole("menuitemradio", { name: "deep" }));
    expect(onSelect).toHaveBeenLastCalledWith("deep");
  });

  it("stays hidden for an agent with no presets while the DM inherits", async () => {
    setup(null, ["base", "reset"]);
    await waitFor(() => expect(getAgentPresetCompletions).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.queryByRole("button", { name: enChat.header.preset.label })).toBeNull();
  });
});
