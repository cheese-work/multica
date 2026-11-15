import { afterEach, describe, expect, it, vi } from "vitest";
import { copyText } from "./clipboard";

// A code block as the support-ticket draft in CHE-4386: spaces, tabs,
// newlines, quotes and literal `%` / `%20` sequences.
const SAMPLE = [
  'Subject: "Copy button" fails',
  "\tindented with a tab",
  "  two leading spaces and 100% sure",
  "literal %20 and %0A stay as typed",
  "",
].join("\n");

const originalClipboard = Object.getOwnPropertyDescriptor(navigator, "clipboard");

afterEach(() => {
  if (originalClipboard) Object.defineProperty(navigator, "clipboard", originalClipboard);
  else delete (navigator as { clipboard?: unknown }).clipboard;
  vi.restoreAllMocks();
});

function setClipboard(value: unknown) {
  Object.defineProperty(navigator, "clipboard", { value, configurable: true });
}

describe("copyText", () => {
  it("passes the exact text to navigator.clipboard.writeText", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    setClipboard({ writeText });

    expect(await copyText(SAMPLE)).toBe(true);
    expect(writeText).toHaveBeenCalledTimes(1);
    expect(writeText).toHaveBeenCalledWith(SAMPLE);
  });

  it("fallback: the textarea handed to execCommand holds the exact text", async () => {
    // iOS Safari path when the async API is missing or rejects.
    setClipboard({ writeText: vi.fn().mockRejectedValue(new Error("NotAllowedError")) });
    let copied: string | undefined;
    document.execCommand = vi.fn(() => {
      copied = document.querySelector("textarea")?.value;
      return true;
    });

    expect(await copyText(SAMPLE)).toBe(true);
    expect(copied).toBe(SAMPLE);
    expect(document.querySelector("textarea")).toBeNull();
  });

  it("fallback without the async API selects the whole text", async () => {
    setClipboard(undefined);
    // Model iOS Safari: select() on a readonly textarea is a no-op, so only
    // an explicit setSelectionRange() leaves the text selected.
    vi.spyOn(HTMLTextAreaElement.prototype, "select").mockImplementation(() => {});
    let selected: string | undefined;
    document.execCommand = vi.fn(() => {
      const ta = document.querySelector("textarea")!;
      selected = ta.value.slice(ta.selectionStart, ta.selectionEnd);
      return true;
    });

    expect(await copyText(SAMPLE)).toBe(true);
    expect(selected).toBe(SAMPLE);
  });

  it("fallback sets an explicit selection range, which iOS Safari needs", async () => {
    // iOS ignores textarea.select() on a readonly field; only
    // setSelectionRange() selects, and an empty selection copies nothing.
    setClipboard(undefined);
    const range = vi.spyOn(HTMLTextAreaElement.prototype, "setSelectionRange");
    document.execCommand = vi.fn(() => true);

    expect(await copyText(SAMPLE)).toBe(true);
    expect(range).toHaveBeenCalledWith(0, SAMPLE.length);
  });
});
