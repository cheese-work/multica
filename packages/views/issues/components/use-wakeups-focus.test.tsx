// @vitest-environment jsdom

import { describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/react";
import { NavigationProvider } from "../../navigation";
import type { NavigationAdapter } from "../../navigation";
import { useWakeupsFocus } from "./use-wakeups-focus";

function Harness({ resourceId, onFocus, enabled = true }: { resourceId: string; onFocus: () => void; enabled?: boolean }) {
  useWakeupsFocus(resourceId, onFocus, enabled);
  return null;
}

// "?focus=wakeups" is the deep link to a project's or an issue's wakeup rules:
// the page reveals them once and clears the param, so a refresh does not replay it.
describe("useWakeupsFocus", () => {
  // A fresh adapter object per render, as the web platform hands down.
  const adapter = (search: string, replace = vi.fn()): NavigationAdapter => ({
    push: vi.fn(),
    replace,
    back: vi.fn(),
    pathname: "/acme/projects/p1",
    searchParams: new URLSearchParams(search),
    hash: "",
    getShareableUrl: (path: string) => `https://app.test${path}`,
  });

  function setup(search: string, enabled = true) {
    const replace = vi.fn();
    const onFocus = vi.fn();
    const tree = (value: NavigationAdapter, id: string, on = enabled) => (
      <NavigationProvider value={value}>
        <Harness resourceId={id} onFocus={onFocus} enabled={on} />
      </NavigationProvider>
    );
    const view = render(tree(adapter(search, replace), "p1"));
    return {
      replace,
      onFocus,
      settle: (next: string, id = "p1") => view.rerender(tree(adapter(next, replace), id)),
      enable: (next: string) => view.rerender(tree(adapter(next, replace), "p1", true)),
    };
  }

  it("reveals the rules once and drops the focus param, keeping the others", () => {
    const { replace, onFocus } = setup("tab=a&focus=wakeups");
    expect(onFocus).toHaveBeenCalledTimes(1);
    expect(replace).toHaveBeenCalledWith("/acme/projects/p1?tab=a");
  });

  it("does nothing on an ordinary visit or a different focus", () => {
    const { replace, onFocus } = setup("focus=conversation_starters");
    expect(onFocus).not.toHaveBeenCalled();
    expect(replace).not.toHaveBeenCalled();
  });

  it("does not replay on the re-render the replace causes, but lands a second click", () => {
    const { onFocus, settle } = setup("focus=wakeups");
    settle("focus=wakeups");
    expect(onFocus).toHaveBeenCalledTimes(1);
    settle("");
    settle("focus=wakeups");
    expect(onFocus).toHaveBeenCalledTimes(2);
  });

  it("lands a link aimed at a different resource while the first is still latched", () => {
    const { onFocus, settle } = setup("focus=wakeups");
    settle("focus=wakeups", "p2");
    expect(onFocus).toHaveBeenCalledTimes(2);
  });

  it("keeps the param while disabled and consumes it once enabled", () => {
    const { replace, onFocus, enable } = setup("focus=wakeups", false);
    expect(onFocus).not.toHaveBeenCalled();
    expect(replace).not.toHaveBeenCalled();
    enable("focus=wakeups");
    expect(onFocus).toHaveBeenCalledTimes(1);
    expect(replace).toHaveBeenCalledWith("/acme/projects/p1");
  });

  it("is inert without a navigation adapter", () => {
    const onFocus = vi.fn();
    render(<Harness resourceId="p1" onFocus={onFocus} />);
    expect(onFocus).not.toHaveBeenCalled();
  });
});
