/**
 * Near-viewport lazy shell (MUL-4922 performance contract).
 *
 * jsdom has no IntersectionObserver, so these tests install a controllable
 * fake: it records observed elements and lets a test decide when a block
 * becomes near-viewport. That makes the deferral, the latch and the size
 * reservation observable rather than assumed.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { renderToString } from "react-dom/server";
import { createRoot, hydrateRoot } from "react-dom/client";
import { flushSync } from "react-dom";
import { LazyRichBlock, useRichBlockHold } from "./lazy-rich-block";
import { RichContentScrollRootProvider } from "./scroll-root";
import { resetMountedBlocks } from "./mounted-block-registry";

type IOCallback = (entries: { isIntersecting: boolean }[]) => void;

interface FakeObserver {
  callback: IOCallback;
  options?: IntersectionObserverInit;
  observed: Element[];
  disconnected: boolean;
}

let observers: FakeObserver[] = [];

beforeEach(() => {
  observers = [];
  resetMountedBlocks();
  class FakeIntersectionObserver {
    private readonly self: FakeObserver;
    constructor(callback: IOCallback, options?: IntersectionObserverInit) {
      this.self = { callback, options, observed: [], disconnected: false };
      observers.push(this.self);
    }
    observe(el: Element) {
      this.self.observed.push(el);
    }
    disconnect() {
      this.self.disconnected = true;
    }
    unobserve() {}
    takeRecords() {
      return [];
    }
  }
  vi.stubGlobal("IntersectionObserver", FakeIntersectionObserver);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

/** Fire the near-viewport signal on every live observer. */
function enterViewport() {
  act(() => {
    for (const o of observers) o.callback([{ isIntersecting: true }]);
  });
}

const Expensive = () => <div data-testid="expensive">diagram</div>;

describe("LazyRichBlock", () => {
  it("does not mount its child until the block is near the viewport", () => {
    render(
      <LazyRichBlock reservedHeightPx={280}>
        <Expensive />
      </LazyRichBlock>,
    );

    expect(screen.queryByTestId("expensive")).toBeNull();
    expect(observers).toHaveLength(1);
    expect(observers[0]?.observed).toHaveLength(1);
  });

  it("mounts the child once it becomes near-viewport", () => {
    render(
      <LazyRichBlock reservedHeightPx={280}>
        <Expensive />
      </LazyRichBlock>,
    );
    expect(screen.queryByTestId("expensive")).toBeNull();

    enterViewport();

    expect(screen.getByTestId("expensive")).toBeInTheDocument();
  });

  it("stops using the near observer after mounting", () => {
    render(
      <LazyRichBlock reservedHeightPx={280}>
        <Expensive />
      </LazyRichBlock>,
    );
    enterViewport();

    expect(observers[0]?.disconnected).toBe(true);
  });

  // Howard's stated risk: lazy mounting must not disturb Virtuoso's height
  // measurement. The shell therefore reserves the same space before and after.
  it("reserves the same height before and after mount", () => {
    const { container } = render(
      <LazyRichBlock reservedHeightPx={480}>
        <Expensive />
      </LazyRichBlock>,
    );
    const shell = container.querySelector("[data-rich-block-shell]") as HTMLElement;

    expect(shell.style.minHeight).toBe("480px");
    expect(shell.hasAttribute("data-mounted")).toBe(false);

    enterViewport();

    expect(shell.style.minHeight).toBe("480px");
    expect(shell.hasAttribute("data-mounted")).toBe(true);
  });

  it("watches an area larger than the viewport so blocks are ready on arrival", () => {
    render(
      <LazyRichBlock reservedHeightPx={280}>
        <Expensive />
      </LazyRichBlock>,
    );

    const margin = observers[0]?.options?.rootMargin ?? "";
    const topPx = Number.parseInt(margin.split(" ")[0] ?? "0", 10);
    // Must exceed the chat list's own overscan (600px bottom) or a block would
    // still be blank when Virtuoso has already rendered its row.
    expect(topPx).toBeGreaterThan(600);
  });

  // Chat scrolls inside its own element. With the default (viewport) root,
  // rootMargin expands the wrong box, so a block only loads once it is already
  // visible — the preloading is silently dead in the surface that needs it most.
  it("observes against the surface's scroll root when one is provided", () => {
    const scrollRoot = document.createElement("div");
    document.body.appendChild(scrollRoot);

    render(
      <RichContentScrollRootProvider scrollRoot={scrollRoot}>
        <LazyRichBlock reservedHeightPx={280}>
          <Expensive />
        </LazyRichBlock>
      </RichContentScrollRootProvider>,
    );

    expect(observers[0]?.options?.root).toBe(scrollRoot);
    scrollRoot.remove();
  });

  it("falls back to the viewport root for page-scrolled surfaces", () => {
    // Issue description / Comment scroll with the page; a null root is correct.
    render(
      <LazyRichBlock reservedHeightPx={280}>
        <Expensive />
      </LazyRichBlock>,
    );

    expect(observers[0]?.options?.root ?? null).toBeNull();
  });

  it("mounts via an effect when IntersectionObserver is unavailable", () => {
    vi.unstubAllGlobals();
    vi.stubGlobal("IntersectionObserver", undefined);

    render(
      <LazyRichBlock reservedHeightPx={280}>
        <Expensive />
      </LazyRichBlock>,
    );

    // Degrading to eager mount is correct; rendering nothing would not be.
    // The mount happens in an effect (not in the initial state) so the first
    // committed frame still matches what a server render would produce — see
    // the SSR suite below.
    expect(screen.getByTestId("expensive")).toBeInTheDocument();
  });
});

