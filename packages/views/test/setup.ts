import "@testing-library/jest-dom/vitest";
import { vi } from "vitest";

// Production loads the chat composer's editor on demand; suites render it
// synchronously instead of awaiting the lazy chunk or waiting for intent
// (CHE-3018). deferred-content-editor.test.tsx covers the real wrapper.
vi.mock("../chat/components/lazy-content-editor", async () => ({
  LazyContentEditor: (await import("../editor/content-editor")).ContentEditor,
}));
vi.mock("../chat/components/deferred-content-editor", async () => ({
  DeferredContentEditor: (await import("../editor/content-editor")).ContentEditor,
}));
import "@multica/vitest-setup/web-storage";

// Everything below patches gaps in jsdom. Pure-logic suites opt out of jsdom
// with `// @vitest-environment node` and share this file, so there is no DOM to
// patch there — bail out rather than guard each stub.
if (typeof window !== "undefined") {
  // jsdom doesn't provide matchMedia; useIsMobile() relies on it.
  if (typeof window.matchMedia !== "function") {
    window.matchMedia = (query: string) =>
      ({
        matches: false,
        media: query,
        onchange: null,
        addListener: () => {},
        removeListener: () => {},
        addEventListener: () => {},
        removeEventListener: () => {},
        dispatchEvent: () => false,
      }) as MediaQueryList;
  }

  // jsdom doesn't provide ResizeObserver; stub it so components that rely on it
  // (e.g. input-otp) can render in tests.
  if (typeof globalThis.ResizeObserver === "undefined") {
    globalThis.ResizeObserver = class ResizeObserver {
      observe() {}
      unobserve() {}
      disconnect() {}
    } as unknown as typeof ResizeObserver;
  }

  // jsdom doesn't implement elementFromPoint; input-otp uses it internally.
  if (typeof document.elementFromPoint !== "function") {
    document.elementFromPoint = () => null;
  }

  // jsdom has no layout, so it doesn't implement scrollIntoView; list components
  // that keep a keyboard cursor in view (e.g. the thread navigator) call it.
  if (typeof Element.prototype.scrollIntoView !== "function") {
    Element.prototype.scrollIntoView = () => {};
  }
}
