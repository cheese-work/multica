import { expect, test } from "@playwright/test";
import { createTestApi, loginAsDefault, waitForPageText } from "./helpers";
import type { TestApiClient } from "./fixtures";

interface RowMeasurement {
  readonly totalRows: number;
  readonly hiddenRows: number;
}

/** Long enough to overflow the 12-line collapsed preview at the default test viewport. */
const LONG_DESCRIPTION = Array.from({ length: 30 }, (_, i) => `Line ${i + 1} of the description.`).join("\n\n");

test("Range geometry distinguishes exactly twelve and thirteen visual lines", async ({ page }) => {
  for (const lineCount of [12, 13]) {
    await page.setContent(`
      <style>
        #fixture { font: 16px/20px monospace; width: 480px; }
        #fixture span { display: block; height: 20px; margin: 0; padding: 0; }
      </style>
      <div id="fixture">${Array.from({ length: lineCount }, (_, index) => `<span>line ${index + 1}</span>`).join("")}</div>
    `);

    const result = await page.locator("#fixture").evaluate((root): RowMeasurement & { oracleRows: number } => {
      const epsilon = 0.5;
      const rootTop = root.getBoundingClientRect().top;
      const lineHeight = Number.parseFloat(getComputedStyle(root).lineHeight);
      const cutoff = rootTop + lineHeight * 12;
      const intervals: Array<{ top: number; bottom: number }> = [];

      const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        if (!node.textContent?.trim()) continue;
        const range = document.createRange();
        range.selectNodeContents(node);
        for (const rect of range.getClientRects()) {
          if (rect.width > 0 && rect.height > 0) intervals.push({ top: rect.top, bottom: rect.bottom });
        }
      }
      intervals.sort((a, b) => a.top - b.top || a.bottom - b.bottom);
      const rows: Array<{ top: number; bottom: number }> = [];
      for (const interval of intervals) {
        const previous = rows.at(-1);
        if (previous && interval.top < previous.bottom - epsilon) previous.bottom = Math.max(previous.bottom, interval.bottom);
        else rows.push({ ...interval });
      }

      // Independent, test-only oracle: one Range per non-whitespace character.
      const glyphRows: Array<{ top: number; bottom: number }> = [];
      const characterWalker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      for (let node = characterWalker.nextNode(); node; node = characterWalker.nextNode()) {
        for (let offset = 0; offset < node.textContent!.length; offset++) {
          if (/\s/.test(node.textContent![offset]!)) continue;
          const range = document.createRange();
          range.setStart(node, offset);
          range.setEnd(node, offset + 1);
          for (const rect of range.getClientRects()) {
            if (rect.width > 0 && rect.height > 0) glyphRows.push({ top: rect.top, bottom: rect.bottom });
          }
        }
      }
      glyphRows.sort((a, b) => a.top - b.top || a.bottom - b.bottom);
      const oracle: Array<{ top: number; bottom: number }> = [];
      for (const interval of glyphRows) {
        const previous = oracle.at(-1);
        if (previous && interval.top < previous.bottom - epsilon) previous.bottom = Math.max(previous.bottom, interval.bottom);
        else oracle.push({ ...interval });
      }

      return {
        totalRows: rows.length,
        hiddenRows: rows.filter((row) => row.bottom > cutoff + epsilon).length,
        oracleRows: oracle.length,
      };
    });

    expect(result.totalRows).toBe(lineCount);
    expect(result.oracleRows).toBe(lineCount);
    expect(result.hiddenRows).toBe(lineCount === 12 ? 0 : 1);
    expect(result.hiddenRows > 0).toBe(lineCount === 13);
    if (lineCount === 13) expect(2).not.toBe(result.hiddenRows);
  }
});

