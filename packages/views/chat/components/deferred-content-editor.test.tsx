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
  // What the live editor reports as its text (empty unless a test loads it with content).
  text: "",
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
        getText: () => editor.text,
        hasActiveUploads: () => false,
        flushPendingUpdate: () => null,
        adoptContent: (md: string) => void editor.calls.push(`adopt:${md}`),
        clearContent: () => void editor.calls.push("clear"),
        flagMentions: (ids: string[]) => void editor.calls.push(`flag:${ids.join(",")}`),
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
  editor.text = "";
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

  it("forwards flagMentions to the live editor and ignores it before the editor mounts", async () => {
    const { ref } = setup({ value: "draft in progress" });
    act(() => ref.current?.flagMentions(["early"]));
    expect(editor.calls).not.toContain("flag:early");
    expect(await screen.findByTestId("live-editor")).toBeInTheDocument();
    await waitFor(() => expect(ref.current?.hasActiveUploads()).toBe(false));
    act(() => ref.current?.flagMentions(["a", "b"]));
    expect(editor.calls).toContain("flag:a,b");
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

  it("flags mentions on the live editor and ignores the call while only the stand-in exists", async () => {
    const { ref } = setup({ value: "x" });
    expect(() => ref.current!.flagMentions(["early"])).not.toThrow();
    await screen.findByTestId("live-editor");
    ref.current!.flagMentions(["a", "b"]);
    expect(editor.calls).toContain("flag:a,b");
    expect(editor.calls).not.toContain("flag:early");
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

    it("keeps whitespace exactly as typed when it replays into an uncontrolled editor", async () => {
      const release = slowLoad();
      setup({ value: undefined });
      fireEvent.focus(standIn());
      type("  indented\n  and trailing  \n");
      await act(async () => release());
      await screen.findByTestId("live-editor");
      await waitFor(() => expect(editor.calls).toContain("insert:  indented\n  and trailing  \n"));
    });

    it("reports typed text to the host draft at once, untrimmed, so it is never only in the stand-in", () => {
      slowLoad();
      const onUpdate = vi.fn();
      setup({ onUpdate });
      fireEvent.focus(standIn());
      type("  hi  ");
      expect(onUpdate).toHaveBeenLastCalledWith("  hi  ", "");
    });

    it("keeps text on the draft it was typed in when the host switches drafts mid-load", async () => {
      const release = slowLoad();
      const onUpdate = vi.fn();
      const { rerender, ref } = setup({ onUpdate });
      fireEvent.focus(standIn());
      type("from A");
      expect(onUpdate).toHaveBeenLastCalledWith("from A", "");
      // The host switches to draft B (controlled value changes) while the chunk is still loading.
      editor.text = "B draft";
      rerender(
        <DeferredContentEditor ref={ref} ariaLabel="Message" placeholder="Type…" value="B draft" onUpdate={onUpdate} />,
      );
      expect(standIn().textContent).toBe("B draft");
      await act(async () => release());
      await screen.findByTestId("live-editor");
      await waitFor(() => expect(editor.calls).toContain("focus"));
      expect(editor.calls.some((c) => c.startsWith("insert:"))).toBe(false);
      expect(onUpdate).toHaveBeenCalledTimes(1); // nothing was written to B
    });

    describe("when the host switches draft (draftKey) mid-load", () => {
      const withKey = (ref: React.Ref<ContentEditorRef>, draftKey: string) => (
        <DeferredContentEditor ref={ref} ariaLabel="Message" placeholder="Type…" value="" draftKey={draftKey} />
      );

      it("drops a file and a quote queued on draft A instead of replaying them into draft B", async () => {
        const release = slowLoad();
        const { rerender, ref } = setup({ draftKey: "A" });
        act(() => {
          ref.current!.uploadFile(new File(["x"], "a-secret.png"));
          ref.current!.insertMarkdownAtEnd("> quote from A");
        });
        rerender(withKey(ref, "B"));
        act(() => ref.current!.focus()); // belongs to B; proves the replay ran
        await act(async () => release());
        await screen.findByTestId("live-editor");
        await waitFor(() => expect(editor.calls).toContain("focus"));
        expect(editor.calls).not.toContain("upload:a-secret.png");
        expect(editor.calls).not.toContain("insert:> quote from A");
      });

      it("keeps content the host queues after the switch, which belongs to draft B", async () => {
        const release = slowLoad();
        const { rerender, ref } = setup({ draftKey: "A" });
        act(() => void ref.current!.insertMarkdownAtEnd("> from A"));
        rerender(withKey(ref, "B"));
        act(() => void ref.current!.insertMarkdownAtEnd("> from B"));
        await act(async () => release());
        await screen.findByTestId("live-editor");
        await waitFor(() => expect(editor.calls).toContain("insert:> from B"));
        expect(editor.calls).not.toContain("insert:> from A");
      });

      it("does not carry text typed on draft A into draft B when both drafts are empty", async () => {
        const release = slowLoad();
        const { rerender, ref } = setup({ draftKey: "A", value: undefined });
        fireEvent.focus(standIn());
        type("typed on A");
        rerender(
          <DeferredContentEditor ref={ref} ariaLabel="Message" placeholder="Type…" draftKey="B" />,
        );
        expect(standIn().textContent).toBe("");
        await act(async () => release());
        await screen.findByTestId("live-editor");
        await waitFor(() => expect(editor.calls).toContain("focus"));
        expect(editor.calls.some((c) => c.startsWith("insert:"))).toBe(false);
      });
    });

    it("shows an empty stand-in and replays nothing when the new draft is empty", async () => {
      const release = slowLoad();
      const { rerender, ref } = setup({ value: "" });
      fireEvent.focus(standIn());
      type("from A");
      rerender(<DeferredContentEditor ref={ref} ariaLabel="Message" placeholder="Type…" value="x" />);
      rerender(<DeferredContentEditor ref={ref} ariaLabel="Message" placeholder="Type…" value="" />);
      expect(standIn().textContent).toBe("");
      await act(async () => release());
      await screen.findByTestId("live-editor");
      expect(editor.calls.some((c) => c.startsWith("insert:"))).toBe(false);
    });

    it("keeps the typed text in the host draft when unmounted during a delayed load", async () => {
      const release = slowLoad();
      const onUpdate = vi.fn();
      const { unmount } = setup({ onUpdate });
      fireEvent.focus(standIn());
      type("keep me");
      unmount();
      await act(async () => release());
      expect(onUpdate).toHaveBeenLastCalledWith("keep me", "");
    });

    it("answers getMarkdown from the stand-in text before the editor exists", () => {
      slowLoad();
      const { ref } = setup();
      fireEvent.focus(standIn());
      type("draft so far");
      expect(ref.current!.getMarkdown()).toBe("draft so far");
    });

    it("keeps the typed text in the host draft when the chunk fails to load", async () => {
      const logged = vi.spyOn(console, "error").mockImplementation(() => {});
      editor.failNextLoad = true;
      const onUpdate = vi.fn();
      setup({ onUpdate });
      fireEvent.focus(standIn());
      type("typed");
      await waitFor(() => expect(logged).toHaveBeenCalled());
      expect(standIn().textContent).toBe("typed");
      expect(onUpdate).toHaveBeenLastCalledWith("typed", "");
    });
  });
});
