import type { Page, WebSocketRoute } from "@playwright/test";

/**
 * The mocked realtime socket of a scenario. A stimulus that never reaches the
 * page reads as "stable", so a send without a connected socket throws, and
 * `delivered` counts the frames the page actually received.
 */
export class MockSocket {
  private route: WebSocketRoute | null = null;
  private page: Page | null = null;

  /** Hand this to `context.routeWebSocket`. */
  attach = (route: WebSocketRoute) => {
    this.route = route;
    route.onMessage(() => {});
  };

  /** Count frames the page receives, by event type. Call before the page loads. */
  watch(page: Page) {
    this.page = page;
    return page.addInitScript(() => {
      const counts: Record<string, number> = {};
      (window as unknown as { __wsReceived: typeof counts }).__wsReceived = counts;
      // Wrap whatever class is installed, including the one Playwright injects
      // for a routed socket, whichever runs first.
      const wrap = (Native: typeof WebSocket): typeof WebSocket => class extends Native {
        constructor(...args: ConstructorParameters<typeof Native>) {
          super(...args);
          this.addEventListener("message", (event) => {
            try {
              const type = (JSON.parse(String(event.data)) as { type?: string }).type;
              if (type) counts[type] = (counts[type] ?? 0) + 1;
            } catch { /* not a JSON event frame */ }
          });
        }
      };
      let installed = wrap(window.WebSocket);
      Object.defineProperty(window, "WebSocket", {
        configurable: true,
        get: () => installed,
        set: (next: typeof WebSocket) => { installed = wrap(next); },
      });
    });
  }

  get connected() {
    return this.route !== null;
  }

  send(type: string, payload: unknown) {
    if (!this.route) throw new Error(`mock WebSocket is not connected; "${type}" would be dropped`);
    this.route.send(JSON.stringify({ type, payload, actor_id: "", actor_type: "system" }));
  }

  /** Frames of this type the page has received. */
  delivered(type: string) {
    return this.page!.evaluate(
      (t) => (window as unknown as { __wsReceived: Record<string, number> }).__wsReceived[t] ?? 0, type);
  }
}
