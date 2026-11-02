import { createRef, forwardRef, useEffect, useImperativeHandle } from "react";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ContentEditorRef } from "../../editor/content-editor";

const editor = vi.hoisted(() => ({
  mounts: 0,
  calls: [] as string[],
  failNextLoad: false,
  // When set, the next chunk load waits for it (a slow network).
  gate: null as Promise<void> | null,
  created: 0,
  // A resolved `lazy` is cached for good, so every test starts from a fresh one.
  current: undefined as unknown,
  make: undefined as unknown as () => unknown,
}));

// The real editor is ~600 KiB of Tiptap; here a stub records what the host asks of it.
vi.mock("./lazy-content-editor", async () => {
  const { lazy } = await import("react");
  const Stub = forwardRef<ContentEditorRef, { onReady?: () => void; value?: string }>(
    function Stub({ onReady, value }, ref) {
      editor.mounts += 1;
      useEffect(() => void onReady?.(), []); // eslint-disable-line react-hooks/exhaustive-deps
      useImperativeHandle(ref, () => ({
        focus: () => void editor.calls.push("focus"),
        uploadFile: (f: File) => void editor.calls.push(`upload:${f.name}`),
        insertMarkdownAtEnd: (md: string) => (editor.calls.push(`insert:${md}`), true),
        getMarkdown: () => "live-md",
        getText: () => "live-text",
        hasActiveUploads: () => false,
        flushPendingUpdate: () => null,
        adoptContent: (md: string) => void editor.calls.push(`adopt:${md}`),
        clearContent: () => void editor.calls.push("clear"),
      }) as unknown as ContentEditorRef);
      return (
        <div data-testid="live-editor">
          {value}
        </div>
      );
    },
  );
  const make = () =>
    lazy(async () => {
      if (editor.gate) await editor.gate;
      if (editor.failNextLoad) {
        editor.failNextLoad = false;
        throw new Error("chunk load failed");
      }
      return { default: Stub as never };
    });
  editor.make = make;
  return {
    get LazyContentEditor() {
      return editor.current;
    },
    createLazyContentEditor: () => (editor.created++, make()),
    preloadContentEditor: vi.fn(() => void editor.calls.push("preload")),
  };
});
vi.mock("./deferred-content-editor", () => vi.importActual("./deferred-content-editor"));

import { DeferredContentEditor } from "./deferred-content-editor";

const standIn = () => screen.getByRole("textbox", { name: "Message" });

function setup(props: Record<string, unknown> = {}) {
  const ref = createRef<ContentEditorRef>();
  const view = render(
    <DeferredContentEditor ref={ref} ariaLabel="Message" placeholder="Type…" value="" {...props} />,
  );
  return { ref, ...view };
}

beforeEach(() => {
  editor.mounts = 0;
  editor.calls = [];
  editor.failNextLoad = false;
  editor.gate = null;
  editor.created = 0;
  editor.current = editor.make();
});

