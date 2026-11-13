import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

const copyTextMock = vi.hoisted(() => vi.fn().mockResolvedValue(true));

vi.mock("@multica/ui/lib/clipboard", () => ({ copyText: copyTextMock }));

vi.mock("../i18n", async () => {
  const editor = (await import("../locales/en/editor.json")).default;
  return {
    useT: () => ({
      t: (select: (bundle: Record<string, unknown>) => string) =>
        select(editor as Record<string, unknown>),
    }),
  };
});

import { trackFeedbackTimers } from "../test/feedback-timer";
import { CodeBlockShell } from "./rich-code-block";

describe("CodeBlockShell copy toolbar", () => {
  it("is revealed on touch screens, which have no hover (CHE-2939)", () => {
    render(
      <CodeBlockShell language="bash" code="echo hi">
        <code>echo hi</code>
      </CodeBlockShell>,
    );
    const toolbar = screen.getByRole("button", { name: /copy/i }).parentElement!;
    expect(toolbar.className).toContain("[@media(hover:none)]:opacity-100");
    // Desktop keeps hover-reveal.
    expect(toolbar.className).toContain("group-hover/code:opacity-100");
  });

  it("copies the block text", async () => {
    render(
      <CodeBlockShell language="bash" code="echo hi">
        <code>echo hi</code>
      </CodeBlockShell>,
    );
    fireEvent.click(screen.getByRole("button", { name: /copy/i }));
    await waitFor(() => expect(copyTextMock).toHaveBeenCalledWith("echo hi"));
  });

  it("clears the copy-feedback timer on unmount", async () => {
    const view = render(
      <CodeBlockShell language="bash" code="echo hi">
        <code>echo hi</code>
      </CodeBlockShell>,
    );
    const timers = trackFeedbackTimers();
    try {
      fireEvent.click(screen.getByRole("button", { name: /copy/i }));
      await waitFor(() => expect(timers.ids).toHaveLength(1));
      view.unmount();
      expect(timers.cleared(timers.ids[0])).toBe(true);
    } finally {
      timers.restore();
    }
  });

  it("starts no feedback timer when unmounted before the copy resolves", async () => {
    let resolveCopy!: (copied: boolean) => void;
    copyTextMock.mockReturnValueOnce(new Promise<boolean>((resolve) => (resolveCopy = resolve)));
    const view = render(
      <CodeBlockShell language="bash" code="echo hi">
        <code>echo hi</code>
      </CodeBlockShell>,
    );
    const timers = trackFeedbackTimers();
    try {
      fireEvent.click(screen.getByRole("button", { name: /copy/i }));
      view.unmount();
      resolveCopy(true);
      await Promise.resolve();
      await Promise.resolve();
      expect(timers.ids).toHaveLength(0);
    } finally {
      timers.restore();
    }
  });
});
