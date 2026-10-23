import { expect, test, type Page } from "@playwright/test";
import { buildSurfaceFrameDocument } from "../packages/views/plugins/surface-document";

/**
 * Real Chromium coverage for behavior jsdom does not implement: the trusted
 * wrapper document loading a sandboxed plugin document, and what the host sees
 * when the plugin errors or the host rewrites srcdoc.
 *
 * The plugin document itself is served by the content origin and built on the
 * server, so these tests stand in for it with a routed page. The host arms its
 * listener before it assigns srcdoc (plugin-surface-frame.tsx), and so does the
 * page below; a message sent before a listener exists is not replayed.
 */

const CONTENT_ORIGIN = "https://plugin-content.example.test";

async function serveContent(page: Page, script: string) {
  await page.route(`${CONTENT_ORIGIN}/**`, (route) =>
    route.fulfill({
      contentType: "text/html",
      headers: { "Content-Security-Policy": "default-src 'none'; script-src 'unsafe-inline'" },
      body: `<!doctype html><script>${script}</script>`,
    }),
  );
}

async function mountHost(page: Page) {
  await page.setContent("<!doctype html><body></body>");
  await page.evaluate(() => {
    const state = { connected: 0, error: 0, navigated: 0, blocked: 0 };
    (window as unknown as { __surface: typeof state }).__surface = state;
    window.addEventListener("message", (event) => {
      const type = (event.data as { type?: string } | null)?.type;
      if (type === "multica:plugin-bridge-connect" && event.ports[0]) state.connected++;
      if (type === "multica:plugin-surface-error") state.error++;
      if (type === "multica:plugin-surface-navigated") state.navigated++;
      if (type === "multica:plugin-surface-navigation-blocked") state.blocked++;
    });
    const frame = document.createElement("iframe");
    frame.id = "surface";
    frame.sandbox.add("allow-scripts");
    document.body.appendChild(frame);
  });
}

const surface = (page: Page) =>
  page.evaluate(
    () => (window as unknown as { __surface: Record<string, number> }).__surface,
  );

const load = (page: Page, bridgeToken: string) =>
  page.evaluate((srcdoc) => {
    document.querySelector<HTMLIFrameElement>("#surface")!.srcdoc = srcdoc;
  }, buildSurfaceFrameDocument({ url: `${CONTENT_ORIGIN}/plugin-surfaces/opaque`, bridgeToken }));

test.describe("plugin surface document (real Chromium, sandboxed srcdoc)", () => {
  test("a host-authored srcdoc reload is not reported as hostile navigation", async ({ page }) => {
    await serveContent(
      page,
      `const channel = new MessageChannel();
       parent.postMessage({ type: "multica:plugin-bridge-connect", version: 2, challenge: "proof" }, "*", [channel.port1]);`,
    );
    await mountHost(page);

    await load(page, "proof");
    await expect.poll(async () => (await surface(page)).connected).toBe(1);

    // The host rewrites srcdoc, for instance to apply a new theme.
    await load(page, "proof");
    await expect.poll(async () => (await surface(page)).connected).toBe(2);

    const seen = await surface(page);
    expect(seen.navigated).toBe(0);
    expect(seen.blocked).toBe(0);
    expect(seen.error).toBe(0);
  });

  test("reports a synchronous plugin error to a host that is already listening", async ({ page }) => {
    await serveContent(page, `parent.postMessage({ type: "multica:plugin-surface-error" }, "*");`);
    await mountHost(page);

    await load(page, "proof");

    await expect.poll(async () => (await surface(page)).error).toBe(1);
    expect((await surface(page)).connected).toBe(0);
  });
});
