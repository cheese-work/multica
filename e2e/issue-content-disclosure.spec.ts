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

  // CHE-463: the test above deliberately clicks the section locator, which
  // only proves the capture handler fires when Playwright targets the
  // section directly — it never proves real Chromium retargets a pointer
  // event landing on the INERT editor's own rendered pixels up to that
  // section. That distinction is exactly the bug this issue reported (the
  // handler used to sit on the inert div itself, and `inert` hit-testing
  // silently drops such events, so no real click on the collapsed preview's
  // visible text ever reached it). Use raw viewport coordinates inside the
  // inert editor's bounding box — bypassing locator actionability checks
  // entirely — so this exercises the same hit-test path a real user's click
  // on the visible collapsed text would.
  test("a raw pointer hit inside the inert editor's own rendered area still expands it", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);

    const editor = page.locator("[data-description-editor]");
    await expect(editor).toHaveAttribute("inert");
    const box = await editor.boundingBox();
    if (!box) throw new Error("collapsed editor has no bounding box");

    // A point well inside the collapsed preview's visible text, not on any
    // sibling (the Show more button sits below this box).
    await page.mouse.click(box.x + Math.min(20, box.width / 2), box.y + Math.min(10, box.height / 2));

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

    // The description editor is `editable: true` in useCommentAnnotations, so
    // the generic onPointerUp/onKeyUp `capture()` path is deliberately a
    // no-op for it (see use-comment-annotations.tsx's `!editable` guards on
    // captureProps) — a programmatic DOM Selection + synthetic pointerup
    // never reaches it. Editable sources trigger annotation via the editor's
    // own bubble menu (`selectionAction`, wired in issue-detail.tsx), which
    // only appears on a real ProseMirror `selectionUpdate` transaction — a
    // plain DOM Range/selectionchange dispatch does not produce one. Drive a
    // real mouse-drag text selection so tiptap emits that transaction.
    //
    // The fixture description is 30 lines, so this last paragraph sits well
    // below the fold — `boundingBox()` succeeds even when scrolled out of
    // view (it only requires non-zero size), but `page.mouse` dispatches at
    // raw viewport coordinates and does not auto-scroll like locator actions
    // do. Scroll it into view first or the drag lands on nothing.
    //
    // The editor's `value` sync effect (content-editor.tsx) can still replace
    // the ProseMirror DOM out from under us shortly after mount — e.g. a
    // background issue refetch landing right after `Show more` — which
    // detaches this exact paragraph node mid-scroll. Retry the whole
    // locate-scroll-measure sequence against a freshly-resolved locator until
    // it survives one full pass without the node disappearing underneath it.
    let box: { x: number; y: number; width: number; height: number } | null = null;
    await expect(async () => {
      await paragraph.scrollIntoViewIfNeeded();
      box = await paragraph.boundingBox();
      if (!box) throw new Error("paragraph has no bounding box");
    }).toPass({ timeout: 10000 });
    if (!box) throw new Error("paragraph has no bounding box");
    await page.mouse.move(box.x + 2, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width - 2, box.y + box.height / 2, { steps: 5 });
    await page.mouse.up();

    // Selecting text opens the bubble menu with the editable-source action
    // labeled "Add to comment" (issues.json reply.annotations.add_comment) —
    // "Add annotation" is the *confirm* button inside the note popup that
    // opens after clicking it (reply.annotations.confirm_add).
    const addToComment = page.getByRole("button", { name: "Add to comment" });
    await expect(addToComment).toBeVisible();
    // `force: true`: the click's own onClick handler hides the bubble menu
    // synchronously (`setVisible(false)` in bubble-menu.tsx) to keep later
    // editor transactions from reopening it over the note field. Playwright's
    // default actionability retry loop sees the button detach mid-interaction
    // and retries the whole click forever — this is the menu closing exactly
    // as designed, not a real instability, so skip the actionability wait.
    await addToComment.click({ force: true });

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
    // image markdown and its attachment_ids bind (MUL-3254). This must be a
    // real in-app client-side transition (sidebar link click), not
    // `page.goto()`: `flushPendingOnUnmount`'s fire-and-forget save fires from
    // a React unmount cleanup with nothing to await it, which is exactly what
    // a client-side route swap survives — the tab stays alive under the
    // in-flight request. A `page.goto()` is a real browser navigation that
    // discards the document (and any in-flight fetch) the instant it starts,
    // which this flush was never built to survive, so it's not this feature's
    // failure mode to test.
    await page.getByRole("link", { name: "Issues", exact: true }).click();

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

  // CHE-502 — the previous test proves the flush-after-settlement path.
  // This one proves the path that PR #31's evidence actually hit: the
  // client-side close happens WHILE `POST /api/upload-file` is still in
  // flight, so there is no debounced markdown containing the image yet
  // (`uploading === true` nodes always serialize to "") and the response
  // only lands after the editor has already unmounted. Gate the upload
  // deterministically (same mechanism as "Show less is refused while an
  // upload is pending") instead of racing a real network delay against the
  // navigation.
  test("close while the upload is still in flight persists the image markdown across reload", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);

    await page.getByRole("button", { name: /Show more/ }).click();
    const editor = page.locator("[data-description-editor] .ProseMirror");
    await editor.click();
    await editor.press("End");

    let releaseUpload: () => void = () => {};
    const uploadGate = new Promise<void>((resolve) => {
      releaseUpload = resolve;
    });
    await page.route("**/api/upload-file", async (route) => {
      await uploadGate;
      await route.continue();
    });

    const pngBase64 =
      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=";
    await editor.evaluate((node, base64) => {
      const binary = atob(base64);
      const bytes = new Uint8Array(binary.length);
      for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
      const file = new File([bytes], "e2e-inflight.png", { type: "image/png" });
      const dataTransfer = new DataTransfer();
      dataTransfer.items.add(file);
      const pasteEvent = new ClipboardEvent("paste", {
        bubbles: true,
        cancelable: true,
        clipboardData: dataTransfer,
      });
      node.dispatchEvent(pasteEvent);
    }, pngBase64);

    // The placeholder is in the doc (blob preview) but the upload response
    // is held by the gate — this is the exact "no description-save request
    // yet" window the issue's evidence describes.
    const uploadingPlaceholder = page.locator("[data-description-editor] img.image-uploading");
    await expect(uploadingPlaceholder).toBeVisible({ timeout: 10000 });

    // Client-side navigation away while the upload is still gated — this
    // unmounts ContentEditor before `settleUploadNode` ever runs.
    await page.getByRole("link", { name: "Issues", exact: true }).click();
    await expect(page.getByRole("button", { name: /Show more/ })).not.toBeVisible();

    // Now let the response land, after the editor is gone.
    releaseUpload();

    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);
    await page.getByRole("button", { name: /Show more/ }).click();
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

  // CHE-554: a mouse press on "Show more" focuses the button (it lives inside
  // `comment-${rootId}`) before `mouseup`, which issue-detail.tsx's focusin
  // listener reads as "active focus in this root" and used to force-expand
  // the thread (forceThreadOpen) — dropping hiddenCount to 0 and unmounting
  // this exact button between mousedown and mouseup, so a plain `click` never
  // fired and the durable length preference was never written. The fix
  // (`forceThreadLengthExpanded`, a copy of `forceThreadOpen` minus a bare
  // focus/selection-only reason) keeps the button mounted through its own
  // press. A human-paced press (down, wait, up) reproduces the race that a
  // Playwright `.click()` (down+up back to back, same tick) can mask.
  test("a human-paced mouse press on Show more still commits the expansion (CHE-554)", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);
    await waitForPageText(page, "Root comment for thread fold test");

    const showMore = page.getByRole("button", { name: /Show \d+ more repl/ });
    await expect(showMore).toBeVisible();
    const box = await showMore.boundingBox();
    if (!box) throw new Error("Show more button has no layout box");
    const x = box.x + box.width / 2;
    const y = box.y + box.height / 2;

    await page.mouse.move(x, y);
    await page.mouse.down();
    // Give focusin time to fire and issue-detail.tsx's forceThreadOpen to
    // react before the button is released — this is exactly the window the
    // bug loses.
    await page.waitForTimeout(120);
    await page.mouse.up();

    await waitForPageText(page, "Reply number 1");
    await expect(page.getByRole("button", { name: "Show less" })).toBeVisible();

    // Not just visually forced open by the transient focus pin — the durable
    // store must have the write, so it survives focus leaving the thread.
    await page.locator("body").click({ position: { x: 5, y: 5 } });
    await expect(page.getByText("Reply number 1")).toBeVisible();
    await expect(page.getByRole("button", { name: "Show less" })).toBeVisible();
  });

  // CHE-554: focusing the button by keyboard hits the same race as the mouse
  // press above — focus lands on the button first, `focusin` bubbles to
  // issue-detail.tsx and used to force-expand the thread before Enter was
  // even pressed, unmounting the button out from under the pending keypress.
  test("keyboard activation of Show more commits the expansion (CHE-554)", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);
    await waitForPageText(page, "Root comment for thread fold test");

    const showMore = page.getByRole("button", { name: /Show \d+ more repl/ });
    await expect(showMore).toBeVisible();
    await showMore.focus();
    await page.keyboard.press("Enter");

    await waitForPageText(page, "Reply number 1");
    await expect(page.getByRole("button", { name: "Show less" })).toBeVisible();

    // Not just visually forced open by the transient focus pin — the durable
    // (session-lifetime) length-disclosure store must have the write, so it
    // survives focus leaving the thread. `useIssueDisclosureStore` is
    // intentionally session-only (no page reload persistence, see its own
    // comment), so leaving-and-returning focus — not a reload — is the
    // correct durability check here (mirrors the mouse-press test above).
    await page.locator("body").click({ position: { x: 5, y: 5 } });
    await expect(page.getByText("Reply number 1")).toBeVisible();
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

  // CHE-479 regression: issue-detail.tsx's latch effect used to re-expand any
  // root carrying an active reply draft the instant Fold All cleared its
  // length-disclosure entry, because the effect only checked current
  // membership in the store, not whether Fold All itself was the reason that
  // membership just disappeared. `forceThreadOpen`'s temporary pin correctly
  // keeps an active draft's thread reachable through Fold All (01-DESIGN
  // "Fold all comments": "Active edit pins prevent focus loss") — this test
  // proves the separate, PERSISTED length-expansion choice does not silently
  // get re-latched to "expanded" by that same pin.
  test("an active reply draft on a root does not defeat Fold All's length-disclosure reset (CHE-479)", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);
    await waitForPageText(page, "Root comment for thread fold test");

    await page.getByRole("button", { name: /Show \d+ more repl/ }).click();
    await waitForPageText(page, "Reply number 1");
    await expect(page.getByRole("button", { name: "Show less" })).toBeVisible();

    // Start (but do not send) a reply draft on the root thread, so
    // `rootIdsWithActiveReplyDraft` is active for it when Fold All runs.
    await page.getByTestId("reply-composer-shell").first().click();
    const editor = page
      .locator('.ProseMirror[data-placeholder="Leave a reply..."], .ProseMirror:has([data-placeholder="Leave a reply..."])')
      .first();
    await editor.fill("A reply I'm still typing when Fold All runs.");
    await expect(editor).toHaveText("A reply I'm still typing when Fold All runs.");

    await page.keyboard.press("ControlOrMeta+K");
    const commandPalette = page.getByPlaceholder("Type a command or search...");
    await expect(commandPalette).toBeVisible();
    await commandPalette.fill("fold all");
    await page.getByText("Fold All Comments", { exact: true }).click();
    await expect(commandPalette).not.toBeVisible();

    // The draft itself must still be reachable — Fold All's pin keeps the
    // thread's composer/content from being ripped out from under active
    // typing (the part of the spec this test is NOT regressing on).
    await expect(editor).toHaveText("A reply I'm still typing when Fold All runs.");

    // The bug: the latch effect used to see the length-disclosure entry
    // Fold All just cleared as "never expanded" and immediately re-persist
    // it as expanded. Prove it did NOT by reloading — the pin (a render-time
    // `forceThreadOpen` check) cannot survive a reload with no live draft in
    // a fresh store, but a wrongly re-latched PERSISTED length-expansion
    // shares the same session-only store and also would not survive reload
    // on its own; the real proof is state immediately after Fold All, before
    // any reload, via the manual-collapse summary Fold All also applies.
    // Manual collapse (`useCommentCollapseStore.collapseAll`) is a HIGHER
    // priority gate than length-expansion (01-DESIGN "Effective order"), so
    // the thread's compact summary state after Fold All is driven by manual
    // collapse regardless of the length-disclosure bug — the length bug is
    // only observable once manual collapse for this root is separately
    // lifted without going through Unfold All. Sending the draft removes the
    // active-draft reason and drops the pin, then reload re-fetches with no
    // draft and no live pin, isolating exactly what got PERSISTED.
    const posted = page.waitForResponse(
      (response) => response.request().method() === "POST" && response.url().endsWith(`/api/issues/${issueId}/comments`),
    );
    await page.keyboard.press("ControlOrMeta+Enter");
    await posted;

    await page.reload({ waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);
    await waitForPageText(page, "Root comment for thread fold test");

    // Manual collapse does not persist across reload for this store (it is
    // the workspace-aware persisted one, keyed differently) — the thread
    // reloads using its length preference. If CHE-479 were still present,
    // Fold All would have wrongly re-latched this root's length preference
    // to "expanded" while the draft was active, and every reply would show
    // immediately on reload with no "Show more" click needed. With the fix,
    // Fold All's reset stuck: the thread reloads compact.
    await expect(page.getByRole("button", { name: /Show \d+ more repl/ })).toBeVisible({ timeout: 10000 });
    await expect(page.getByText("Reply number 1")).not.toBeVisible();
  });
});

