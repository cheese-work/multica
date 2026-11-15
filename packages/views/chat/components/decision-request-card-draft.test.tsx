import { DRAFT_SESSION_KEY, resetAllRegisteredDrafts } from "@multica/core/drafts";
import { act, screen } from "@testing-library/react";
import { forwardRef, useEffect, useImperativeHandle, useRef } from "react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@multica/core/api";
import { WorkspaceSlugProvider } from "@multica/core/paths";
import { setCurrentWorkspace } from "@multica/core/platform";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderWithI18n } from "../../test/i18n";
import { useDecisionAnswerDraftStore } from "@multica/core/issues/stores/decision-answer-draft-store";
import type { DecisionRequestCard as RequestCard } from "@multica/core/types";
import { DecisionRequestCard } from "./decision-request-card";
import { IssueDecisionBanner } from "../../issues/components/issue-decision-banner";

// CHE-3521: the answer draft (option + note) lives in the keyed draft store, so
// an unmount (virtualized scroll-out, route change, reload) cannot drop it.

vi.mock("@multica/core/hooks", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/hooks")>()),
  useWorkspaceId: () => "ws",
}));
vi.mock("../../navigation", async (importActual) => ({
  ...(await importActual<typeof import("../../navigation")>()),
  AppLink: ({ href, children, ...rest }: { href: string; children: React.ReactNode }) => <a href={href} {...rest}>{children}</a>,
}));
vi.mock("../../issues/components/issue-chip", () => ({
  IssueChip: ({ fallbackLabel }: { fallbackLabel?: string }) => <span>{fallbackLabel}</span>,
}));

const api = vi.hoisted(() => ({
  answerDecisionRequest: vi.fn(),
  listDecisionArtifacts: vi.fn(),
  listOpenDecisionRequests: vi.fn(),
  listTasksByIssue: vi.fn(),
  uploadFile: vi.fn(),
}));
vi.mock("@multica/core/api", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/api")>()),
  api,
}));

type EditorProps = {
  defaultValue?: string;
  ariaLabel?: string;
  flushPendingOnUnmount?: boolean;
  onUpdate?: (markdown: string, base?: string) => void;
  onSubmit?: () => void;
  onUploadFile?: (file: File, uploadId: string) => Promise<{ id: string; filename: string; url: string } | null>;
};
// Stand-in for Tiptap with the two contracts the card relies on: an update reaches `onUpdate` through
// a timer that unmount drops unless `flushPendingOnUnmount` is set (an upload still in flight is
// re-emitted when it settles, same opt-in), and `adoptContent` replaces the text.
const uploads = vi.hoisted(() => ({ settle: undefined as undefined | (() => Promise<void>) }));
const editorHandle = vi.hoisted(() => ({ current: undefined as undefined | { uploadFile: (file: File) => void } }));
vi.mock("./lazy-content-editor", () => ({
  LazyContentEditor: forwardRef(function MockEditor(props: EditorProps, ref) {
    const box = useRef<HTMLTextAreaElement>(null);
    const pending = useRef<string | null>(null);
    const lastValue = useRef(props.defaultValue ?? "");
    const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
    const latest = useRef(props);
    latest.current = props;
    useEffect(() => () => {
      clearTimeout(timer.current);
      if (latest.current.flushPendingOnUnmount && pending.current !== null) latest.current.onUpdate?.(pending.current);
    }, []);
    useImperativeHandle(ref, () => {
      const handle = {
      hasActiveUploads: () => false,
      getMarkdown: () => box.current?.value ?? "",
      adoptContent: (md: string) => { lastValue.current = md; if (box.current) box.current.value = md; },
      uploadFile: (file: File) => {
        const done = latest.current.onUploadFile?.(file, "u1");
        uploads.settle = async () => {
          const att = await done;
          if (att && latest.current.flushPendingOnUnmount) latest.current.onUpdate?.(`${lastValue.current}\n\n![${att.filename}](${att.url})`, lastValue.current);
        };
      },
      };
      editorHandle.current = handle;
      return handle as never;
    });
    return (
      <textarea
        ref={box}
        aria-label={props.ariaLabel}
        defaultValue={props.defaultValue ?? ""}
        onChange={(event) => {
          pending.current = lastValue.current = event.target.value;
          clearTimeout(timer.current);
          timer.current = setTimeout(() => { pending.current = null; latest.current.onUpdate?.(event.target.value); }, 200);
        }}
      />
    );
  }),
}));

const options = [{ label: "Option A", what_happens: "A happens" }, { label: "Option B", what_happens: "B happens" }];
const card = (id: string): RequestCard => ({
  id, issue_id: "issue", issue_identifier: "CHE-1", source_comment_id: "source",
  ask: "Pick one.", state: "open", created_at: "2026-10-26T23:30:00Z", options,
});
const notes = () => screen.getAllByRole("textbox", { name: "Add a note (optional)" });
const note = () => screen.getByRole("textbox", { name: "Add a note (optional)" });

