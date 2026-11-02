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
  getCurrentWsId: () => "ws-dm-commands",
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: { getState: () => ({ user: null }) },
}));

const resources = { en: { common: enCommon, chat: enChat, editor: enEditor } };

beforeEach(() => {
  localStorage.clear();
  registerChatStore(createChatStore({ storage: localStorage }));
  useChatStore.setState({
    activeSessionId: "session-dm",
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

async function type(editor: Editor, text: string) {
  await waitFor(() => expect(editor.isInitialized).toBe(true));
  act(() => editor.view.dispatch(editor.state.tr.insertText(text)));
  await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
}

function commandIds(editor: Editor): string[] {
  const suggestion = editor.extensionManager.extensions.find((e) => e.name === "slashCommand")!.options.suggestion;
  return suggestion
    .items({ editor, query: "", signal: new AbortController().signal })
    .map((item: { id: string }) => item.id);
}

describe("ChatInput DM commands", () => {
  it("offers statistic, archive and pin ahead of compact for an active DM", () => {
    const { editor } = renderInput({
      onStatistic: vi.fn(),
      onArchive: vi.fn(),
      onPin: vi.fn(),
    });
    expect(commandIds(editor)).toEqual(["statistic", "archive", "pin", "compact"]);
  });

  it("offers only unarchive on an archived DM", () => {
    const { editor } = renderInput({ onUnarchive: vi.fn(), sessionArchived: true });
    expect(commandIds(editor)).toEqual(["unarchive"]);
  });

  it("offers no DM command without a handler", () => {
    const { editor } = renderInput();
    expect(commandIds(editor)).toEqual(["compact"]);
  });

  it("opens the dialog for /statistic and sends nothing to the agent", async () => {
    const onStatistic = vi.fn();
    const { editor, onSend } = renderInput({ onStatistic });
    await type(editor, "/statistic");

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onStatistic).toHaveBeenCalledTimes(1));
    expect(onSend).not.toHaveBeenCalled();
    await waitFor(() => expect(editor.getText()).toBe(""));
  });

  it("archives for /archive without sending a message and clears the box", async () => {
    const onArchive = vi.fn().mockResolvedValue(true);
    const { editor, onSend } = renderInput({ onArchive });
    await type(editor, "/archive");

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onArchive).toHaveBeenCalledTimes(1));
    expect(onSend).not.toHaveBeenCalled();
    await waitFor(() => expect(editor.getText()).toBe(""));
  });

  it("keeps /archive in the box when the archive fails", async () => {
    const onArchive = vi.fn().mockResolvedValue(false);
    const { editor, onSend } = renderInput({ onArchive });
    await type(editor, "/archive");

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onArchive).toHaveBeenCalledTimes(1));
    expect(onSend).not.toHaveBeenCalled();
    expect(editor.getText()).toBe("/archive");
  });

  it("pins for /pin without sending a message", async () => {
    const onPin = vi.fn().mockResolvedValue(true);
    const { editor, onSend } = renderInput({ onPin });
    await type(editor, "/pin");

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onPin).toHaveBeenCalledTimes(1));
    expect(onSend).not.toHaveBeenCalled();
  });

  it("unarchives for /unarchive on an archived DM without sending a message", async () => {
    const onUnarchive = vi.fn().mockResolvedValue(true);
    const { editor, onSend } = renderInput({ onUnarchive, sessionArchived: true });
    await type(editor, "/unarchive");

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onUnarchive).toHaveBeenCalledTimes(1));
    expect(onSend).not.toHaveBeenCalled();
    await waitFor(() => expect(editor.getText()).toBe(""));
  });

  it("runs a DM command while an agent turn is running", async () => {
    const onStatistic = vi.fn();
    const { editor, onSend } = renderInput({ onStatistic, isRunning: true, allowSubmitWhileRunning: false });
    await type(editor, "/statistic");

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onStatistic).toHaveBeenCalledTimes(1));
    expect(onSend).not.toHaveBeenCalled();
  });

  it.each(["/archive now", "Please /archive", "/archived"])("sends %j as an ordinary message", async (text) => {
    const onArchive = vi.fn();
    const { editor, onSend } = renderInput({ onArchive });
    await type(editor, text);
    const markdown = editor.getMarkdown();

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onSend).toHaveBeenCalled());
    expect(onSend.mock.calls[0]?.[0]).toBe(markdown);
    expect(onArchive).not.toHaveBeenCalled();
  });

  it("sends /archive as an ordinary message when the surface has no archive handler", async () => {
    const { editor, onSend } = renderInput();
    await type(editor, "/archive");

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onSend).toHaveBeenCalled());
  });

  it("explains in the placeholder that an archived DM asks to unarchive on send", () => {
    const { container } = renderInput({ sessionArchived: true });
    expect(container.querySelector("[data-placeholder]")).toHaveAttribute(
      "data-placeholder",
      enChat.input.placeholder_archived_send,
    );
  });
});
