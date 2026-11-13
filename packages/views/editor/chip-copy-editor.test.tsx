/**
 * Live ProseMirror editor: the wrapper's React `onCopy` runs after
 * ProseMirror's own copy handler and overwrites the clipboard with chip keys
 * (CHE-3371). Wrapper wiring mirrors ContentEditor; chips are stubbed to the
 * token attribute the real IssueChip / ProjectChip emit.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render } from "@testing-library/react";
import { useEditor, EditorContent } from "@tiptap/react";
import type { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";
import { Markdown } from "@tiptap/markdown";
import { copyChipKeys } from "../rich-content/chip-copy";
import { createMarkdownCopyExtension } from "./extensions/markdown-copy";
import { BaseMentionExtension } from "./extensions/mention-extension";

vi.mock("../issues/components/issue-mention-card", () => ({
  IssueMentionCard: () => (
    <a href="/acme/issues/i1"><span data-copy-token="CHE-3229">CHE-3229 · long title</span></a>
  ),
}));
vi.mock("../projects/components/project-mention-card", () => ({
  ProjectMentionCard: () => (
    <a href="/acme/projects/p1"><span data-copy-token="Roadmap">🗺 Roadmap</span></a>
  ),
}));

const CONTENT = {
  type: "doc",
  content: [
    {
      type: "paragraph",
      content: [
        { type: "text", text: "see " },
        { type: "mention", attrs: { id: "i1", type: "issue", label: "CHE-3229" } },
        { type: "text", text: " and " },
        { type: "mention", attrs: { id: "p1", type: "project", label: "Roadmap" } },
        { type: "text", text: " now" },
      ],
    },
  ],
};

function Harness({ onReady, withHandler = true }: { onReady: (el: HTMLElement, editor: Editor) => void; withHandler?: boolean }) {
  const editor = useEditor({
    immediatelyRender: true,
    extensions: [
      StarterKit,
      Markdown,
      BaseMentionExtension,
      createMarkdownCopyExtension(),
    ],
    content: CONTENT,
  });
  return (
    <div
      data-testid="wrapper"
      ref={(el) => {
        if (el && editor) onReady(el, editor);
      }}
      onCopy={withHandler ? (e) => copyChipKeys(e.nativeEvent) : undefined}
    >
      <EditorContent editor={editor} />
    </div>
  );
}

afterEach(() => {
  document.body.innerHTML = "";
});

async function copyAll(withHandler: boolean) {
  let wrapper!: HTMLElement;
  let editor!: Editor;
  render(<Harness withHandler={withHandler} onReady={(el, ed) => ((wrapper = el), (editor = ed))} />);
  const pm = await vi.waitFor(() => {
    const el = wrapper.querySelector<HTMLElement>(".ProseMirror");
    expect(el?.querySelector("[data-copy-token]")).not.toBeNull();
    return el!;
  });

  // ProseMirror copies from its own selection; the chip handler from the DOM one.
  editor.commands.selectAll();
  const range = document.createRange();
  range.selectNodeContents(pm.querySelector("p")!);
  window.getSelection()!.removeAllRanges();
  window.getSelection()!.addRange(range);

  const data: Record<string, string> = {};
  await act(async () => {
    fireEvent.copy(pm, {
      clipboardData: { clearData: () => {}, setData: (t: string, v: string) => (data[t] = v), getData: (t: string) => data[t] ?? "" },
    });
  });
  return data;
}

describe("editor chip copy", () => {
  it("ProseMirror alone writes the markdown form (proves it ran first)", async () => {
    const data = await copyAll(false);
    expect(data["text/plain"]).toContain("mention://issue/i1");
  });

  it("copies issue and project chips as keys, text around them unchanged", async () => {
    const data = await copyAll(true);
    expect(data["text/plain"]).toBe("see CHE-3229 and Roadmap now");
    expect(data["text/html"]).toContain('href="http://localhost:3000/acme/issues/i1"');
  });
});
