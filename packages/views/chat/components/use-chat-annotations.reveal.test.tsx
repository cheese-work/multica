import { useState } from "react";
import { act, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useCommentDraftStore } from "@multica/core/issues/stores";
import type { ChatMessage } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { AnnotatableAnswer } from "./annotatable-answer";
import { ChatAnnotationContext } from "./chat-annotation-context";
import { useChatAnnotations } from "./use-chat-annotations";

vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));

const message = (id: string): ChatMessage => ({
  id, chat_session_id: "s1", role: "assistant", content: "Selected text", task_id: null,
  created_at: "2026-10-20T00:00:00Z",
});

let edit: (id: string) => boolean = () => false;

// The list is virtualized: the source row only mounts once the list reveals it.
function Fixture({ messages }: { messages: ChatMessage[] }) {
  const annotation = useChatAnnotations({ sessionId: "s1", messages, agentName: "Terra", enabled: true });
  edit = (id) => annotation.input!.onEditAnnotation(id);
  const [mounted, setMounted] = useState(false);
  if (annotation.scope.revealMessageId === "m1" && !mounted) setMounted(true);
  return (
    <ChatAnnotationContext.Provider value={annotation.scope}>
      <div ref={annotation.cardRef} {...annotation.captureProps}>
        {annotation.popup}
        <span data-testid="reveal">{annotation.scope.revealMessageId ?? ""}</span>
        {mounted && <AnnotatableAnswer messageId="m1">Selected text</AnnotatableAnswer>}
      </div>
    </ChatAnnotationContext.Provider>
  );
}

beforeEach(() => {
  useCommentDraftStore.setState({ drafts: {} });
  Range.prototype.getBoundingClientRect = vi.fn(() => new DOMRect(10, 10, 120, 20));
  Range.prototype.getClientRects = vi.fn(() => [] as unknown as DOMRectList);
});
afterEach(() => vi.restoreAllMocks());

describe("editing a chat annotation whose source is not rendered", () => {
  it("asks the list to reveal the source, then opens the editor once it mounts", async () => {
    useCommentDraftStore.getState().addAnnotation("chat:s1", {
      id: "a1", sourceCommentId: "m1", sourceActorName: "Terra", quote: "Selected text", note: "Saved note", start: 0, prefix: "", suffix: "",
    });
    renderWithI18n(<Fixture messages={[message("m1")]} />);
    expect(screen.queryByText("Selected text")).toBeNull();

    let accepted = false;
    act(() => { accepted = edit("a1"); });
    expect(accepted).toBe(true);
    await waitFor(() => expect(screen.getByTestId("annotation-note")).toHaveValue("Saved note"));
    expect(screen.getByTestId("reveal").textContent).toBe("");
  });
});
