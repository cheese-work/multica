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
  getCurrentWsId: () => "ws-rename",
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: { getState: () => ({ user: null }) },
}));

const resources = { en: { common: enCommon, chat: enChat, editor: enEditor } };

beforeEach(() => {
  localStorage.clear();
  registerChatStore(createChatStore({ storage: localStorage }));
  useChatStore.setState({
    activeSessionId: "session-rename",
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

describe("ChatInput rename with the real editor", () => {
  it("saves literal title characters instead of their Markdown escapes", async () => {
    const onRename = vi.fn().mockResolvedValue(true);
    const { editor, onSend } = renderInput({ onRename });
    await waitFor(() => expect(editor.isInitialized).toBe(true));
    const title = "my_project *v2* [a] <b>";
    act(() => editor.view.dispatch(editor.state.tr.insertText(`/rename ${title}`)));
    expect(editor.getMarkdown()).not.toBe(`/rename ${title}`);

    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onRename).toHaveBeenCalledWith(title));
    expect(onSend).not.toHaveBeenCalled();
    expect(editor.getText()).toBe("");
  });

  it.each(["paragraph", "hardBreak"])("collapses a %s newline to a single-line title", async (breakType) => {
    const onRename = vi.fn().mockResolvedValue(true);
    const { editor, onSend } = renderInput({ onRename });
    await waitFor(() => expect(editor.isInitialized).toBe(true));
    act(() => editor.commands.setContent({
      type: "doc",
      content: breakType === "paragraph"
        ? [
            { type: "paragraph", content: [{ type: "text", text: "/rename a" }] },
            { type: "paragraph", content: [{ type: "text", text: "b" }] },
          ]
        : [{ type: "paragraph", content: [
            { type: "text", text: "/rename a" },
            { type: "hardBreak" },
            { type: "text", text: "b" },
          ] }],
    }));

    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onRename).toHaveBeenCalledWith("a b"));
    expect(onSend).not.toHaveBeenCalled();
  });

  it("offers rename only while the composer has a rename handler", () => {
    const { editor, element, rerender } = renderInput();
    const suggestion = editor.extensionManager.extensions.find((extension) => extension.name === "slashCommand")!.options.suggestion;
    const commandIds = () => suggestion.items({ editor, query: "", signal: new AbortController().signal })
      .map((item: { id: string }) => item.id);
    expect(commandIds()).toEqual(["compact"]);

    rerender(element({ onRename: vi.fn() }));
    expect(commandIds()).toEqual(["compact", "rename"]);

    rerender(element({}));
    expect(commandIds()).toEqual(["compact"]);
  });

  it.each(["/compact my_project *v2* <b>", "Please /rename my_project"])("sends %j unchanged", async (text) => {
    const onRename = vi.fn();
    const { editor, onSend } = renderInput({ onRename });
    await waitFor(() => expect(editor.isInitialized).toBe(true));
    act(() => editor.view.dispatch(editor.state.tr.insertText(text)));
    const markdown = editor.getMarkdown();
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onSend).toHaveBeenCalled());
    expect(onSend.mock.calls[0]?.[0]).toBe(markdown);
    expect(onRename).not.toHaveBeenCalled();
  });
});