/**
 * Releasing far off-screen blocks (CHE-2164).
 *
 * A mounted Mermaid block holds an SVG, two sandboxed iframes' documents and
 * a viewer's worth of state. Once the user has scrolled far away and stayed
 * there, the shell tears the block down to its reserved-size placeholder, and
 * rebuilds it when it comes back near. Anything the user is interacting with
 * is never torn down.
 */
describe("LazyRichBlock release", () => {
  const RELEASE_AFTER_MS = 5_000;

  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  /** The observer watching a mounted block for "far away". */
  function farObserver(): FakeObserver {
    const live = observers.filter((o) => !o.disconnected);
    expect(live).toHaveLength(1);
    return live[0]!;
  }

  function leaveFar() {
    act(() => {
      farObserver().callback([{ isIntersecting: false }]);
    });
  }

  function idle() {
    act(() => {
      vi.advanceTimersByTime(RELEASE_AFTER_MS);
    });
  }

  function mountBlock(extra?: React.ReactNode) {
    const view = render(
      <LazyRichBlock reservedHeightPx={280} sourceKey="chart">
        <Expensive />
        {extra}
      </LazyRichBlock>,
    );
    enterViewport();
    expect(screen.getByTestId("expensive")).toBeInTheDocument();
    return view;
  }

  it("watches a mounted block with a margin wider than the mount margin", () => {
    mountBlock();

    const near = Number.parseInt(observers[0]?.options?.rootMargin ?? "0", 10);
    const far = Number.parseInt(farObserver().options?.rootMargin ?? "0", 10);
    expect(far).toBeGreaterThan(near);
  });

  it("releases the block to a placeholder after it stays far away", () => {
    const { container } = mountBlock();

    leaveFar();
    expect(screen.getByTestId("expensive")).toBeInTheDocument();
    idle();

    expect(screen.queryByTestId("expensive")).toBeNull();
    const shell = container.querySelector("[data-rich-block-shell]") as HTMLElement;
    expect(shell.hasAttribute("data-mounted")).toBe(false);
  });

  it("keeps the measured height when the real block was taller than the reservation", () => {
    const { container } = mountBlock();
    const shell = container.querySelector("[data-rich-block-shell]") as HTMLElement;
    vi.spyOn(shell, "getBoundingClientRect").mockReturnValue({
      height: 640,
      width: 100,
      top: 0,
      left: 0,
      right: 100,
      bottom: 640,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    });

    leaveFar();
    idle();

    expect(shell.style.minHeight).toBe("640px");
  });

  it("does not release when the block comes back before the delay ends", () => {
    mountBlock();

    leaveFar();
    act(() => {
      vi.advanceTimersByTime(RELEASE_AFTER_MS - 1_000);
    });
    act(() => {
      farObserver().callback([{ isIntersecting: true }]);
    });
    idle();

    expect(screen.getByTestId("expensive")).toBeInTheDocument();
  });

  it("mounts a released block again when it comes near", () => {
    mountBlock();
    leaveFar();
    idle();
    expect(screen.queryByTestId("expensive")).toBeNull();

    enterViewport();

    expect(screen.getByTestId("expensive")).toBeInTheDocument();
  });

  it("does not release while focus is inside the block", () => {
    mountBlock(<button type="button">zoom in</button>);
    act(() => {
      screen.getByRole("button", { name: "zoom in" }).focus();
    });

    leaveFar();
    idle();

    expect(screen.getByTestId("expensive")).toBeInTheDocument();
  });

  it("releases after focus leaves the block", () => {
    mountBlock(<button type="button">zoom in</button>);
    const button = screen.getByRole("button", { name: "zoom in" });
    act(() => button.focus());
    leaveFar();
    idle();
    act(() => button.blur());
    idle();

    expect(screen.queryByTestId("expensive")).toBeNull();
  });

  it("does not release while a child holds it, for example an open viewer", () => {
    function Holder({ held }: { held: boolean }) {
      useRichBlockHold(held);
      return null;
    }
    const { rerender } = render(
      <LazyRichBlock reservedHeightPx={280} sourceKey="held">
        <Expensive />
        <Holder held />
      </LazyRichBlock>,
    );
    enterViewport();

    leaveFar();
    idle();
    expect(screen.getByTestId("expensive")).toBeInTheDocument();

    rerender(
      <LazyRichBlock reservedHeightPx={280} sourceKey="held">
        <Expensive />
        <Holder held={false} />
      </LazyRichBlock>,
    );
    idle();
    expect(screen.queryByTestId("expensive")).toBeNull();
  });

  it("does not let a recycled row resurrect a block that was released", () => {
    const first = mountBlock();
    leaveFar();
    idle();
    first.unmount();
    observers = [];

    const second = render(
      <LazyRichBlock reservedHeightPx={280} sourceKey="chart">
        <Expensive />
      </LazyRichBlock>,
    );

    expect(second.queryByTestId("expensive")).toBeNull();
  });
});

