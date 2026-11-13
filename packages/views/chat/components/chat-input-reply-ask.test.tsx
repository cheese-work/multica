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
  getCurrentWsId: () => "ws-reply-ask",
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: { getState: () => ({ user: null }) },
}));

const resources = { en: { common: enCommon, chat: enChat, editor: enEditor } };

beforeEach(() => {
  localStorage.clear();
  registerChatStore(createChatStore({ storage: localStorage }));
  useChatStore.setState({
    activeSessionId: "session-reply-ask",
    inputDrafts: {},
    inputDraftAttachments: {},
  });
});

afterEach(cleanup);

function renderInput(props: Partial<ComponentProps<typeof ChatInput>> = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const onSend = vi.fn().mockResolvedValue(true);
  const view = render(
    <QueryClientProvider client={queryClient}>
      <I18nProvider locale="en" resources={resources}>
        <ChatInput agentName="Multica" onSend={onSend} {...props} />
      </I18nProvider>
    </QueryClientProvider>,
  );
  const editor = (view.container.querySelector(".tiptap") as HTMLElement & { editor: Editor }).editor;
  return { ...view, editor, onSend };
}

async function typeAndSend(editor: Editor, text: string) {
  await waitFor(() => expect(editor.isInitialized).toBe(true));
  act(() => editor.view.dispatch(editor.state.tr.insertText(text)));
  await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
  fireEvent.click(screen.getByRole("button", { name: /Send/ }));
}

describe("ChatInput reply-to-ask", () => {
  it("shows no control without open asks", () => {
    renderInput();
    expect(screen.queryByRole("checkbox")).toBeNull();
  });

  it("one open ask: checked by default and the send targets it", async () => {
    const { editor, onSend } = renderInput({ replyAskIds: ["ask-1"] });
    const box = screen.getByRole("checkbox", { name: "This message answers the open question" });
    expect(box).toBeChecked();
    await typeAndSend(editor, "yes, go ahead");
    await waitFor(() => expect(onSend).toHaveBeenCalled());
    expect(onSend.mock.calls[0]![5]).toEqual(["ask-1"]);
  });

  it("one open ask: unchecking sends a plain message", async () => {
    const { editor, onSend } = renderInput({ replyAskIds: ["ask-1"] });
    fireEvent.click(screen.getByRole("checkbox"));
    await typeAndSend(editor, "unrelated");
    await waitFor(() => expect(onSend).toHaveBeenCalled());
    expect(onSend.mock.calls[0]![5]).toBeUndefined();
  });

  it("several open asks: unchecked by default, checking targets all of them", async () => {
    const { editor, onSend } = renderInput({ replyAskIds: ["ask-1", "ask-2"] });
    const box = screen.getByRole("checkbox", { name: "This message answers the 2 open questions" });
    expect(box).not.toBeChecked();
    fireEvent.click(box);
    await typeAndSend(editor, "both answered");
    await waitFor(() => expect(onSend).toHaveBeenCalled());
    expect(onSend.mock.calls[0]![5]).toEqual(["ask-1", "ask-2"]);
  });

  it("several open asks: left unchecked sends no ask ids", async () => {
    const { editor, onSend } = renderInput({ replyAskIds: ["ask-1", "ask-2"] });
    await typeAndSend(editor, "hello");
    await waitFor(() => expect(onSend).toHaveBeenCalled());
    expect(onSend.mock.calls[0]![5]).toBeUndefined();
  });
});
