import type { ComponentProps } from "react";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Editor } from "@tiptap/core";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createChatStore, registerChatStore, useChatStore } from "@multica/core/chat";
import { attachmentToDraftUpload } from "@multica/core/drafts";
import { I18nProvider } from "@multica/core/i18n/react";
import type { Attachment } from "@multica/core/types";
import enCommon from "../../locales/en/common.json";
import enChat from "../../locales/en/chat.json";
import enEditor from "../../locales/en/editor.json";
import { ChatInput } from "./chat-input";

vi.mock("@multica/core/platform", async (importOriginal) => ({
  ...await importOriginal<typeof import("@multica/core/platform")>(),
  getCurrentWsId: () => "ws-fork",
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: { getState: () => ({ user: null }) },
}));

const resources = { en: { common: enCommon, chat: enChat, editor: enEditor } };
const SESSION = "session-fork";

beforeEach(() => {
  localStorage.clear();
  registerChatStore(createChatStore({ storage: localStorage }));
  useChatStore.setState({ activeSessionId: SESSION, inputDrafts: {}, inputDraftAttachments: {} });
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
}

const ok = () => vi.fn().mockResolvedValue({ ok: true });

describe("ChatInput /fork", () => {
  it("forks with the message, sends no chat message and clears the composer", async () => {
    const onFork = ok();
    const { editor, onSend } = renderInput({ onFork });
    await type(editor, "/fork continue with plan B");
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onFork).toHaveBeenCalledTimes(1));
    expect(onFork).toHaveBeenCalledWith("continue with plan B", []);
    expect(onSend).not.toHaveBeenCalled();
    await waitFor(() => expect(editor.getText()).toBe(""));
    expect(useChatStore.getState().inputDrafts[SESSION] ?? "").toBe("");
  });

  it("forks without a message for a bare /fork", async () => {
    const onFork = ok();
    const { editor, onSend } = renderInput({ onFork });
    await type(editor, "/fork");
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onFork).toHaveBeenCalledWith("", []));
    expect(onSend).not.toHaveBeenCalled();
  });

  it("forks while a turn runs on an agent without queued-message support", async () => {
    const onFork = ok();
    const onStop = vi.fn();
    const { editor, onSend } = renderInput({ onFork, onStop, isRunning: true, allowSubmitWhileRunning: false });
    await type(editor, "/fork");
    // The action slot offers Send for a fork input, not Stop.
    await waitFor(() => expect(screen.getByRole("button", { name: "Send" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() => expect(onFork).toHaveBeenCalledTimes(1));
    expect(onSend).not.toHaveBeenCalled();
    expect(onStop).not.toHaveBeenCalled();
  });

  it("still blocks an ordinary message while a turn runs without queue support", async () => {
    const onFork = ok();
    const { editor, onSend } = renderInput({ onFork, onStop: vi.fn(), isRunning: true, allowSubmitWhileRunning: false });
    await type(editor, "hello");
    editor.commands.focus();
    await act(async () => { editor.view.dom.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", metaKey: true, bubbles: true })); });
    expect(onSend).not.toHaveBeenCalled();
    expect(onFork).not.toHaveBeenCalled();
  });

  it("keeps the text and shows the server message when the fork is refused", async () => {
    const onFork = vi.fn().mockResolvedValue({ ok: false, error: "nothing to fork yet" });
    const { editor, onSend } = renderInput({ onFork });
    await type(editor, "/fork hi");
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    expect(await screen.findByText("nothing to fork yet")).toBeInTheDocument();
    expect(editor.getText()).toBe("/fork hi");
    expect(onSend).not.toHaveBeenCalled();
  });

  it("shows one error line and clears it once the text is edited", async () => {
    const onFork = vi.fn().mockResolvedValue({ ok: false, error: "fork is only available for web DMs" });
    const { editor } = renderInput({ onFork });
    await type(editor, "/fork hi");
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /Send/ }));
    await screen.findByText("fork is only available for web DMs");
    expect(screen.getAllByRole("alert")).toHaveLength(1);

    await type(editor, " more");
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
  });

  it("lets the person retry the same text after a failure", async () => {
    const onFork = vi.fn()
      .mockResolvedValueOnce({ ok: false, error: "Fork could not be confirmed." })
      .mockResolvedValueOnce({ ok: true });
    const { editor } = renderInput({ onFork });
    await type(editor, "/fork hi");
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /Send/ }));
    await screen.findByText("Fork could not be confirmed.");

    fireEvent.click(screen.getByRole("button", { name: /Send/ }));
    await waitFor(() => expect(onFork).toHaveBeenCalledTimes(2));
    expect(onFork.mock.calls[1]?.[0]).toBe("hi");
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
    expect(editor.getText()).toBe("");
  });

  it("forks with the draft attachment ids and clears the draft attachments", async () => {
    const attachment = {
      id: "att-1", url: "/api/attachments/att-1/download", download_url: "/api/attachments/att-1/download",
      filename: "a.png", content_type: "image/png", size_bytes: 1,
    } as unknown as Attachment;
    useChatStore.setState({ inputDraftAttachments: { [SESSION]: [attachmentToDraftUpload(attachment)] } });
    const onFork = ok();
    const { editor, onSend } = renderInput({ onFork });
    await type(editor, "/fork look /api/attachments/att-1/download");
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onFork).toHaveBeenCalledTimes(1));
    expect(onFork).toHaveBeenCalledWith(expect.stringContaining("look"), ["att-1"]);
    expect(onSend).not.toHaveBeenCalled();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    await waitFor(() => expect(useChatStore.getState().inputDraftAttachments[SESSION] ?? []).toHaveLength(0));
  });

  it("keeps text and attachment when the fork fails", async () => {
    const attachment = {
      id: "att-1", url: "/api/attachments/att-1/download", download_url: "/api/attachments/att-1/download",
      filename: "a.png", content_type: "image/png", size_bytes: 1,
    } as unknown as Attachment;
    useChatStore.setState({ inputDraftAttachments: { [SESSION]: [attachmentToDraftUpload(attachment)] } });
    const onFork = vi.fn().mockResolvedValue({ ok: false, error: "nope" });
    const { editor } = renderInput({ onFork });
    await type(editor, "/fork look /api/attachments/att-1/download");
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    expect(await screen.findByText("nope")).toBeInTheDocument();
    expect(editor.getText()).toContain("/fork look");
    expect(useChatStore.getState().inputDraftAttachments[SESSION]).toHaveLength(1);
  });

  it("sends /fork as a normal message when the composer has no fork handler", async () => {
    const { editor, onSend } = renderInput();
    await type(editor, "/fork hi");
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onSend).toHaveBeenCalled());
    expect(onSend.mock.calls[0]?.[0]).toBe("/fork hi");
  });

  it.each(["/forked now", "please /fork", "hello\n/fork"])("sends %j as a normal message", async (text) => {
    const onFork = ok();
    const { editor, onSend } = renderInput({ onFork });
    await waitFor(() => expect(editor.isInitialized).toBe(true));
    act(() => editor.commands.setContent(`<p>${text.replace("\n", "</p><p>")}</p>`));
    await waitFor(() => expect(screen.getByRole("button", { name: /Send/ })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: /Send/ }));

    await waitFor(() => expect(onSend).toHaveBeenCalled());
    expect(onFork).not.toHaveBeenCalled();
  });
});