/**
 * Virtualized-row recycling.
 *
 * Chat's list unmounts rows that scroll far enough away. A `mounted` flag held
 * only in component state disappears with the row, so scrolling back would
 * re-run Mermaid, rebuild the sandboxed iframe and drop the viewer's pan/zoom —
 * turning the one-time mount cost into a per-pass cost. The latch therefore
 * lives outside the component, keyed by content.
 */
describe("LazyRichBlock across row recycling", () => {
  const SOURCE = "flowchart LR\n  A --> B";

  beforeEach(() => {
    resetMountedBlocks();
  });

  it("re-mounts a recycled block immediately, without waiting to be seen again", () => {
    const first = render(
      <LazyRichBlock reservedHeightPx={280} sourceKey={SOURCE}>
        <Expensive />
      </LazyRichBlock>,
    );
    enterViewport();
    expect(first.getByTestId("expensive")).toBeInTheDocument();

    // Virtuoso recycles the row: the whole subtree is unmounted.
    first.unmount();
    observers = [];

    // The row comes back. No intersection is reported this time — the block was
    // already built once, so it must not wait to be observed again.
    const second = render(
      <LazyRichBlock reservedHeightPx={280} sourceKey={SOURCE}>
        <Expensive />
      </LazyRichBlock>,
    );

    expect(second.getByTestId("expensive")).toBeInTheDocument();
  });

  it("does not resurrect a different block that was never mounted", () => {
    const first = render(
      <LazyRichBlock reservedHeightPx={280} sourceKey={SOURCE}>
        <Expensive />
      </LazyRichBlock>,
    );
    enterViewport();
    first.unmount();
    observers = [];

    // Different content => different key => still deferred.
    const other = render(
      <LazyRichBlock reservedHeightPx={280} sourceKey="graph TD\n  X --> Y">
        <Expensive />
      </LazyRichBlock>,
    );

    expect(other.queryByTestId("expensive")).toBeNull();
  });

  it("keeps deferring when no sourceKey is supplied", () => {
    // Without an identity there is nothing to remember; the block must not be
    // treated as already-mounted.
    const first = render(
      <LazyRichBlock reservedHeightPx={280}>
        <Expensive />
      </LazyRichBlock>,
    );
    enterViewport();
    first.unmount();
    observers = [];

    const second = render(
      <LazyRichBlock reservedHeightPx={280}>
        <Expensive />
      </LazyRichBlock>,
    );

    expect(second.queryByTestId("expensive")).toBeNull();
  });
});

