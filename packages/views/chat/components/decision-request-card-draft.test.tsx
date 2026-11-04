import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { setCurrentWorkspace } from "@multica/core/platform";
import { act, screen } from "@testing-library/react";
import { forwardRef, useEffect, useImperativeHandle, useRef } from "react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@multica/core/api";
import { WorkspaceSlugProvider } from "@multica/core/paths";
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

  it("chat surface: option and note come back after unmount and remount", async () => {
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("chat-remount")));
    await user.click(screen.getByRole("radio", { name: /Option B/ }));
    await user.type(note(), "my note");
    first.unmount();

    renderWithI18n(chat(card("chat-remount")));
    expect(screen.getByRole("radio", { name: /Option B/ })).toBeChecked();
    expect(note()).toHaveValue("my note");
  });

  it("issue banner surface: option and note come back after unmount and remount", async () => {
    api.listOpenDecisionRequests.mockResolvedValue([{ ...card("banner-remount"), asking_agent_id: "a", asking_agent_name: "Terra", escalation_class: "spend", chat_session_id: "s", message_id: null }]);
    const user = userEvent.setup();
    const first = renderWithI18n(banner());
    await user.click(await screen.findByRole("radio", { name: /Option A/ }));
    await user.type(note(), "banner note");
    first.unmount();

    renderWithI18n(banner());
    expect(await screen.findByRole("radio", { name: /Option A/ })).toBeChecked();
    expect(note()).toHaveValue("banner note");
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

  it("a refused send keeps the draft across remount", async () => {
    api.answerDecisionRequest.mockRejectedValue(new ApiError("refused", 500, "Error"));
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("refused")));
    await user.click(screen.getByRole("radio", { name: /Option A/ }));
    await user.click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(api.answerDecisionRequest).toHaveBeenCalledTimes(1));
    first.unmount();

    renderWithI18n(chat(card("refused")));
    expect(screen.getByRole("radio", { name: /Option A/ })).toBeChecked();
  });

  it("a successful send clears the draft, even when the card unmounted mid-send", async () => {
    let resolve: (value?: unknown) => void = () => { throw new Error("not started"); };
    api.answerDecisionRequest.mockReturnValue(new Promise((r) => { resolve = r; }));
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("sent")));
    await user.click(screen.getByRole("radio", { name: /Option B/ }));
    await user.click(screen.getByRole("button", { name: "Send" }));
    await vi.waitFor(() => expect(api.answerDecisionRequest).toHaveBeenCalledTimes(1));
    first.unmount();
    resolve();
    await vi.waitFor(() => expect(useDecisionAnswerDraftStore.getState().hasDraft("decision:sent")).toBe(false));
  });

  it("Discard draft clears it for good", async () => {
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("discard")));
    await user.click(screen.getByRole("radio", { name: /Option A/ }));
    first.rerender(chat({ ...card("discard"), state: "answered" }));
    await user.click(screen.getByRole("button", { name: "Discard draft" }));
    first.unmount();

    renderWithI18n(chat(card("discard")));
    expect(screen.getByRole("radio", { name: /Option A/ })).not.toBeChecked();
  });
  it("keeps the last edit when the card unmounts before the editor's update timer fires", async () => {
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card("pending")));
    await user.type(note(), "last words");
    first.unmount();

    renderWithI18n(chat(card("pending")));
    expect(note()).toHaveValue("last words");
  });

  // An upload started in a card that unmounts before it settles still lands in the stored note,
  // appended once, and never later than a send, Discard, logout or workspace change. The tests
  // settle right after the unmount: no timer wait, so a microtask-late editor report is covered.
  const startUpload = async (id: string) => {
    let resolveUpload: (v: unknown) => void = () => { throw new Error("not started"); };
    api.uploadFile.mockReturnValue(new Promise((r) => { resolveUpload = r; }));
    const user = userEvent.setup();
    const first = renderWithI18n(chat(card(id)));
    await user.type(note(), "see");
    await vi.waitFor(() => expect(useDecisionAnswerDraftStore.getState().hasDraft(`decision:${id}`)).toBe(true), { timeout: 2000 });
    act(() => editorHandle.current?.uploadFile(new File(["x"], "shot.png", { type: "image/png" })));
    first.unmount();
    return {
      user,
      settle: async () => {
        resolveUpload({ id: "att", filename: "shot.png", content_type: "image/png", url: "/api/attachments/att/download", download_url: "/api/attachments/att/download" });
        await act(async () => { await uploads.settle?.(); });
      },
    };
  };
  const stored = (id: string) => useDecisionAnswerDraftStore.getState().getDraft(`decision:${id}`)?.text;
  const occurrences = (text: string | undefined) => (text?.match(/shot\.png/g) ?? []).length;

  it("a late upload adds its attachment to the stored note, once", async () => {
    const { settle } = await startUpload("late-note");
    await settle();
    expect(stored("late-note")).toMatch(/^see\n\n!\[shot\.png\]/);
    expect(occurrences(stored("late-note"))).toBe(1);
  });

  it("a card reopened before the upload settles shows the attachment once it does", async () => {
    const { settle } = await startUpload("late-reopen");
    renderWithI18n(chat(card("late-reopen")));
    expect(note()).toHaveValue("see");
    await settle();
    await vi.waitFor(() => expect((note() as HTMLTextAreaElement).value).toContain("shot.png"));
    expect(occurrences(stored("late-reopen"))).toBe(1);
  });

  it("Send stays disabled in the reopened card while the upload is pending", async () => {
    const { user, settle } = await startUpload("late-gate");
    renderWithI18n(chat(card("late-gate")));
    await user.click(screen.getByRole("radio", { name: /Option A/ }));
    expect(screen.getByRole("button", { name: "Send" })).toBeDisabled();
    await settle();
    await vi.waitFor(() => expect(screen.getByRole("button", { name: "Send" })).toBeEnabled());
  });

  it("a late upload keeps a note edited after the card was reopened", async () => {
    const { user, settle } = await startUpload("late-edit");
    renderWithI18n(chat(card("late-edit")));
    await user.type(note(), " and more");
    await vi.waitFor(() => expect(stored("late-edit")).toBe("see and more"), { timeout: 2000 });
    await settle();
    expect(stored("late-edit")).toMatch(/^see and more\n\n!\[shot\.png\]/);
    expect(occurrences(stored("late-edit"))).toBe(1);
  });

  it("a late upload does not bring the draft back after Discard", async () => {
    const { user, settle } = await startUpload("late-discard");
    const second = renderWithI18n(chat(card("late-discard")));
    second.rerender(chat({ ...card("late-discard"), state: "answered" }));
    await user.click(screen.getByRole("button", { name: "Discard draft" }));
    await settle();
    expect(useDecisionAnswerDraftStore.getState().hasDraft("decision:late-discard")).toBe(false);
  });

  it("a late upload writes nothing after logout", async () => {
    const { settle } = await startUpload("late-logout");
    resetAllRegisteredDrafts();
    await settle();
    expect(stored("late-logout")).toBeUndefined();
  });

  it("a late upload writes nothing into a different workspace", async () => {
    setCurrentWorkspace("acme", "ws-a");
    const { settle } = await startUpload("late-ws");
    setCurrentWorkspace("other", "ws-b");
    await settle();
    expect(stored("late-ws")).toBeUndefined();
    setCurrentWorkspace(null, null);
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
