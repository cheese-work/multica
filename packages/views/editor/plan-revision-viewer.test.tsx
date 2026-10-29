import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import type { Attachment } from "@multica/core/types";
import messages from "../locales/en/editor.json";
import { PlanRevisionViewer } from "./plan-revision-viewer";

const { list, content } = vi.hoisted(() => ({ list: vi.fn(), content: vi.fn() }));
vi.mock("@multica/core/api", () => ({ api: {
  listAttachments: list,
  getAttachmentTextContent: async (id: string) => ({ text: await content(id), originalContentType: "text/markdown" }),
  getIssue: async () => ({ identifier: "CHE-2124" }),
} }));
vi.mock("@multica/core/workspace/hooks", () => ({ useActorName: () => ({ getActorName: () => "Cheese" }) }));
vi.mock("@multica/core/paths", () => ({
  useWorkspaceSlug: () => "cheese-work",
  paths: { workspace: () => ({ issueDetail: (issue: string) => `/cheese-work/issues/${issue}` }) },
}));
vi.mock("../navigation", () => ({ AppLink: ({ children, ...props }: React.AnchorHTMLAttributes<HTMLAnchorElement>) => <a {...props}>{children}</a> }));
vi.mock("../i18n", () => ({ useT: () => ({
  t: (select: (value: typeof messages) => string, values: Record<string, unknown> = {}) =>
    String(select(messages)).replace(/\{\{(\w+)\}\}/g, (_match, key) => String(values[key] ?? key)),
}) }));
vi.mock("./readonly-content", () => ({ ReadonlyContent: ({ content: text }: { content: string }) => <article>{text}</article> }));

const current: Attachment = {
  id: "current", workspace_id: "workspace", issue_id: "issue", comment_id: null,
  chat_session_id: null, chat_message_id: null, uploader_type: "member", uploader_id: "cheese",
  filename: "plan-rev-3.md", url: "", download_url: "", markdown_url: "",
  content_type: "text/markdown", size_bytes: 100, created_at: "2026-10-27T04:53:00Z",
};
const base: Attachment = { ...current, id: "base", filename: "plan-rev-2.md" };
const older: Attachment = { ...current, id: "older", filename: "plan-rev-1.md" };
const document = Array.from({ length: 340 }, (_, index) => `Line ${index + 1}`);
const changed = document.with(8, "<script>literal, not executable</script>").with(309, "New conclusion").join("\n");

function mount(attachment = current) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const download = vi.fn();
  const close = vi.fn();
  render(<QueryClientProvider client={client}><PlanRevisionViewer attachment={attachment} onDownload={download} onClose={close} /></QueryClientProvider>);
  return { download, close };
}

beforeEach(() => {
  list.mockReset().mockResolvedValue([current, base, older]);
  content.mockReset().mockImplementation(async (id: string) => id === "current" ? changed : document.join("\n"));
  HTMLElement.prototype.scrollIntoView = vi.fn();
});
afterEach(cleanup);

