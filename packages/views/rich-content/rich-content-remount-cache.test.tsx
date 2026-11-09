/**
 * CHE-4808: scrolling a long chat remounts rows inside Virtuoso's scroll
 * handler. A settled message must not run the markdown pipeline again on a
 * remount; a streaming one must not fill the cache with every partial token.
 */
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/react";
import { configStore } from "@multica/core/config";

const highlight = vi.hoisted(() => ({ calls: 0 }));
vi.mock("../editor/utils/highlight-markdown", async (importOriginal) => {
  const real = await importOriginal<typeof import("../editor/utils/highlight-markdown")>();
  return {
    ...real,
    highlightToHtml: (markdown: string) => {
      highlight.calls++;
      return real.highlightToHtml(markdown);
    },
  };
});

vi.mock("../issues/hooks", () => ({
  useResolveIssueIdentifier: () => null,
}));

vi.mock("../i18n", async () => {
  const editor = (await import("../locales/en/editor.json")).default;
  return {
    useT: () => ({
      t: (select: (bundle: typeof editor) => string) => select(editor),
    }),
    useTimeAgo: () => "just now",
  };
});

vi.mock("@multica/core/api", () => ({
  api: { getAttachmentTextContent: vi.fn() },
  PreviewTooLargeError: class extends Error {},
  PreviewUnsupportedError: class extends Error {},
}));

vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({
    issueDetail: (id: string) => `/acme/issues/${id}`,
    projectDetail: (id: string) => `/acme/projects/${id}`,
  }),
  useWorkspaceSlug: () => "acme",
}));

vi.mock("../navigation", () => ({
  useNavigation: () => ({ push: vi.fn(), openInNewTab: vi.fn() }),
  useOptionalNavigation: () => ({ push: vi.fn(), openInNewTab: vi.fn() }),
  resolveClickIntent: () => "push",
  useAppOrigin: () => null,
  AppLink: ({ href, children }: { href: string; children: React.ReactNode }) => (
    <a href={href}>{children}</a>
  ),
}));

vi.mock("../issues/components/issue-mention-card", () => ({
  IssueMentionCard: ({ issueId }: { issueId: string }) => <span>{issueId}</span>,
}));

vi.mock("../projects/components/project-chip", () => ({
  ProjectChip: ({ projectId }: { projectId: string }) => <span>{projectId}</span>,
}));

vi.mock("../editor/link-hover-card", () => ({
  useLinkHover: () => ({}),
  LinkHoverCard: () => null,
}));

// Surface what the file-card renderer received, so the assertion is about the
// emitted attachment rather than incidental chrome.
vi.mock("../editor/attachment", () => ({
  Attachment: ({ attachment }: { attachment: { url: string; filename: string } }) => (
    <span data-testid="file-card" data-url={attachment.url}>
      {attachment.filename}
    </span>
  ),
}));

import { RichContent } from "./rich-content";

const body = (n: number) => `## Reply ${n}\n\n\`\`\`ts\nconst v = ${n};\n\`\`\`\n\n| a | b |\n| - | - |\n| 1 | 2 |`;

beforeEach(() => {
  configStore.setState({ cdnDomain: "", cdnSigned: false });
  highlight.calls = 0;
});

describe("RichContent remount cache", () => {
  it("runs the pipeline once for a settled message mounted twice", () => {
    const first = render(<RichContent content={body(1)} />);
    const text = first.container.textContent;
    first.unmount();
    const second = render(<RichContent content={body(1)} />);
    expect(highlight.calls).toBe(1);
    expect(second.container.textContent).toBe(text);
    expect(second.container.querySelectorAll("table, pre")).toHaveLength(2);
  });

  it("runs the pipeline again for different content", () => {
    render(<RichContent content={body(2)} />).unmount();
    render(<RichContent content={body(3)} />);
    expect(highlight.calls).toBe(2);
  });

  it("does not cache streaming content", () => {
    render(<RichContent content={body(4)} phase="streaming" />).unmount();
    render(<RichContent content={body(4)} phase="streaming" />);
    expect(highlight.calls).toBe(2);
  });

  it("reprocesses when the CDN domain changes", () => {
    render(<RichContent content={body(5)} />).unmount();
    configStore.setState({ cdnDomain: "cdn.example.com" });
    render(<RichContent content={body(5)} />);
    expect(highlight.calls).toBe(2);
  });
});
