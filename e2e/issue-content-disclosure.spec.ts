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

  // Warm up the `/issues/[id]` route's dev-server compile once before this
  // suite. `/issues` (hit inside loginAsDefault) is a different route and
  // compiles separately, so its warm cache doesn't cover this one — the
  // first real navigation below was seen failing with ERR_ABORTED /
  // waitForPageText timeouts consistent with Next.js's first-request
  // compile trap in dev mode.
  test.beforeAll(async ({ browser }) => {
    const warmupApi = await createTestApi();
    try {
      const warmupIssue = await warmupApi.createIssue("E2E Warmup " + Date.now());
      const page = await browser.newPage();
      try {
        const slug = await loginAsDefault(page);
        await page.goto(`/${slug}/issues/${warmupIssue.id}`, { waitUntil: "domcontentloaded" });
        await waitForPageText(page, "E2E Warmup");
      } finally {
        await page.close();
      }
    } finally {
      await warmupApi.cleanup();
    }
  });

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

    // The editor itself is `inert` while collapsed, so real Chromium never
    // hit-tests it — the enclosing, non-inert section is what actually
    // receives the pointer event at that point (and owns the capture
    // handler). Click the section, not the inert div: Playwright's
    // actionability check refuses to click an element real hit-testing
    // would never deliver the event to.
    const section = page.locator("[data-description-disclosure]");
    await section.click({ position: { x: 10, y: 10 } });

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
    const editor = page.locator("[data-description-editor]");
    await expect(editor).not.toHaveAttribute("inert");
    const paragraph = editor.locator(".ProseMirror p").last();
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
    // The annotation capture handler is wired to `onPointerUp` (see
    // use-comment-annotations.tsx's captureProps), which listens for native
    // `pointerup` events specifically — a plain `mouseup` dispatch never
    // reaches it, since the two are distinct DOM event types.
    await paragraph.dispatchEvent("pointerup");

    const addAnnotation = page.getByRole("button", { name: "Add annotation" });
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

    await expect(page.getByRole("button", { name: "Add annotation" })).not.toBeVisible();
  });

  test("paste-then-immediate-close persists the image markdown across reload", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);

    await page.getByRole("button", { name: /Show more/ }).click();
    const editor = page.locator("[data-description-editor] .ProseMirror");
    await editor.click();
    await editor.press("End");

    // A real clipboard `paste` with an image File on the DataTransfer — the
    // same shape the file-upload ProseMirror plugin's `handlePaste` reads
    // (`event.clipboardData.files`) — not the Attach-file chooser, so this
    // actually exercises the paste path rather than the upload-button path.
    const pngBase64 =
      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=";
    await editor.evaluate((node, base64) => {
      const binary = atob(base64);
      const bytes = new Uint8Array(binary.length);
      for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
      const file = new File([bytes], "e2e-paste.png", { type: "image/png" });
      const dataTransfer = new DataTransfer();
      dataTransfer.items.add(file);
      const pasteEvent = new ClipboardEvent("paste", {
        bubbles: true,
        cancelable: true,
        clipboardData: dataTransfer,
      });
      node.dispatchEvent(pasteEvent);
    }, pngBase64);

    // Wait for the upload to SETTLE — the NodeView drops its `image-uploading`
    // class and a real (non-blob) src lands — before navigating. The image
    // NodeView (`attachment.tsx`) never renders `data-uploading` on the `img`
    // itself; that attribute exists only on the ProseMirror node's HTML
    // serialization, not this React NodeView's DOM, so `img:not([data-uploading])`
    // matches even the in-flight blob preview and asserts nothing about upload
    // state. An in-flight placeholder also serializes to no markdown at all
    // (`extensions/index.ts` `renderMarkdown`: `uploading === true` -> `""`),
    // so flushing before settlement writes back content unchanged by the
    // paste; it does not prove persistence. Settling starts a FRESH 1500ms
    // debounce for the now-real image markdown, which is the actual flush
    // boundary this test needs to race.
    const insertedImage = page.locator("[data-description-editor] img.image-content:not(.image-uploading)");
    await expect(insertedImage).toBeVisible({ timeout: 10000 });
    await expect(insertedImage).not.toHaveAttribute("src", /^blob:/);

    // Navigate away immediately after settlement — before the freshly-started
    // 1500ms debounce can fire — so only `flushPendingOnUnmount` can save the
    // image markdown and its attachment_ids bind (MUL-3254).
    await page.goto(`/${workspaceSlug}/issues`, { waitUntil: "domcontentloaded" });

    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);
    await page.getByRole("button", { name: /Show more/ }).click();
    // A bare `img` locator is satisfied by an unsettled blob preview too, so
    // this would pass even if the flush lost the bind and only a transient
    // client-side node survived. After a full navigate-away-and-back the blob
    // URL is gone from memory regardless; requiring the SAME durable,
    // non-blob src that settlement produced is what actually proves this
    // reload re-fetched persisted markdown rather than showing left-over
    // client state.
    const reopenedImage = page.locator("[data-description-editor] img.image-content:not(.image-uploading)");
    await expect(reopenedImage).toBeVisible({ timeout: 10000 });
    await expect(reopenedImage).not.toHaveAttribute("src", /^blob:/);
  });

  test("Show less is refused while an upload is pending, independent of editor focus", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);

    await page.getByRole("button", { name: /Show more/ }).click();
    const editor = page.locator("[data-description-editor] .ProseMirror");
    await editor.click();

    // Gate the upload deterministically instead of racing a large buffer
    // against the assertion window: against a local API a multi-MB body
    // settles well inside any fixed wait, so the only reliable way to
    // observe "upload pending" is to hold the response until the test says
    // so. Intercept the exact endpoint the description editor's upload path
    // calls (`api.uploadFile` -> `POST /api/upload-file`, client.ts:3187)
    // and park it on a promise this test resolves after asserting.
    let releaseUpload: () => void = () => {};
    const uploadGate = new Promise<void>((resolve) => {
      releaseUpload = resolve;
    });
    await page.route("**/api/upload-file", async (route) => {
      await uploadGate;
      await route.continue();
    });

    // "Attach file" also exists on the page's own comment composer — scope to
    // the wrapper containing the description's disclosure section so this
    // clicks the description's attach button, not the comment composer's.
    const descriptionWrapper = page.locator("div:has(> [data-description-disclosure])").first();
    const fileChooserPromise = page.waitForEvent("filechooser");
    await descriptionWrapper.getByLabel("Attach file").click();
    const fileChooser = await fileChooserPromise;
    // A real (tiny) PNG — the point of the gate above is to hold the
    // in-flight request open, not to rely on payload size, and an invalid
    // PNG body risks a content-type rejection producing the same "pending"
    // symptom as a genuine in-flight upload.
    const pngBase64 =
      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=";
    await fileChooser.setFiles({
      name: "e2e-pending.png",
      mimeType: "image/png",
      buffer: Buffer.from(pngBase64, "base64"),
    });

    // Move focus OUT of the description wrapper entirely, not just off the
    // editor. `onFocusCapture`/`onBlurCapture` live on the wrapper `<div>`
    // that contains both the ProseMirror editor AND the Attach-file button
    // (`issue-detail.tsx`); `onBlurCapture` only clears `descriptionFocused`
    // when `relatedTarget` is outside that wrapper's `contains()` check.
    // Blurring the editor locator alone doesn't prove that: the file chooser
    // interaction above can leave focus on the Attach-file button instead,
    // which sits in the SAME wrapper as the editor, so blurring the
    // (already-unfocused) editor element is a no-op and the wrapper's
    // captured focus state never changes. Explicitly blur whatever currently
    // has focus and move it to `document.body` — unambiguously outside the
    // wrapper — then confirm that landed before asserting on Show less.
    await page.evaluate(() => {
      (document.activeElement as HTMLElement | null)?.blur();
      document.body.focus();
    });
    await expect.poll(() => page.evaluate(() => document.activeElement === document.body)).toBe(true);

    const showLess = page.getByRole("button", { name: "Show less" });
    await expect(showLess).toBeDisabled();

    // Release the gated response so the upload settles and doesn't leak a
    // pending request into the next test.
    releaseUpload();
    await expect(showLess).toBeEnabled({ timeout: 10000 });
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

