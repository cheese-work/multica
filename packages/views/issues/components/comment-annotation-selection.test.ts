import { describe, expect, it } from "vitest";
import { annotationRange, captureCommentSelection, indexCommentText } from "./comment-annotation-selection";
import { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";

function fixture() {
  document.body.innerHTML = '<div data-annotation-thread="thread"><div data-comment-content="source"><p>Hello <strong>world</strong></p><pre><code>one\ntwo</code><button>Copy</button></pre><p>Last</p></div><div data-comment-content="other">Other reply</div><div data-annotation-thread="nested"><div data-comment-content="nested-source">Nested</div></div></div>';
  const card = document.body.firstElementChild as HTMLElement;
  const root = card.firstElementChild as HTMLElement;
  return { card, root };
}

function select(start: Node, startOffset: number, end: Node, endOffset: number) {
  const range = document.createRange();
  range.setStart(start, startOffset);
  range.setEnd(end, endOffset);
  const selection = window.getSelection()!;
  selection.removeAllRanges(); selection.addRange(range);
  return selection;
}

describe("comment annotation selection", () => {
  it("captures and reanchors real ProseMirror code and paragraph DOM in editable descriptions", () => {
    document.body.innerHTML = '<div data-annotation-thread="new:issue"><div data-comment-content="description:issue"><div id="editor"></div></div></div>';
    const card = document.body.firstElementChild as HTMLElement;
    const root = card.firstElementChild as HTMLElement;
    const code = "steps:\n  - name: build\n    run: make";
    const editor = new Editor({ element: root.firstElementChild as HTMLElement, extensions: [StarterKit], content: {
      type: "doc", content: [
        { type: "codeBlock", content: [{ type: "text", text: code }] },
        { type: "paragraph", content: [{ type: "text", text: "Last" }] },
        { type: "paragraph" },
      ],
    } });
    try {
      const first = root.querySelector("code")!.firstChild!;
      const last = root.querySelector("p")!.firstChild!;
      const captured = captureCommentSelection(card, select(first, 0, last, 4), true)!;
      expect(captured.quote).toBe(code + "\nLast");
      expect(root.querySelector(".ProseMirror-trailingBreak")).not.toBeNull();
      const range = annotationRange(root, { ...captured, id: "a", sourceActorName: "Description", note: "" }, true)!;
      expect(range.startContainer).toBe(first);
      expect(range.endContainer).toBe(last);
      expect(range.endOffset).toBe(4);
    } finally { editor.destroy(); }
  });

  it("captures rendered text and code newlines, excludes controls, and restores the same Range", () => {
    const { card, root } = fixture();
    const first = root.querySelector("strong")!.firstChild!;
    const last = root.querySelector("code")!.firstChild!;
    const captured = captureCommentSelection(card, select(first, 0, last, 7))!;
    expect(captured.quote).toBe("world\none\ntwo");
    expect(indexCommentText(root).text).toBe("Hello world\none\ntwo\nLast\n");
    const range = annotationRange(root, { ...captured, id: "a", sourceActorName: "Agent", note: "" })!;
    expect(range.startContainer).toBe(first);
    expect(range.endContainer).toBe(last);
    expect(range.endOffset).toBe(7);
  });

  it("rejects selection spanning comments or owned by a nested card", () => {
    const { card, root } = fixture();
    expect(captureCommentSelection(card, select(root.querySelector("strong")!.firstChild!, 0, card.children[1]!.firstChild!, 5))).toBeNull();
    const nested = card.querySelector('[data-comment-content="nested-source"]')!.firstChild!;
    expect(captureCommentSelection(card, select(nested, 0, nested, 6))).toBeNull();
  });

  it("rejects selections in editors, controls, and whitespace", () => {
    const { card, root } = fixture();
    const control = root.querySelector("button")!.firstChild!;
    expect(captureCommentSelection(card, select(control, 0, control, 4))).toBeNull();
    const first = root.querySelector("p")!.firstChild!;
    expect(captureCommentSelection(card, select(first, 5, first, 6))).toBeNull();
  });

  it("captures a triple-click on the last block, whose range ends after the content root", () => {
    const { card, root } = fixture();
    const last = root.querySelector("p:last-child")!;
    // Browsers report a triple-click as (block, 0) -> (next sibling or parent, offset).
    const captured = captureCommentSelection(card, select(last, 0, card, 1));
    expect(captured?.quote).toBe("Last");
    expect(captured?.sourceCommentId).toBe("source");
  });

  it("captures a drag that starts in the text and ends in the content root padding", () => {
    const { card, root } = fixture();
    const first = root.querySelector("p")!.firstChild!;
    const captured = captureCommentSelection(card, select(first, 2, root, root.childNodes.length));
    expect(captured?.quote.startsWith("llo world")).toBe(true);
    expect(captured?.quote.endsWith("Last")).toBe(true);
  });

  it("captures a drag that starts before the content root and ends in its text", () => {
    const { card, root } = fixture();
    const first = root.querySelector("p")!.firstChild!;
    const captured = captureCommentSelection(card, select(card, 0, first, 5));
    expect(captured?.quote).toBe("Hello");
  });

  it("rejects a selection that runs from one thread card into another card's comment", () => {
    document.body.innerHTML = '<div id="a" data-annotation-thread="a"><div data-comment-content="a1"><p>Alpha text</p></div></div><div id="b" data-annotation-thread="b"><div data-comment-content="b1"><p>Beta text</p></div></div>';
    const a = document.getElementById("a")!;
    const start = a.querySelector("p")!.firstChild!;
    const end = document.getElementById("b")!.querySelector("p")!.firstChild!;
    expect(captureCommentSelection(a, select(start, 2, end, 4))).toBeNull();
    expect(captureCommentSelection(document.getElementById("b")!, select(start, 2, end, 4))).toBeNull();
  });
});