/**
 * Server/client determinism.
 *
 * The initial `mounted` state must not be derived from feature detection. If it
 * were, the server (no `window`) and the browser (has IntersectionObserver)
 * would disagree on the very first frame: React would find different markup
 * during hydration, and the server render would silently bypass the lazy gate
 * it exists to enforce. `"use client"` does not exempt a component from Next's
 * server render, so this has to hold.
 */
describe("LazyRichBlock SSR", () => {
  /**
   * Render the way a server would.
   *
   * jsdom always provides `window`, so simply calling renderToString here would
   * take the SAME feature-detection branch as the browser and could not observe
   * the mismatch at all. Removing IntersectionObserver for the duration
   * reproduces the real asymmetry: on a server the detection says
   * "unsupported", in the browser it says "supported". A component whose first
   * frame depends on that answer renders differently in the two environments —
   * which is precisely the bug.
   */
  function serverRender(ui: Parameters<typeof renderToString>[0]): string {
    const browserObserver = globalThis.IntersectionObserver;
    vi.stubGlobal("IntersectionObserver", undefined);
    try {
      return renderToString(ui);
    } finally {
      vi.stubGlobal("IntersectionObserver", browserObserver);
    }
  }

  it("renders the placeholder, not the block, on the server", () => {
    const html = serverRender(
      <LazyRichBlock reservedHeightPx={280}>
        <Expensive />
      </LazyRichBlock>,
    );

    expect(html).toContain("data-rich-block-shell");
    // The expensive subtree must not be in server output: SSR has no viewport,
    // so nothing is near it.
    expect(html).not.toContain("expensive");
    expect(html).not.toContain("data-mounted");
  });

  it("hydrates the server markup without a mismatch", async () => {
    const html = serverRender(
      <LazyRichBlock reservedHeightPx={280}>
        <Expensive />
      </LazyRichBlock>,
    );

    const container = document.createElement("div");
    container.innerHTML = html;
    document.body.appendChild(container);

    const errors: string[] = [];
    const errorSpy = vi.spyOn(console, "error").mockImplementation((...args) => {
      errors.push(args.map(String).join(" "));
    });

    let root: ReturnType<typeof hydrateRoot> | undefined;
    await act(async () => {
      root = hydrateRoot(
        container,
        <LazyRichBlock reservedHeightPx={280}>
          <Expensive />
        </LazyRichBlock>,
      );
    });

    const hydrationErrors = errors.filter((e) =>
      /hydrat|did not match|mismatch/i.test(e),
    );
    expect(hydrationErrors).toEqual([]);

    errorSpy.mockRestore();
    await act(async () => {
      root?.unmount();
    });
    container.remove();
  });

  it("produces the same first frame on server and client", () => {
    // Same component, both environments, IntersectionObserver present on the
    // client — the frames must be identical before any effect runs.
    const serverHtml = serverRender(
      <LazyRichBlock reservedHeightPx={280}>
        <Expensive />
      </LazyRichBlock>,
    );

    const container = document.createElement("div");
    document.body.appendChild(container);
    const root = createRoot(container);
    // No act(): flushing effects is exactly what we must NOT do here, because
    // the comparison is against the first committed frame.
    flushSync(() => {
      root.render(
        <LazyRichBlock reservedHeightPx={280}>
          <Expensive />
        </LazyRichBlock>,
      );
    });
    const clientFirstFrame = container.innerHTML;

    // Normalize inline-style serialization only: React's server renderer emits
    // `min-height:280px` while the browser's CSSOM round-trips it as
    // `min-height: 280px;`. That difference is cosmetic — React hydration does
    // not string-compare markup — and the hydration test above is the
    // authoritative check. Everything else is compared verbatim.
    const normalize = (html: string) =>
      html.replace(/style="([^"]*)"/g, (_, css: string) =>
        `style="${css.replace(/\s*;\s*$/, "").replace(/:\s+/g, ":")}"`,
      );

    expect(normalize(clientFirstFrame)).toBe(normalize(serverHtml));

    act(() => root.unmount());
    container.remove();
  });
});
