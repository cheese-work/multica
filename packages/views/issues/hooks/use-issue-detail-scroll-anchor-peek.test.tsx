import { useState } from "react";
import { act, fireEvent, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ScrollRestorationProvider, type ScrollRestorationAdapter } from "../../platform";
import {
  issueScrollAnchorMementoKey,
  useIssueDetailScrollAnchor,
  type ScrollAnchor,
} from "./use-issue-detail-scroll-anchor";

const saved = JSON.stringify({ key: "comment:30", offset: 30 } satisfies ScrollAnchor);

function Harness({
  disabled,
  onAnchor,
}: {
  disabled?: boolean;
  onAnchor: (anchor: ScrollAnchor | null) => void;
}) {
  const [el, setEl] = useState<HTMLDivElement | null>(null);
  const anchor = useIssueDetailScrollAnchor({
    issueId: "issue-1",
    scrollContainerEl: el,
    ready: true,
    disabled,
    keyAt: (i) => `comment:${i}`,
  });
  onAnchor(anchor);
  return (
    <div ref={setEl} data-testid="scroller">
      <div data-item-index="5" />
    </div>
  );
}

describe("useIssueDetailScrollAnchor in a peek", () => {
  beforeEach(() => {
    vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => {
      cb(0);
      return 1;
    });
  });
  afterEach(() => vi.unstubAllGlobals());

  function mount(disabled: boolean | undefined) {
    const setViewState = vi.fn();
    const adapter: ScrollRestorationAdapter = {
      get: () => undefined,
      getViewState: (key) => (key === issueScrollAnchorMementoKey("issue-1") ? saved : undefined),
      setViewState,
    };
    let anchor: ScrollAnchor | null = null;
    const view = render(
      <ScrollRestorationProvider adapter={adapter}>
        <Harness disabled={disabled} onAnchor={(a) => (anchor = a)} />
      </ScrollRestorationProvider>,
    );
    return { view, setViewState, anchor: () => anchor };
  }

  it("restores the saved anchor when enabled", () => {
    expect(mount(false).anchor()).toEqual({ key: "comment:30", offset: 30 });
  });

  it("neither restores nor records when disabled", () => {
    const { view, setViewState, anchor } = mount(true);
    expect(anchor()).toBeNull();
    act(() => {
      fireEvent.scroll(view.getByTestId("scroller"));
    });
    expect(setViewState).not.toHaveBeenCalled();
  });
});
