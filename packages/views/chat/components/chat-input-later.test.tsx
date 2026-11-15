import type { ComponentProps } from "react";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Editor } from "@tiptap/core";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createChatStore, registerChatStore, useChatStore } from "@multica/core/chat";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enChat from "../../locales/en/chat.json";
import enEditor from "../../locales/en/editor.json";
import { ChatInput } from "./chat-input";

vi.mock("@multica/core/platform", async (importOriginal) => ({
  ...await importOriginal<typeof import("@multica/core/platform")>(),
  getCurrentWsId: () => "ws-later",
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: { getState: () => ({ user: null }) },
}));

const resources = { en: { common: enCommon, chat: enChat, editor: enEditor } };

beforeEach(() => {
  localStorage.clear();
  registerChatStore(createChatStore({ storage: localStorage }));
  useChatStore.setState({
    activeSessionId: "session-later",
    inputDrafts: {},
    inputDraftAttachments: {},
  });
});

afterEach(cleanup);

function renderInput(props: Partial<ComponentProps<typeof ChatInput>> = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const onSend = vi.fn().mockResolvedValue(true);
  const element = (overrides = props) => (
    <QueryClientProvider client={queryClient}>
      <I18nProvider locale="en" resources={resources}>
        <ChatInput agentName="Multica" onSend={onSend} {...overrides} />
      </I18nProvider>
    </QueryClientProvider>
  );
  const view = render(element());
  const editor = (view.container.querySelector(".tiptap") as HTMLElement & { editor: Editor }).editor;
  return { ...view, editor, onSend, element };
}

describe("ChatInput /later", () => {
  const typeAndSend = async (editor: Editor, text: string) => {
    await waitFor(() => expect(editor.isInitialized).toBe(true));
    act(() => editor.view.dispatch(editor.state.tr.insertText(text)));
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /Send/ }));
  };

  it("E16: /later never reaches the agent as a message", async () => {
    const onLater = vi.fn().mockResolvedValue(true);
    const { editor, onSend } = renderInput({ onLater });
    await typeAndSend(editor, "/later 2h");
    await waitFor(() => expect(onLater).toHaveBeenCalledWith("2h"));
    expect(onSend).not.toHaveBeenCalled();
    expect(editor.getText()).toBe("");
  });

  it("passes an empty argument for a bare /later (server default 2h)", async () => {
    const onLater = vi.fn().mockResolvedValue(true);
    const { editor, onSend } = renderInput({ onLater });
    await typeAndSend(editor, "/later");
    await waitFor(() => expect(onLater).toHaveBeenCalledWith(""));
    expect(onSend).not.toHaveBeenCalled();
  });

  it("keeps the command text when the server did not confirm", async () => {
    const onLater = vi.fn().mockResolvedValue(false);
    const { editor, onSend } = renderInput({ onLater });
    await typeAndSend(editor, "/later 99d");
    await waitFor(() => expect(onLater).toHaveBeenCalledWith("99d"));
    expect(onSend).not.toHaveBeenCalled();
    expect(editor.getText()).toBe("/later 99d");
  });

  it("offers /later only while the composer has a later handler", () => {
    const { editor, element, rerender } = renderInput();
    const suggestion = editor.extensionManager.extensions.find((e) => e.name === "slashCommand")!.options.suggestion;
    const ids = () =>
      suggestion.items({ editor, query: "", signal: new AbortController().signal }).map((i: { id: string }) => i.id);
    expect(ids()).not.toContain("later");
    rerender(element({ onLater: vi.fn() }));
    expect(ids()).toContain("later");
  });
});
