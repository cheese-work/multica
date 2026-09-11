import { expect, test } from "@playwright/test";
import { buildSurfaceFrameDocument } from "../packages/views/plugins/surface-document";

/**
 * Chromium coverage for the hosted plugin-surface boundary. The visible host
 * frame is trusted code; the routed page is the sandboxed plugin document.
 */

test.describe("plugin surface document (real Chromium, hosted content)", () => {
  test("a host-authored wrapper replacement preserves bridge lifecycle without hostile navigation", async ({ page }) => {
    let contentRequests = 0;
    await page.route("https://plugin-content.example.test/**", async (route) => {
      contentRequests += 1;
      await route.fulfill({
        contentType: "text/html",
        headers: { "Content-Security-Policy": "default-src 'none'; script-src 'unsafe-inline'" },
        body: `<!doctype html><script>
          const channel = new MessageChannel();
          parent.postMessage({
            type: "multica:plugin-bridge-connect",
            version: 2,
            challenge: "replacement-proof"
          }, "*", [channel.port1]);
        </script>`,
      });
    });

    const wrapper = buildSurfaceFrameDocument({
      url: "https://plugin-content.example.test/plugin-surfaces/opaque",
      bridgeToken: "replacement-proof",
    });

    await page.setContent(`<script>
      window.surfaceState = { bridges: 0, navigated: 0 };
      addEventListener("message", event => {
        if (event.data?.type === "multica:plugin-bridge-connect" && event.ports[0]) {
          window.surfaceState.bridges += 1;
          event.ports[0].close();
        }
        if (event.data?.type === "multica:plugin-surface-navigated") {
          window.surfaceState.navigated += 1;
        }
      });
    </script><iframe id="host" sandbox="allow-scripts allow-same-origin"></iframe>`);

    await page.locator("#host").evaluate((frame, srcdoc) => {
      (frame as HTMLIFrameElement).srcdoc = srcdoc as string;
    }, wrapper);
    await expect.poll(() => page.evaluate(() =>
      (window as unknown as { surfaceState: { bridges: number } }).surfaceState.bridges,
    )).toBe(1);

    await page.locator("#host").evaluate((frame, srcdoc) => {
      (frame as HTMLIFrameElement).srcdoc = srcdoc as string;
    }, wrapper);
    await expect.poll(() => page.evaluate(() =>
      (window as unknown as { surfaceState: { bridges: number } }).surfaceState.bridges,
    )).toBe(2);
    expect(contentRequests).toBe(2);
    expect(await page.evaluate(() =>
      (window as unknown as { surfaceState: { navigated: number } }).surfaceState.navigated,
    )).toBe(0);
  });

  test("reports a hosted plugin error when the host listener is ready before launch", async ({ page }) => {
    await page.route("https://plugin-content.example.test/**", async (route) => {
      await route.fulfill({
        contentType: "text/html",
        headers: { "Content-Security-Policy": "default-src 'none'; script-src 'unsafe-inline'" },
        body: `<!doctype html><script>
          parent.postMessage({ type: "multica:plugin-surface-error" }, "*");
        </script>`,
      });
    });
    const wrapper = buildSurfaceFrameDocument({
      url: "https://plugin-content.example.test/plugin-surfaces/failing",
      bridgeToken: "error-proof",
    });

    // The hosted document emits one error only. Register before srcdoc so the
    // visible error cannot be lost to the obsolete late-listener ordering.
    await page.setContent(`<script>
      window.surfaceErrors = 0;
      addEventListener("message", event => {
        if (event.data?.type === "multica:plugin-surface-error") {
          window.surfaceErrors += 1;
        }
      });
    </script><iframe id="host" sandbox="allow-scripts allow-same-origin"></iframe>`);
    await page.locator("#host").evaluate((frame, srcdoc) => {
      (frame as HTMLIFrameElement).srcdoc = srcdoc as string;
    }, wrapper);

    await expect.poll(() => page.evaluate(() =>
      (window as unknown as { surfaceErrors: number }).surfaceErrors,
    )).toBe(1);
  });
});