// Real-browser description editor lifecycle through the disclosure wrapper.
// The pure/mocked matrix for the disclosure primitive lives in
// description-disclosure.test.tsx; issue-detail.test.tsx covers the wiring
// against a mocked ContentEditor. This suite is the only layer that can prove
// pointer/keyboard behavior, annotation source, and upload-pending refusal
// against the real ProseMirror editor and real DOM geometry.
test.describe("Description editor lifecycle through folding", () => {
  let api: TestApiClient;
  let issueId: string;
  let issueTitle: string;
  let workspaceSlug: string;

  test.beforeEach(async ({ page }) => {
    api = await createTestApi();
    issueTitle = "E2E Disclosure Test " + Date.now();
    const issue = await api.createIssue(issueTitle, { description: LONG_DESCRIPTION });
    issueId = issue.id;
    workspaceSlug = await loginAsDefault(page);
  });

  test.afterEach(async () => {
    if (api) await api.cleanup();
  });

  test("first pointer gesture on the collapsed preview expands before editing capture", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);

    const editor = page.locator("[data-description-editor]");
    await expect(editor).toHaveAttribute("aria-hidden", "true");
    const showMore = page.getByRole("button", { name: /Show more/ });
    await expect(showMore).toBeVisible();

    // Click lands on the clipped, inert editor surface itself — not the Show
    // more button — to prove the wrapper's own pointer handler expands first.
    await editor.click({ position: { x: 10, y: 10 } });

    await expect(editor).not.toHaveAttribute("aria-hidden");
    await expect(editor).not.toHaveAttribute("inert");
    await expect(page.getByRole("button", { name: "Show less" })).toBeVisible();
  });

  test("keyboard Show more expands and Tab enters the real editor", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);

    const showMore = page.getByRole("button", { name: /Show more/ });
    await showMore.focus();
    await page.keyboard.press("Enter");

    await expect(page.getByRole("button", { name: "Show less" })).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(page.locator("[data-description-editor] .ProseMirror")).toBeFocused();
  });

  test("selecting revealed text after expansion attaches the annotation to the real editor", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);

    await page.getByRole("button", { name: /Show more/ }).click();
    const paragraph = page.locator("[data-description-editor] .ProseMirror p").last();
    await expect(paragraph).toBeVisible();

    // Select the visible text via a real text-node Range, then dispatch the
    // selectionchange the annotation capture listens for.
    await paragraph.evaluate((node) => {
      const range = document.createRange();
      range.selectNodeContents(node);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      document.dispatchEvent(new Event("selectionchange"));
    });
    await paragraph.dispatchEvent("mouseup");

    const addAnnotation = page.getByRole("button", { name: /Add|Comment/ });
    await expect(addAnnotation).toBeVisible();
  });

  test("hidden collapsed suffix never spawns an annotation bubble", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);

    // The accessible preview is the only text exposed while collapsed; a
    // selection Range inside it must never reach real annotation capture,
    // since it isn't the real editor and is marked data-find-ignore.
    const preview = page.locator("[data-description-accessible-preview]");
    await expect(preview).toBeAttached();
    await preview.evaluate((node) => {
      const range = document.createRange();
      range.selectNodeContents(node);
      window.getSelection()?.removeAllRanges();
      window.getSelection()?.addRange(range);
      document.dispatchEvent(new Event("selectionchange"));
    });
    await preview.dispatchEvent("mouseup");

    await expect(page.getByRole("button", { name: /Add|Comment/ })).not.toBeVisible();
  });

  test("paste-then-immediate-close persists the image markdown and its attachment bind", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);

    await page.getByRole("button", { name: /Show more/ }).click();
    const editor = page.locator("[data-description-editor] .ProseMirror");
    await editor.click();
    await editor.press("End");

    const fileChooserPromise = page.waitForEvent("filechooser");
    await page.getByLabel("Attach file").click();
    const fileChooser = await fileChooserPromise;
    await fileChooser.setFiles({
      name: "e2e-paste.png",
      mimeType: "image/png",
      buffer: Buffer.from(
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
        "base64",
      ),
    });

    // Immediate navigation away simulates paste-followed-by-quick-close: the
    // debounce (1500ms) has not fired, so only flushPendingOnUnmount can save
    // the image markdown and its attachment_ids bind (MUL-3254).
    await expect(page.locator("[data-description-editor] img, [data-description-editor] [data-attachment-id]")).toBeVisible({ timeout: 10000 });
    await page.goto(`/${workspaceSlug}/issues`, { waitUntil: "domcontentloaded" });

    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);
    await page.getByRole("button", { name: /Show more/ }).click();
    await expect(page.locator("[data-description-editor] img, [data-description-editor] [data-attachment-id]")).toBeVisible({ timeout: 10000 });
  });

  test("Show less is refused while an upload is pending", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);

    await page.getByRole("button", { name: /Show more/ }).click();
    const editor = page.locator("[data-description-editor] .ProseMirror");
    await editor.click();

    const fileChooserPromise = page.waitForEvent("filechooser");
    await page.getByLabel("Attach file").click();
    const fileChooser = await fileChooserPromise;
    // A large buffer keeps the upload in flight long enough to observe the
    // disabled Show less before it settles.
    await fileChooser.setFiles({
      name: "e2e-pending.png",
      mimeType: "image/png",
      buffer: Buffer.alloc(3 * 1024 * 1024, 1),
    });

    const showLess = page.getByRole("button", { name: "Show less" });
    await expect(showLess).toBeDisabled();
  });

  test("issue switch remounts the description with one mounted editor, no stale content", async ({ page }) => {
    const otherIssue = await api.createIssue("E2E Disclosure Second " + Date.now(), {
      description: "Second issue description",
    });

    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);
    await expect(page.locator("[data-description-editor] .ProseMirror")).toHaveCount(1);

    await page.goto(`/${workspaceSlug}/issues/${otherIssue.id}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, "Second issue description");
    await expect(page.locator("[data-description-editor] .ProseMirror")).toHaveCount(1);
    await expect(page.locator("text=Line 1 of the description.")).not.toBeVisible();
  });
});