describe("PlanRevisionViewer", () => {
  it("visually marks only the pressed diff mode", async () => {
    mount();
    await screen.findByRole("article");
    const split = screen.getByRole("button", { name: "Split" });
    const unify = screen.getByRole("button", { name: "Unify" });
    expect(unify).toHaveAttribute("aria-pressed", "true");
    expect(split).toHaveAttribute("aria-pressed", "false");
    expect(unify).toHaveClass("aria-pressed:bg-muted");
    expect(split).toHaveClass("aria-pressed:bg-muted");
    fireEvent.click(split);
    expect(split).toHaveAttribute("aria-pressed", "true");
    expect(unify).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(unify);
    expect(unify).toHaveAttribute("aria-pressed", "true");
    expect(split).toHaveAttribute("aria-pressed", "false");
  });

  it("moves focus and a visible outline between change sections", async () => {
    mount();
    await screen.findByRole("article");
    fireEvent.click(screen.getByRole("switch", { name: "Diff" }));
    const first = await screen.findByRole("region", { name: "Change 1 / 2" });
    const second = screen.getByRole("region", { name: "Change 2 / 2" });
    fireEvent.click(screen.getByRole("button", { name: "Next change" }));
    expect(second).toHaveFocus();
    expect(second).toHaveClass("outline-2", "outline-ring", "-outline-offset-2");
    expect(first).not.toHaveClass("outline-2");
    fireEvent.click(screen.getByRole("button", { name: "Previous change" }));
    expect(first).toHaveFocus();
    expect(first).toHaveClass("outline-2", "outline-ring", "-outline-offset-2");
    expect(second).not.toHaveClass("outline-2");
  });

  it("renders current Markdown first, without fetching a base, and preserves download/close", async () => {
    const { download, close } = mount();
    expect(await screen.findByRole("article")).toHaveTextContent("New conclusion");
    expect(screen.getByRole("switch", { name: "Diff" })).not.toBeChecked();
    expect(content.mock.calls.map(([id]) => id)).toEqual(["current"]);
    fireEvent.click(screen.getByRole("button", { name: "Download" }));
    fireEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(download).toHaveBeenCalledOnce();
    expect(close).toHaveBeenCalledOnce();
  });

  it("compares literal source with collapsed context, change navigation and aligned split rows", async () => {
    mount();
    await screen.findByRole("article");
    await waitFor(() => expect(screen.getByRole("switch", { name: "Diff" })).not.toHaveAttribute("aria-disabled", "true"));
    fireEvent.click(screen.getByRole("switch", { name: "Diff" }));
    expect(await screen.findByText("<script>literal, not executable</script>")).toBeVisible();
    expect(screen.getByText("Change 1 / 2")).toBeVisible();
    expect(screen.getAllByRole("button", { name: /unchanged lines/ }).length).toBeGreaterThan(0);
    expect(screen.queryByText("Line 200")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Next change" }));
    expect(screen.getByText("Change 2 / 2")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "Split" }));
    expect(screen.getByTestId("revision-diff")).toHaveAttribute("data-mode", "split");
    expect(screen.getAllByTestId("revision-split-row").every((row) => row.children.length === 2)).toBe(true);
    fireEvent.click(screen.getByRole("switch", { name: "Diff" }));
    expect(await screen.findByRole("article")).toBeVisible();
    expect(screen.queryByTestId("revision-diff")).not.toBeInTheDocument();
  });

  it("labels bases with ICT time and uploader, and clears stale output after a denied base", async () => {
    mount();
    await screen.findByRole("article");
    fireEvent.click(screen.getByRole("switch", { name: "Diff" }));
    await screen.findByTestId("revision-diff");
    content.mockImplementation(async (id: string) => {
      if (id === "older") throw new Error("403");
      return changed;
    });
    const picker = screen.getByRole("combobox", { name: "Compare with" });
    await userEvent.click(picker);
    const option = await screen.findByRole("option", { name: /plan-rev-1.md.*ICT · Cheese/ });
    await userEvent.click(option);
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not load comparison");
    expect(screen.queryByTestId("revision-diff")).not.toBeInTheDocument();
    expect(screen.getByRole("switch", { name: "Diff" })).not.toBeChecked();
  });

  it("does not silently choose a duplicate revision", async () => {
    list.mockResolvedValue([current, base, { ...base, id: "duplicate" }]);
    mount();
    await screen.findByRole("article");
    expect(screen.getByRole("switch", { name: "Diff" })).toHaveAttribute("aria-disabled", "true");
    expect(content.mock.calls.map(([id]) => id)).toEqual(["current"]);
    await userEvent.click(screen.getByRole("combobox", { name: "Compare with" }));
    await userEvent.click(await screen.findByRole("option", { name: /· base$/ }));
    fireEvent.click(screen.getByRole("switch", { name: "Diff" }));
    expect(await screen.findByTestId("revision-diff")).toBeVisible();
  });

  it("shows first-revision and empty-document states without inventing a diff", async () => {
    list.mockResolvedValue([current]);
    content.mockResolvedValue("");
    mount();
    expect(await screen.findByText("No earlier revisions")).toBeVisible();
    expect(screen.getByRole("switch", { name: "Diff" })).toHaveAttribute("aria-disabled", "true");
    expect(await screen.findByText("This document is empty")).toBeVisible();
  });

  it("does not render a diff while the base is loading and can cancel it", async () => {
    let resolveBase: (text: string) => void = () => {};
    content.mockImplementation((id: string) => id === "current" ? Promise.resolve(changed) : new Promise<string>((resolve) => { resolveBase = resolve; }));
    mount();
    await screen.findByRole("article");
    fireEvent.click(screen.getByRole("switch", { name: "Diff" }));
    expect(await screen.findByText("Loading comparison…")).toBeVisible();
    expect(screen.getByRole("switch", { name: "Diff" })).not.toBeChecked();
    expect(screen.queryByTestId("revision-diff")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("switch", { name: "Diff" }));
    resolveBase(document.join("\n"));
    await waitFor(() => expect(screen.queryByText("Loading comparison…")).not.toBeInTheDocument());
    expect(screen.getByRole("article")).toBeVisible();
    expect(screen.queryByTestId("revision-diff")).not.toBeInTheDocument();
  });

  it("keeps current Markdown visible when discovery fails and exposes recovery", async () => {
    list.mockRejectedValue(new Error("403"));
    mount();
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not load revisions");
    expect(await screen.findByRole("article")).toBeVisible();
    expect(content.mock.calls.map(([id]) => id)).toEqual(["current"]);
    list.mockResolvedValue([current, base]);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.getByRole("switch", { name: "Diff" })).not.toHaveAttribute("aria-disabled", "true"));
  });

  it("reports identical files without inventing added or removed lines", async () => {
    content.mockResolvedValue("Same document");
    mount();
    await screen.findByRole("article");
    fireEvent.click(screen.getByRole("switch", { name: "Diff" }));
    expect(await screen.findByText("No changes between these revisions")).toBeVisible();
    expect(screen.getByRole("switch", { name: "Diff" })).toBeChecked();
    expect(screen.getByText("Change 0 / 0")).toBeVisible();
    expect(screen.getByRole("button", { name: "Next change" })).toBeDisabled();
  });
});