describe("DeferredContentEditor", () => {
  it("renders a static stand-in and does not mount the editor until the user shows intent", () => {
    setup();
    expect(standIn()).toBeInTheDocument();
    expect(screen.queryByTestId("live-editor")).toBeNull();
    expect(editor.mounts).toBe(0);
  });

  it("prefetches the chunk on hover and touch without mounting", () => {
    setup();
    fireEvent.pointerEnter(standIn());
    fireEvent.touchStart(standIn());
    expect(editor.calls.filter((c) => c === "preload")).toHaveLength(2);
    expect(editor.mounts).toBe(0);
  });

  it.each([
    ["click", () => fireEvent.pointerDown(standIn())],
    ["keyboard focus", () => fireEvent.focus(standIn())],
  ])("mounts the editor and focuses it after %s", async (_name, act_) => {
    setup();
    act(act_);
    expect(await screen.findByTestId("live-editor")).toBeInTheDocument();
    await waitFor(() => expect(editor.calls).toContain("focus"));
  });

  it("mounts without stealing focus when the draft already has text", async () => {
    setup({ value: "draft in progress" });
    expect(await screen.findByTestId("live-editor")).toBeInTheDocument();
    expect(editor.calls).not.toContain("focus");
  });

  it("mounts when staged attachments exist", async () => {
    setup({ attachments: [{ id: "a1" }] });
    expect(await screen.findByTestId("live-editor")).toBeInTheDocument();
  });

  it("keeps the editor mounted after the draft is cleared (a sent message)", async () => {
    const { rerender, ref } = setup({ value: "hi" });
    await screen.findByTestId("live-editor");
    rerender(<DeferredContentEditor ref={ref} ariaLabel="Message" placeholder="Type…" value="" />);
    expect(screen.getByTestId("live-editor")).toBeInTheDocument();
  });

  it("mounts and replays host calls that arrive before the editor exists", async () => {
    const { ref } = setup();
    expect(ref.current!.getMarkdown()).toBe("");
    expect(ref.current!.hasActiveUploads()).toBe(false);
    act(() => {
      ref.current!.focus();
      ref.current!.uploadFile(new File(["x"], "a.png"));
      expect(ref.current!.insertMarkdownAtEnd("> quote")).toBe(true);
    });
    await screen.findByTestId("live-editor");
    await waitFor(() => expect(editor.calls).toEqual(["focus", "upload:a.png", "insert:> quote"]));
  });

  it("talks to the live editor directly once it is mounted", async () => {
    const { ref } = setup({ value: "x" });
    await screen.findByTestId("live-editor");
    await waitFor(() => expect(ref.current!.getMarkdown()).toBe("live-md"));
  });

  it("falls back to the stand-in when the chunk fails and retries with a fresh import on the next intent", async () => {
    const logged = vi.spyOn(console, "error").mockImplementation(() => {});
    editor.failNextLoad = true;
    setup();
    fireEvent.pointerDown(standIn());
    await waitFor(() => expect(logged).toHaveBeenCalled()); // the error boundary caught the rejected import
    expect(screen.queryByTestId("live-editor")).toBeNull();
    expect(standIn()).toBeInTheDocument();
    fireEvent.pointerDown(standIn());
    expect(await screen.findByTestId("live-editor")).toBeInTheDocument();
    expect(editor.created).toBe(1);
  });

  describe("while the chunk is loading", () => {
    function slowLoad() {
      let release!: () => void;
      editor.gate = new Promise<void>((done) => (release = done));
      editor.current = editor.make();
      return release;
    }
    const type = (text: string) => {
      standIn().textContent = text;
      fireEvent.input(standIn());
    };

    it("keeps the same editable element, so focus and caret survive the swap", () => {
      slowLoad();
      setup();
      const before = standIn();
      fireEvent.focus(before);
      expect(standIn()).toBe(before);
      expect(before.getAttribute("contenteditable")).toBe("plaintext-only");
    });

    it("replays text typed before the editor arrived and hands focus over", async () => {
      const release = slowLoad();
      setup();
      fireEvent.focus(standIn());
      type("hello");
      expect(editor.mounts).toBe(0);
      await act(async () => release());
      await screen.findByTestId("live-editor");
      await waitFor(() => expect(editor.calls).toEqual(expect.arrayContaining(["insert:hello", "focus"])));
      expect(screen.queryByRole("textbox", { name: "Message" })).toBeNull(); // stand-in gone
    });

    it("does not replay or show a stand-in once the editor is live", async () => {
      const release = slowLoad();
      setup();
      fireEvent.focus(standIn());
      await act(async () => release());
      await screen.findByTestId("live-editor");
      await waitFor(() => expect(editor.calls).toContain("focus"));
      expect(editor.calls.some((c) => c.startsWith("insert:"))).toBe(false);
      expect(screen.queryByRole("textbox", { name: "Message" })).toBeNull();
    });
  });
});
