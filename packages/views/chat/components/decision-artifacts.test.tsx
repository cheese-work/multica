import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { DecisionArtifact } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { DecisionArtifacts } from "./decision-artifacts";

const listDecisionArtifacts = vi.hoisted(() => vi.fn());
const getAttachmentTextContent = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", async (importActual) => ({
  ...(await importActual<typeof import("@multica/core/api")>()),
  api: { listDecisionArtifacts, getAttachmentTextContent },
}));

const downloadAttachment = vi.hoisted(() => vi.fn());
vi.mock("../../editor/use-download-attachment", () => ({ useDownloadAttachment: () => downloadAttachment }));

const sha = "3f9a2c1".padEnd(64, "0");
const artifact = (over: Partial<DecisionArtifact> = {}): DecisionArtifact => ({
  attachment_id: "att-5",
  label: "Design revision 5",
  sha256: sha,
  supersedes_attachment_id: "att-4",
  signed_by: { reviewer: "Sol", comment_id: "c1" },
  supersedes_signed_by: { reviewer: "Terra", comment_id: "c0" },
  // The artifacts endpoint returns the relative, authenticated path outside CloudFront mode.
  attachment: {
    id: "att-5", filename: "design-rev5.md", content_type: "text/markdown",
    size_bytes: 18_800, download_url: "/api/attachments/att-5/download",
  },
  diff: {
    previous_filename: "design-rev4.md",
    hunks: [
      { old_start: 22, new_start: 22, lines: [" - Open: radio list.", "-- Conflict: draft is discarded.", "+- Conflict: draft is kept.", "+- Offline: Send waits.", " - Disclaimed: card stays closed."] },
      { old_start: 68, new_start: 69, lines: [" ## Keyboard", "-Enter sends.", "+Enter sends; Shift+Enter adds a line."] },
    ],
  },
  ...over,
});

function renderArtifacts(artifacts: DecisionArtifact[], answered = false) {
  listDecisionArtifacts.mockResolvedValue(artifacts);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <DecisionArtifacts requestId="req-1" answered={answered} />
    </QueryClientProvider>,
  );
}