// Durable thread-fold state (CHE-435): the length-disclosure store must
// survive a real CommentCard/Virtuoso row unmount+remount, not just a store
// unit test — 01-04-PLAN.md's Task 2 acceptance criterion is explicit that
// this must be OBSERVED via an actual browser scroll-away-and-back, not
// inferred from scroll distance.
test.describe("Durable thread fold state through row unmount/remount", () => {
  let api: TestApiClient;
  let issueId: string;
  let issueTitle: string;
  let workspaceSlug: string;

  test.beforeEach(async ({ page }) => {
    api = await createTestApi();
    issueTitle = "E2E Thread Fold Test " + Date.now();
    const issue = await api.createIssue(issueTitle, { description: "Thread fold fixture" });
    issueId = issue.id;
    const root = await api.createComment(issueId, "Root comment for thread fold test");
    // Five replies so the thread's compact window truncates ("latest three")
    // and Show more/Show less actually has something to prove.
    for (let i = 1; i <= 5; i++) {
      await api.createComment(issueId, `Reply number ${i}`, root.id);
    }
    // Padding so the thread scrolls far enough out of the viewport to
    // actually unmount its Virtuoso row, not just scroll within view.
    for (let i = 1; i <= 40; i++) {
      await api.createComment(issueId, `Padding comment ${i}`);
    }
    workspaceSlug = await loginAsDefault(page);
  });

  test.afterEach(async () => {
    if (api) await api.cleanup();
  });

  test("Show more expansion survives scrolling the thread's row out of view and back", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);
    await waitForPageText(page, "Root comment for thread fold test");

    const showMore = page.getByRole("button", { name: /Show \d+ more repl/ });
    await expect(showMore).toBeVisible();
    await showMore.click();
    await waitForPageText(page, "Reply number 1");
    const showLess = page.getByRole("button", { name: "Show less" });
    await expect(showLess).toBeVisible();

    // Scroll the timeline far past the thread so its row genuinely unmounts
    // from the virtualized list (not just off-screen within a mounted DOM).
    // A single `scrollTop = scrollHeight` assignment is not enough: Virtuoso
    // starts with an estimated `scrollHeight` for rows it hasn't measured
    // yet, so that first assignment lands short of the true bottom, and as
    // real row heights arrive `scrollHeight` keeps growing out from under a
    // scrollTop that was only set once — landing the viewport in a dead zone
    // past "Reply number 1" but short of "Padding comment 40". Re-apply the
    // assignment until scrollHeight (and therefore scrollTop) stops moving.
    const scrollContainer = page.locator("[data-issue-timeline-scroll]").first();
    const hasScrollContainer = await scrollContainer.count() > 0;
    if (hasScrollContainer) {
      let previousHeight = -1;
      await expect
        .poll(
          async () => {
            const height = await scrollContainer.evaluate((el) => {
              el.scrollTop = el.scrollHeight;
              return el.scrollHeight;
            });
            const stable = height === previousHeight;
            previousHeight = height;
            return stable;
          },
          { timeout: 10000 },
        )
        .toBe(true);
    } else {
      await page.mouse.wheel(0, 20000);
    }
    await expect(page.getByText("Padding comment 40")).toBeVisible({ timeout: 10000 });
    // Confirm the original thread's content actually left the DOM (proof of
    // unmount, not merely scrolled out of the viewport).
    await expect(page.getByText("Reply number 1")).not.toBeAttached();

    // Scroll back up to remount the thread's row.
    if (hasScrollContainer) {
      await scrollContainer.evaluate((el) => { el.scrollTop = 0; });
    } else {
      await page.mouse.wheel(0, -20000);
    }
    await waitForPageText(page, "Root comment for thread fold test");

    // The durable length-disclosure store (not row-local useState) must have
    // kept this thread expanded across the unmount — Reply number 1 (a
    // compact-window-hidden reply pre-expansion) is visible again without
    // clicking Show more a second time.
    await expect(page.getByText("Reply number 1")).toBeVisible({ timeout: 10000 });
    await expect(page.getByRole("button", { name: "Show less" })).toBeVisible();
  });

  test("fold-all and unfold-all commands drive the length-disclosure store together with manual collapse and resolved-expand", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);
    await waitForPageText(page, "Root comment for thread fold test");

    await page.getByRole("button", { name: /Show \d+ more repl/ }).click();
    await waitForPageText(page, "Reply number 1");

    // Open the command palette and run Fold All Comments. The command's
    // handler resolves `ensureQueryData(...).then(...)` after the palette
    // has already closed (`setOpen(false)` fires synchronously on click,
    // .catch(() => {}) swallows rejections silently), so the palette
    // dismissing is not proof the fold effect landed — only the DOM
    // reflecting the fold is.
    await page.keyboard.press("ControlOrMeta+K");
    const commandPalette = page.getByPlaceholder("Type a command or search...");
    await expect(commandPalette).toBeVisible();
    await commandPalette.fill("fold all");
    await page.getByText("Fold All Comments", { exact: true }).click();
    await expect(commandPalette).not.toBeVisible();

    // The whole thread collapses to its manual-collapse summary — the
    // length-expanded reply is no longer visible because the manual collapse
    // gate (higher priority in the 01-DESIGN "Effective order") now applies.
    await expect(page.getByText("Reply number 1")).not.toBeVisible({ timeout: 10000 });

    // Unfold All Comments restores full disclosure, including the
    // length-disclosure store's "all replies" state for this thread.
    await page.keyboard.press("ControlOrMeta+K");
    await expect(commandPalette).toBeVisible();
    await commandPalette.fill("unfold all");
    await page.getByText("Unfold All Comments", { exact: true }).click();
    await expect(commandPalette).not.toBeVisible();

    await expect(page.getByText("Reply number 1")).toBeVisible({ timeout: 10000 });
  });
});