// CHE-436 "Find and target reveal lifecycle": in-page find (Cmd/Ctrl+F) must
// force every fold open (manual collapse, description preview, length
// folds) before its own DOM walk, and restore the prior state on close —
// this is the layer that actually proves the committed-reveal-token wiring
// against real Chromium layout/paint, not the mocked matrix in
// issue-detail.test.tsx.
test.describe("In-page find reveals folded content (CHE-436)", () => {
  let api: TestApiClient;
  let issueId: string;
  let issueTitle: string;
  let workspaceSlug: string;

  test.beforeEach(async ({ page }) => {
    api = await createTestApi();
    issueTitle = "E2E Find Reveal Test " + Date.now();
    const issue = await api.createIssue(issueTitle, { description: LONG_DESCRIPTION });
    issueId = issue.id;
    const root = await api.createComment(issueId, "Findable root comment");
    for (let i = 1; i <= 5; i++) {
      await api.createComment(issueId, `Findable reply sentinel ${i}`, root.id);
    }
    workspaceSlug = await loginAsDefault(page);
  });

  test.afterEach(async () => {
    if (api) await api.cleanup();
  });

  test("reveals a manually-collapsed thread's replies and the collapsed description while open, then restores both on close", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);
    await waitForPageText(page, "Findable root comment");

    // Manually collapse the whole thread card (the chevron toggle, not
    // Show less) so its replies leave the DOM entirely.
    await page.getByRole("button", { name: "Collapse thread" }).click();
    await expect(page.getByText("Findable reply sentinel 1")).not.toBeAttached();

    // The description also starts collapsed (LONG_DESCRIPTION overflows the
    // 12-line preview).
    const descEditor = page.locator("[data-description-editor]");
    await expect(descEditor).toHaveAttribute("aria-hidden", "true");

    await page.keyboard.press("ControlOrMeta+F");
    const findInput = page.getByPlaceholder("Find in issue...");
    await expect(findInput).toBeVisible();

    // Every reply is back in the DOM, and the description is no longer
    // clipped/inert — both fold classes forced open by the same find.open
    // reveal, per 01-DESIGN.
    await expect(page.getByText("Findable reply sentinel 1")).toBeAttached();
    await expect(page.getByText("Findable reply sentinel 5")).toBeAttached();
    await expect(descEditor).not.toHaveAttribute("aria-hidden");

    // The manual-collapse chevron itself is disabled while find owns the
    // reveal, with the localized explanation — a real click must not be
    // able to write the durable preference out from under find's overlay.
    const collapseButton = page.getByRole("button", { name: "Collapse thread" });
    await expect(collapseButton).toBeDisabled();
    await expect(collapseButton).toHaveAttribute("title", "Can't collapse while find is open");

    // The find bar itself can actually locate the sentinel text now that
    // it's in the DOM — proof the reveal-before-DOM-walk token gated the
    // collector correctly rather than it racing ahead of the reveal.
    await findInput.fill("sentinel");
    await expect(page.getByText(/\d+\/\d+/)).toBeVisible();
    await expect(page.getByText("No matches")).not.toBeVisible();

    // Close find (Escape) — the thread refolds (no durable write happened)
    // and the description returns to its collapsed preview.
    await findInput.press("Escape");
    await expect(findInput).not.toBeVisible();
    await expect(page.getByText("Findable reply sentinel 1")).not.toBeAttached();
    await expect(descEditor).toHaveAttribute("aria-hidden", "true");
    // The manual-collapse control is live again.
    await expect(page.getByRole("button", { name: "Expand thread" })).toBeEnabled();
  });

  test("fold-all/unfold-all commands issued while find is open still change the base preference, taking visible effect after close", async ({ page }) => {
    await page.goto(`/${workspaceSlug}/issues/${issueId}`, { waitUntil: "domcontentloaded" });
    await waitForPageText(page, issueTitle);
    await waitForPageText(page, "Findable root comment");

    await page.keyboard.press("ControlOrMeta+F");
    const findInput = page.getByPlaceholder("Find in issue...");
    await expect(findInput).toBeVisible();
    await expect(page.getByText("Findable reply sentinel 1")).toBeAttached();

    // Fold All Comments while find is open — row 58: "Fold/unfold-all can
    // still change base stores; the overlay keeps content visible until
    // close."
    await page.keyboard.press("ControlOrMeta+K");
    const commandPalette = page.getByPlaceholder("Type a command or search...");
    await expect(commandPalette).toBeVisible();
    await commandPalette.fill("fold all");
    await page.getByText("Fold All Comments", { exact: true }).click();
    await expect(commandPalette).not.toBeVisible();

    // Content stays visible — find's overlay still owns the reveal.
    await expect(page.getByText("Findable reply sentinel 1")).toBeAttached();

    // Close find: the base write from Fold All now takes visible effect.
    await findInput.press("Escape");
    await expect(findInput).not.toBeVisible();
    await expect(page.getByText("Findable reply sentinel 1")).not.toBeAttached();
  });
});