describe("DecisionArtifacts", () => {
  beforeEach(() => {
    listDecisionArtifacts.mockReset();
    getAttachmentTextContent.mockReset();
    downloadAttachment.mockReset();
  });

  it("renders nothing for an ask without files", async () => {
    const { container } = renderArtifacts([]);
    await waitFor(() => expect(listDecisionArtifacts).toHaveBeenCalledWith("req-1"));
    expect(container).toBeEmptyDOMElement();
  });

  it("shows each file as a card with name, size, short SHA and a download button", async () => {
    renderArtifacts([artifact()]);
    const section = await screen.findByRole("region", { name: "You are approving" });
    expect(within(section).getByText("Design revision 5")).toBeInTheDocument();
    expect(within(section).getByText(/design-rev5\.md/)).toHaveTextContent("18 KB");
    expect(within(section).getByText("3f9a2c1")).toBeInTheDocument();
    expect(within(section).getByText("Hash verified")).toBeInTheDocument();
    expect(within(section).queryByRole("link", { name: /Download/ })).toBeNull();
    expect(within(section).getByRole("button", { name: "Download design-rev5.md" })).toBeInTheDocument();
  });

  it("carries the element ids the admit sheet names", async () => {
    renderArtifacts([artifact()]);
    const section = await screen.findByTestId("decision-request-artifacts");
    for (const id of ["decision-artifact-file", "decision-artifact-signature", "decision-artifact-download", "decision-artifact-viewer"]) {
      expect(within(section).getByTestId(id)).toBeInTheDocument();
    }
  });

  it("downloads through the shared attachment downloader, not a bare link", async () => {
    renderArtifacts([artifact()]);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Download design-rev5.md" }));
    expect(downloadAttachment).toHaveBeenCalledWith("att-5");
  });

  it("calls an answered card 'You approved'", async () => {
    renderArtifacts([artifact()], true);
    expect(await screen.findByRole("region", { name: "You approved" })).toBeInTheDocument();
  });

  it("starts closed with a hint and opens a labelled tab panel on select", async () => {
    renderArtifacts([artifact()]);
    const user = userEvent.setup();
    expect(await screen.findByText("Select a tab to read the file here.")).toBeInTheDocument();
    expect(screen.queryByRole("tabpanel")).toBeNull();
    const changes = screen.getByRole("tab", { name: /Changes since design-rev4\.md/ });
    expect(changes).toHaveAttribute("aria-selected", "false");
    await user.click(changes);
    const panel = screen.getByRole("tabpanel");
    expect(changes).toHaveAttribute("aria-selected", "true");
    expect(changes).toHaveAttribute("aria-controls", panel.id);
    expect(panel).toHaveAttribute("aria-labelledby", changes.id);
    await user.click(changes);
    expect(screen.queryByRole("tabpanel")).toBeNull();
    expect(changes).toHaveAttribute("aria-selected", "false");
  });

  it("moves focus between tabs with the arrow, Home and End keys; only one tab is tabbable", async () => {
    renderArtifacts([artifact()]);
    const user = userEvent.setup();
    const changes = await screen.findByRole("tab", { name: /Changes since/ });
    const preview = screen.getByRole("tab", { name: /Preview/ });
    expect(changes).toHaveAttribute("tabindex", "0");
    expect(preview).toHaveAttribute("tabindex", "-1");
    changes.focus();
    await user.keyboard("{ArrowRight}");
    expect(preview).toHaveFocus();
    expect(preview).toHaveAttribute("tabindex", "0");
    expect(changes).toHaveAttribute("tabindex", "-1");
    await user.keyboard("{ArrowRight}");
    expect(changes).toHaveFocus();
    await user.keyboard("{ArrowLeft}");
    expect(preview).toHaveFocus();
    await user.keyboard("{Home}");
    expect(changes).toHaveFocus();
    await user.keyboard("{End}");
    expect(preview).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("tabpanel")).toBeInTheDocument();
  });

  it("shows only the changed hunks with +/- marks, a hidden-lines gap and the signature marker", async () => {
    renderArtifacts([artifact()]);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("tab", { name: /Changes since/ }));
    const panel = screen.getByRole("tabpanel");
    expect(within(panel).getByText("design-rev4.md signed by Terra")).toBeInTheDocument();
    expect(within(panel).queryByText(/signed by Sol/)).toBeNull();
    expect(within(panel).getByText("3 lines added, 2 removed.")).toBeInTheDocument();
    const region = within(panel).getByRole("region", { name: "Text changes since design-rev4.md, changed hunks only" });
    expect(region).toHaveAttribute("tabindex", "0");
    expect(within(region).getAllByText(/^\+$/)).toHaveLength(3);
    expect(within(region).getAllByText(/^−$/)).toHaveLength(2);
    expect(within(region).getByText("- Conflict: draft is kept.")).toBeInTheDocument();
    // hunk 1 covers new lines 22-25 (4 new-side lines); hunk 2 starts at 69
    expect(within(region).getByText("Unchanged lines hidden: 43")).toBeInTheDocument();
  });

  it("marks the current revision on the collapsed file card, signed or not", async () => {
    const signed = renderArtifacts([artifact()]);
    const card = await screen.findByRole("region", { name: "You are approving" });
    expect(screen.queryByRole("tabpanel")).toBeNull();
    expect(within(card).getByText("Design revision 5 signed by Sol")).toBeInTheDocument();
    signed.unmount();
    renderArtifacts([artifact({ signed_by: undefined })]);
    expect(await screen.findByText("Design revision 5 is not signed yet")).toBeInTheDocument();
  });

  it("shows the reviewer of a signed first revision that has no Changes tab", async () => {
    renderArtifacts([artifact({ supersedes_attachment_id: undefined, supersedes_signed_by: undefined, diff: undefined })]);
    expect(await screen.findByText("Design revision 5 signed by Sol")).toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: /Changes since/ })).toBeNull();
  });

  it("keeps the previous and the current signature apart: rev 4 signed, rev 5 not yet", async () => {
    renderArtifacts([artifact({ signed_by: undefined })]);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("tab", { name: /Changes since/ }));
    const panel = screen.getByRole("tabpanel");
    expect(within(panel).getByText("design-rev4.md signed by Terra")).toBeInTheDocument();
    expect(screen.getByText("Design revision 5 is not signed yet")).toBeInTheDocument();
    expect(within(panel).queryByText(/Design revision 5/)).toBeNull();
  });

  it("says so when the previous revision has no recorded signature", async () => {
    renderArtifacts([artifact({ supersedes_signed_by: undefined })]);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("tab", { name: /Changes since/ }));
    expect(within(screen.getByRole("tabpanel")).getByText("No signature recorded for design-rev4.md")).toBeInTheDocument();
  });

  it("offers only Preview for a first revision", async () => {
    renderArtifacts([artifact({ supersedes_attachment_id: undefined, diff: undefined })]);
    expect(await screen.findByRole("tab", { name: "Preview" })).toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: /Changes since/ })).toBeNull();
  });

  it("offers no viewer for a file that cannot be previewed or diffed", async () => {
    renderArtifacts([artifact({
      supersedes_attachment_id: undefined, diff: undefined,
      attachment: { id: "att-5", filename: "mock.png", content_type: "image/png", size_bytes: 10, download_url: "https://files.test/x" },
    })]);
    expect(await screen.findByRole("region", { name: "You are approving" })).toBeInTheDocument();
    expect(screen.queryByRole("tablist")).toBeNull();
  });

  it("renders the Markdown preview on demand", async () => {
    getAttachmentTextContent.mockResolvedValue({ text: "# Decision card\n\nBody line.\n", originalContentType: "text/markdown" });
    renderArtifacts([artifact()]);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("tab", { name: "Preview" }));
    expect(getAttachmentTextContent).toHaveBeenCalledWith("att-5");
    const region = await screen.findByRole("region", { name: "Rendered preview of design-rev5.md" });
    expect(region).toHaveAttribute("tabindex", "0");
    expect(await within(region).findByRole("heading", { name: "Decision card" })).toBeInTheDocument();
    expect(screen.getByText("Rendered from design-rev5.md. Scroll to read all 3 lines.")).toBeInTheDocument();
  });

  it("says so when the preview cannot load, and retries", async () => {
    getAttachmentTextContent.mockRejectedValueOnce(new Error("boom")).mockResolvedValue({ text: "# Back", originalContentType: "text/markdown" });
    renderArtifacts([artifact()]);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("tab", { name: "Preview" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not load the preview. Download the file to read it.");
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("heading", { name: "Back" })).toBeInTheDocument();
  });

  it.each([["too_large"], ["not_text"], ["unavailable"]] as const)("shows a banner when the changes cannot be shown (%s)", async (problem) => {
    renderArtifacts([artifact({ diff: { previous_filename: "design-rev4.md", hunks: [], problem } })]);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("tab", { name: /Changes since/ }));
    expect(screen.getByRole("alert")).toHaveTextContent("Could not load the changes. Download the files to read them.");
    await user.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(listDecisionArtifacts).toHaveBeenCalledTimes(2));
  });

  it("says when a diff has no text changes", async () => {
    renderArtifacts([artifact({ diff: { previous_filename: "design-rev4.md", hunks: [] } })]);
    const user = userEvent.setup();
    await user.click(await screen.findByRole("tab", { name: /Changes since/ }));
    expect(screen.getByText("No text changes.")).toBeInTheDocument();
  });

  it("names a file that was deleted after the ask was sent", async () => {
    renderArtifacts([artifact({ attachment: undefined, diff: undefined })]);
    expect(await screen.findByText("This file was deleted after the ask was sent.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Download/ })).toBeNull();
  });

  it("tells the reader when the file list cannot load", async () => {
    listDecisionArtifacts.mockRejectedValueOnce(new Error("boom")).mockResolvedValue([artifact()]);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderWithI18n(<QueryClientProvider client={client}><DecisionArtifacts requestId="req-1" answered={false} /></QueryClientProvider>);
    const user = userEvent.setup();
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not load the files this request approves.");
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("region", { name: "You are approving" })).toBeInTheDocument();
  });
});
