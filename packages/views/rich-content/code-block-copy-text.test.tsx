import { describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { renderWithI18n } from "../test/i18n";
import { NavigationProvider } from "../navigation/context";
import type { NavigationAdapter } from "../navigation/types";

const copyTextMock = vi.hoisted(() => vi.fn().mockResolvedValue(true));
vi.mock("@multica/ui/lib/clipboard", () => ({ copyText: copyTextMock }));
vi.mock("../editor/link-hover-card", () => ({
  useLinkHover: () => ({}),
  LinkHoverCard: () => null,
}));
vi.mock("../issues/hooks", () => ({ useResolveIssueIdentifier: () => null }));
vi.mock("@multica/core/api", () => ({
  api: { getAttachmentTextContent: vi.fn() },
  PreviewTooLargeError: class extends Error {},
  PreviewUnsupportedError: class extends Error {},
}));
vi.mock("mermaid", () => ({ default: { initialize: vi.fn(), render: vi.fn() } }));

import { RichContent } from "./rich-content";

const NAV = {
  push: vi.fn(),
  replace: vi.fn(),
  back: vi.fn(),
  pathname: "/acme/chat",
  searchParams: new URLSearchParams(),
  openInNewTab: vi.fn(),
  getShareableUrl: (p: string) => `https://app.example${p}`,
} as unknown as NavigationAdapter;

// A support-ticket-shaped draft: spaces, indentation, a URL, an issue key,
// a bare link-looking path with spaces, quotes and percent signs.
const BLOCK = [
  "Subject: Copy fails on iPhone",
  "  Steps: open https://app.example/acme/issues/CHE-4386 then tap Copy",
  "\tPath: /my docs/file name.txt",
  'Quote: "100% done" and literal %20 / %0A',
  "[a link](https://x.test/a b) CHE-4386",
].join("\n");

describe("rendered code block copy", () => {
  it.each(["text", ""])("hands the exact fence text to copyText (lang %j)", async (lang) => {
    renderWithI18n(
      <NavigationProvider value={NAV}>
        <RichContent content={"```" + lang + "\n" + BLOCK + "\n```"} />
      </NavigationProvider>,
    );
    fireEvent.click(await screen.findByRole("button", { name: /copy/i }));
    await waitFor(() => expect(copyTextMock).toHaveBeenCalled());
    expect(copyTextMock).toHaveBeenLastCalledWith(BLOCK);
  });
});
