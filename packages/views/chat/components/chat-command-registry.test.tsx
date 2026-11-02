import type { ComponentProps, ReactNode } from "react";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { createChatStore, registerChatStore, useChatStore } from "@multica/core/chat";
import { attachmentToDraftUpload } from "@multica/core/drafts";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Attachment } from "@multica/core/types";
import enCommon from "../../locales/en/common.json";
import enChat from "../../locales/en/chat.json";
import enEditor from "../../locales/en/editor.json";
import {
  SlashCommandList,
  createSlashCommandSuggestion,
  type SlashCommandItem,
} from "../../editor/extensions/slash-command-suggestion";
import { ChatInput } from "./chat-input";

vi.mock("@multica/core/platform", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/platform")>()),
  getCurrentWsId: () => "ws-c1",
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: { getState: () => ({ user: { id: "u1" } }) },
}));

const resources = { en: { common: enCommon, chat: enChat, editor: enEditor } };

function I18nWrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={resources}>
      {children}
    </I18nProvider>
  );
}

beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

beforeEach(() => {
  localStorage.clear();
  registerChatStore(createChatStore({ storage: localStorage }));
  useChatStore.setState({
    activeSessionId: "session-c1",
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

async function type(editor: Editor, text: string) {
  await waitFor(() => expect(editor.isInitialized).toBe(true));
  act(() => editor.view.dispatch(editor.state.tr.insertText(text)));
  await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
}

describe("CHE-1762 web command registry behavior", () => {
  it("matches /statistic by search terms stats, statistics, and usage in the slash suggestion list", () => {
    const queryClient = new QueryClient();
    const config = createSlashCommandSuggestion(
      queryClient,
      () => true,
      () => true,
      (cmd) => cmd === "statistic" || cmd === "archive" || cmd === "pin",
      () => true,
    );
    for (const query of ["stats", "statistics", "usage"]) {
      const editor = new Editor({
        extensions: [StarterKit],
        content: {
          type: "doc",
          content: [{ type: "paragraph", content: [{ type: "text", text: `/${query}` }] }],
        },
      });
      editor.commands.setTextSelection(editor.state.doc.content.size - 1);
      try {
        const matched = config.items!({
          query,
          editor,
          signal: new AbortController().signal,
        }) as SlashCommandItem[];
        expect(matched.map((item) => item.id)).toEqual(["statistic"]);
      } finally {
        editor.destroy();
      }
    }
  });

  it("shows the 'runs the agent' hint on prompt command /compact in SlashCommandList", () => {
    const compact: SlashCommandItem = { id: "compact", label: "compact", descriptionKey: "compact" };
    render(
      <I18nWrapper>
        <SlashCommandList items={[compact]} query="" command={vi.fn()} />
      </I18nWrapper>,
    );
    expect(screen.getByText(/runs the agent/i)).toBeInTheDocument();
  });

  it("executes standalone no-arg commands case-insensitively and with surrounding whitespace", async () => {
    const onArchive = vi.fn().mockResolvedValue(true);
    const { editor, onSend } = renderInput({ onArchive, sessionArchived: false });
    await type(editor, "  /ARCHIVE  ");

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onArchive).toHaveBeenCalledTimes(1));
    expect(onSend).not.toHaveBeenCalled();
  });

  it.each([
    ["/archive extra", { onArchive: vi.fn(), sessionArchived: false }],
    ["/unarchive extra", { onUnarchive: vi.fn(), sessionArchived: true }],
    ["/statistic extra", { onStatistic: vi.fn(), sessionArchived: false }],
    ["/pin extra", { onPin: vi.fn(), sessionArchived: false }],
  ] as const)(
    "rejects extra text on %s with an inline hint and preserves the draft without sending to the agent",
    async (text, props) => {
      const { editor, onSend } = renderInput(props);
      await type(editor, text);

      fireEvent.click(screen.getByRole("button", { name: /Send/ }));

      const alert = await screen.findByRole("alert");
      expect(alert).toHaveTextContent(/takes no arguments/i);
      expect(onSend).not.toHaveBeenCalled();
      expect(editor.getText()).toBe(text);
    },
  );

  it.each([
    ["/archive", { onArchive: vi.fn().mockResolvedValue(true) }],
    ["/statistic", { onStatistic: vi.fn().mockResolvedValue(true) }],
  ] as const)("rejects attachments on %s with an inline hint and retains the draft", async (command, props) => {
    const attachment: Attachment = {
      id: "att-1",
      workspace_id: "ws-c1",
      issue_id: null,
      comment_id: null,
      chat_message_id: null,
      chat_session_id: "session-c1",
      uploader_type: "member",
      uploader_id: "u1",
      filename: "report.txt",
      url: "https://cdn.example.com/report.txt",
      download_url: "https://cdn.example.com/report.txt",
      markdown_url: "https://cdn.example.com/report.txt",
      content_type: "text/plain",
      size_bytes: 42,
      created_at: "2026-10-22T00:00:00Z",
    };
    useChatStore.setState({
      activeSessionId: "session-c1",
      inputDrafts: { "session-c1": command },
      inputDraftAttachments: {
        "session-c1": [attachmentToDraftUpload(attachment)],
      },
    });

    const { editor, onSend } = renderInput({ ...props, sessionArchived: false });
    await waitFor(() => expect(editor.isInitialized).toBe(true));
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(
      `${command} cannot be sent with attachments. Remove attachments before running ${command}.`,
    );
    for (const handler of Object.values(props)) expect(handler).not.toHaveBeenCalled();
    expect(onSend).not.toHaveBeenCalled();
    expect(editor.getText()).toBe(command);
    expect(useChatStore.getState().inputDrafts["session-c1"]).toBe(command);
    expect(useChatStore.getState().inputDraftAttachments["session-c1"]).toEqual([attachmentToDraftUpload(attachment)]);
  });

  it("exempts /fork from the client attachment gate and forwards the attachment ids", async () => {
    const attachment: Attachment = {
      id: "att-fork-1",
      workspace_id: "ws-c1",
      issue_id: null,
      comment_id: null,
      chat_message_id: null,
      chat_session_id: "session-c1",
      uploader_type: "member",
      uploader_id: "u1",
      filename: "diagram.png",
      url: "https://cdn.example.com/diagram.png",
      download_url: "https://cdn.example.com/diagram.png",
      markdown_url: "https://cdn.example.com/diagram.png",
      content_type: "image/png",
      size_bytes: 128,
      created_at: "2026-10-22T00:00:00Z",
    };
    useChatStore.setState({
      activeSessionId: "session-c1",
      inputDrafts: { "session-c1": "/fork continue with plan B https://cdn.example.com/diagram.png" },
      inputDraftAttachments: {
        "session-c1": [attachmentToDraftUpload(attachment)],
      },
    });

    const onFork = vi.fn().mockResolvedValue({ ok: true });
    const { editor, onSend } = renderInput({ onFork, sessionArchived: false });
    await waitFor(() => expect(editor.isInitialized).toBe(true));
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onFork).toHaveBeenCalledTimes(1));
    expect(onFork).toHaveBeenCalledWith(expect.stringContaining("continue with plan B"), ["att-fork-1"]);
    expect(onSend).not.toHaveBeenCalled();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    await waitFor(() => expect(useChatStore.getState().inputDraftAttachments["session-c1"] ?? []).toHaveLength(0));
    expect(editor.getText()).toBe("");
  });
});