const refuseSends = () => api.answerDecisionRequest.mockRejectedValue(new ApiError("refused", 500, "Error"));
const optionButton = (name: RegExp) => screen.getByRole("radio", { name });

const chat = (value: RequestCard) => (
  <QueryClientProvider client={new QueryClient()}><WorkspaceSlugProvider slug="acme"><DecisionRequestCard card={value} /></WorkspaceSlugProvider></QueryClientProvider>
);
const banner = () => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <WorkspaceSlugProvider slug="acme"><IssueDecisionBanner issueId="issue" status="need_decision" ownerAgentId="owner" /></WorkspaceSlugProvider>
  </QueryClientProvider>
);

describe("decision card draft survives remount", () => {
  beforeEach(() => {
    vi.resetAllMocks();
    useDecisionAnswerDraftStore.setState({ drafts: {} });
    api.listDecisionArtifacts.mockResolvedValue([]);
    api.listTasksByIssue.mockResolvedValue([]);
    api.answerDecisionRequest.mockResolvedValue({});
  });

  it("chat surface: the kept option and the note come back after unmount and remount", async () => {
    refuseSends();
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("chat-remount")));
    await user.click(optionButton(/Option B/));
    await user.type(note(), "my note");
    first.unmount();

    renderWithI18n(chat(card("chat-remount")));
    expect(note()).toHaveValue("my note");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(api.answerDecisionRequest).toHaveBeenLastCalledWith("chat-remount", { option: "Option B", text: "my note" }));
  });

  it("issue banner surface: the kept option and the note come back after unmount and remount", async () => {
    refuseSends();
    api.listOpenDecisionRequests.mockResolvedValue([{ ...card("banner-remount"), asking_agent_id: "a", asking_agent_name: "Terra", escalation_class: "spend", chat_session_id: "s", message_id: null }]);
    const user = userEvent.setup();
    const first = renderWithI18n(banner());
    await user.click(await screen.findByRole("radio", { name: /Option A/ }));
    await user.type(note(), "banner note");
    first.unmount();

    renderWithI18n(banner());
    await screen.findByRole("radio", { name: /Option A/ });
    expect(note()).toHaveValue("banner note");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(api.answerDecisionRequest).toHaveBeenLastCalledWith("banner-remount", { option: "Option A", text: "banner note" }));
  });

  it("a draft typed in the chat card shows in the banner card for the same request", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([{ ...card("shared"), asking_agent_id: "a", asking_agent_name: "Terra", escalation_class: "spend", chat_session_id: "s", message_id: null }]);
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("shared")));
    await user.type(note(), "typed in chat");
    first.unmount();

    renderWithI18n(banner());
    await screen.findByRole("radio", { name: /Option A/ });
    expect(note()).toHaveValue("typed in chat");
  });

  it("a draft written by the previous version under the bare request id still shows after upgrade and remount", async () => {
    useDecisionAnswerDraftStore.getState().setDraft("upgraded", { option: "Option B", text: "written before the upgrade" });
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("upgraded")));
    expect(note()).toHaveValue("written before the upgrade");
    first.unmount();

    renderWithI18n(chat(card("upgraded")));
    expect(note()).toHaveValue("written before the upgrade");
    await user.click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(api.answerDecisionRequest).toHaveBeenLastCalledWith("upgraded", { option: "Option B", text: "written before the upgrade" }));
  });

  it("a refused send keeps the draft across remount", async () => {
    refuseSends();
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("refused")));
    await user.click(optionButton(/Option A/));
    await user.click(screen.getByRole("button", { name: "Send" }));
    first.unmount();

    renderWithI18n(chat(card("refused")));
    await user.click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(api.answerDecisionRequest).toHaveBeenLastCalledWith("refused", { option: "Option A" }));
  });

  it("a successful send clears the draft, even when the card unmounted mid-send", async () => {
    let resolve: (value?: unknown) => void = () => { throw new Error("not started"); };
    api.answerDecisionRequest.mockReturnValue(new Promise((r) => { resolve = r; }));
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("sent")));
    await user.click(optionButton(/Option B/));
    await user.click(screen.getByRole("button", { name: "Send" }));
    first.unmount();
    resolve();
    await vi.waitFor(() => expect(useDecisionAnswerDraftStore.getState().hasDraft("sent")).toBe(false));
  });

  it("Discard draft clears it for good", async () => {
    refuseSends();
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("discard")));
    await user.click(optionButton(/Option A/));
    first.rerender(chat({ ...card("discard"), state: "answered" }));
    await user.click(screen.getByRole("button", { name: "Discard draft" }));
    first.unmount();

    renderWithI18n(chat(card("discard")));
    expect(screen.getByRole("button", { name: "Send" })).toBeDisabled();
  });
  it("keeps the last edit when the card unmounts before the editor's update timer fires", async () => {
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("pending")));
    await user.type(note(), "last words");
    first.unmount();

    renderWithI18n(chat(card("pending")));
    expect(note()).toHaveValue("last words");
  });

  // Cheese decision (CHE-3521): an upload still running when the card unmounts (scroll-out, route
  // change, close, workspace switch, logout) is cancelled. Nothing is filed into the draft later;
  // the card says so when it is shown again. Typed text and chosen options are kept.
  const NOTICE = "Upload cancelled because you left this card. Attach the file again.";
  const startUpload = async (id: string) => {
    let resolveUpload: (v: unknown) => void = () => { throw new Error("not started"); };
    api.uploadFile.mockImplementation((_file: File, _ctx: unknown, signal?: AbortSignal) => new Promise((resolve, reject) => {
      resolveUpload = resolve;
      signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
    }));
    refuseSends();
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card(id)));
    await user.click(optionButton(/Option B/));
    await user.type(note(), "see");
    await vi.waitFor(() => expect(useDecisionAnswerDraftStore.getState().hasDraft(id)).toBe(true), { timeout: 2000 });
    act(() => editorHandle.current?.uploadFile(new File(["x"], "shot.png", { type: "image/png" })));
    return {
      user,
      first,
      signal: () => api.uploadFile.mock.calls[0]?.[2] as AbortSignal | undefined,
      settle: async () => {
        resolveUpload({ id: "att", filename: "shot.png", content_type: "image/png", url: "/api/attachments/att/download", download_url: "/api/attachments/att/download" });
        await act(async () => { await uploads.settle?.(); });
      },
    };
  };
  const stored = (id: string) => useDecisionAnswerDraftStore.getState().getDraft(id);

  it("leaving the card aborts the upload and shows the notice on return, with text and option kept", async () => {
    const { first, signal, settle } = await startUpload("left");
    expect(signal()?.aborted).toBe(false);
    first.unmount();
    expect(signal()?.aborted).toBe(true);
    await settle();
    expect(stored("left")?.text).toBe("see");

    renderWithI18n(chat(card("left")));
    expect(screen.getByText(NOTICE)).toBeInTheDocument();
    expect(note()).toHaveValue("see");
    await userEvent.setup().click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(api.answerDecisionRequest).toHaveBeenLastCalledWith("left", { option: "Option B", text: "see" }));
  });

  it("after leaving mid-upload, Send is not gated and a new upload clears the notice", async () => {
    const { first, user } = await startUpload("left-send");
    first.unmount();
    renderWithI18n(chat(card("left-send")));
    expect(screen.getByRole("button", { name: "Send" })).toBeEnabled();
    api.uploadFile.mockReturnValue(new Promise(() => {}));
    act(() => editorHandle.current?.uploadFile(new File(["y"], "again.png", { type: "image/png" })));
    expect(screen.queryByText(NOTICE)).toBeNull();
    await user.click(screen.getByRole("button", { name: "Send" }));
  });

  it("an upload that settles while the card stays mounted is not cancelled and shows no notice", async () => {
    const { first, signal, settle } = await startUpload("stay");
    await settle();
    expect(signal()?.aborted).toBe(false);
    first.unmount();
    renderWithI18n(chat(card("stay")));
    expect(screen.queryByText(NOTICE)).toBeNull();
  });

  it("logout while the card is gone: a late settle writes nothing and leaves no stored draft", async () => {
    const { first, settle } = await startUpload("left-logout");
    first.unmount();
    resetAllRegisteredDrafts();
    window.localStorage.removeItem("multica_decision_answer_drafts");
    await settle();
    expect(window.localStorage.getItem("multica_decision_answer_drafts")).toBeNull();
    expect(stored("left-logout")).toBeUndefined();
  });

  const draftKeys = () => Object.keys(window.localStorage).filter((k) => k.startsWith("multica_decision_answer_drafts"));

  it("another tab logged out: unmounting an uploading card writes nothing back to shared storage", async () => {
    setCurrentWorkspace("acme", "ws-a");
    await act(async () => { await Promise.resolve(); });
    const { first } = await startUpload("xtab");
    // The typed note is stored already, so the unmount flush has nothing left to write.
    await vi.waitFor(() => expect(stored("xtab")?.text).toBe("see"), { timeout: 2000 });
    expect(draftKeys().length).toBeGreaterThan(0);
    // Tab B logs out and clears shared storage; this tab has not heard of it yet.
    draftKeys().forEach((k) => window.localStorage.removeItem(k));
    first.unmount();
    expect(draftKeys()).toEqual([]);
  });

  it("another tab logged out with a note edit still pending: unmounting writes nothing back to shared storage", async () => {
    setCurrentWorkspace("acme", "ws-a");
    await act(async () => { await Promise.resolve(); });
    const user = userEvent.setup();
    refuseSends();
    const first = renderWithI18n(chat(card("xtab-pending")));
    await user.click(optionButton(/Option B/));
    expect(draftKeys().length).toBeGreaterThan(0);
    // This edit is still inside the editor's update timer, so unmount flushes it.
    await user.type(note(), "private note");
    // Tab B logs out: it clears shared storage and ends the session. This tab has not heard of it.
    draftKeys().forEach((k) => window.localStorage.removeItem(k));
    window.localStorage.setItem(DRAFT_SESSION_KEY, `other-tab-${Math.random()}`);
    first.unmount();
    expect(draftKeys()).toEqual([]);
  });

  it("another tab logged out: a card mounted afterwards does not write the old note back", async () => {
    setCurrentWorkspace("acme", "ws-a");
    await act(async () => { await Promise.resolve(); });
    const user = userEvent.setup();
    refuseSends();
    renderWithI18n(chat(card("xtab-old")));
    await user.click(optionButton(/Option B/));
    await user.type(note(), "private note");
    await vi.waitFor(() => expect(stored("xtab-old")?.text).toBe("private note"), { timeout: 2000 });
    // Tab B logs out: shared storage is cleared and the session id changes. This tab has not heard of it.
    draftKeys().forEach((k) => window.localStorage.removeItem(k));
    window.localStorage.setItem(DRAFT_SESSION_KEY, `other-tab-${Math.random()}`);
    // A different card mounts in this tab and the user picks an option, which writes the draft map.
    renderWithI18n(chat(card("xtab-new")));
    await user.click(screen.getAllByRole("radio", { name: /Option B/ })[1]!);
    const persisted = draftKeys().map((k) => window.localStorage.getItem(k)).join("");
    expect(persisted).toContain("xtab-new");
    expect(persisted).not.toContain("private note");
  });

  it("workspace switch: the notice is kept for the card's own workspace, not the one switched to", async () => {
    setCurrentWorkspace("acme", "ws-a");
    await act(async () => { await Promise.resolve(); });
    const { first } = await startUpload("ws-switch");
    // Desktop route switch: the incoming layout sets the workspace in render, then the old card unmounts.
    setCurrentWorkspace("beta", "ws-b");
    first.unmount();
    await act(async () => { await Promise.resolve(); });
    const other = renderWithI18n(chat(card("ws-switch")));
    expect(screen.queryByText(NOTICE)).toBeNull();
    other.unmount();
    setCurrentWorkspace("acme", "ws-a");
    await act(async () => { await Promise.resolve(); });
    renderWithI18n(chat(card("ws-switch")));
    expect(screen.getByText(NOTICE)).toBeInTheDocument();
  });

  it("logout clears the cancel notice", async () => {
    const { first } = await startUpload("notice-logout");
    first.unmount();
    resetAllRegisteredDrafts();
    renderWithI18n(chat(card("notice-logout")));
    expect(screen.queryByText(NOTICE)).toBeNull();
  });

  it("Discard draft removes the notice", async () => {
    const { first, user } = await startUpload("left-discard");
    first.unmount();
    const second = renderWithI18n(chat(card("left-discard")));
    expect(screen.getByText(NOTICE)).toBeInTheDocument();
    second.rerender(chat({ ...card("left-discard"), state: "answered" }));
    await user.click(screen.getByRole("button", { name: "Discard draft" }));
    second.unmount();
    renderWithI18n(chat(card("left-discard")));
    expect(screen.queryByText(NOTICE)).toBeNull();
  });

  it("two mounted surfaces for one request stay in step, and sending from the other sends the note", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([{ ...card("both"), asking_agent_id: "a", asking_agent_name: "Terra", escalation_class: "spend", chat_session_id: "s", message_id: null }]);
    const user = userEvent.setup();
    renderWithI18n(<>{chat(card("both"))}{banner()}</>);
    await vi.waitFor(() => expect(notes()).toHaveLength(2));
    const [inChat, inBanner] = notes() as [HTMLElement, HTMLElement];
    await user.type(inChat, "typed in chat");
    await vi.waitFor(() => expect(inBanner).toHaveValue("typed in chat"));
    await user.click(screen.getAllByRole("radio", { name: /Option B/ })[1] as HTMLElement);
    await user.click(screen.getAllByRole("button", { name: "Send" })[1] as HTMLElement);
    await vi.waitFor(() => expect(api.answerDecisionRequest).toHaveBeenCalledWith("both", { option: "Option B", text: "typed in chat" }));
  });
});
