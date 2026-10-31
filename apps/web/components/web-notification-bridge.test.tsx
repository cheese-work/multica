import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";

const push = vi.hoisted(() => vi.fn());
const platform = vi.hoisted(() => ({
  handler: null as null | ((p: unknown) => void),
}));

vi.mock("@multica/views/navigation", () => ({ useNavigation: () => ({ push }) }));
vi.mock("@multica/core/platform", () => ({
  registerSystemNotificationClickHandler: (h: typeof platform.handler) => {
    platform.handler = h;
  },
}));

import { WebNotificationBridge } from "./web-notification-bridge";

const click = (payload: Record<string, unknown>) => {
  render(<WebNotificationBridge />);
  platform.handler?.({ slug: "acme", itemId: "r1", issueKey: "r1", title: "t", body: "", ...payload });
};

describe("WebNotificationBridge", () => {
  afterEach(() => {
    cleanup();
    push.mockClear();
  });

  it("opens the inbox on the clicked item for an issue notification", () => {
    click({ issueKey: "i-1" });
    expect(push).toHaveBeenCalledWith("/acme/inbox?issue=i-1");
  });

  it("opens the DM for a DM-linked record", () => {
    click({ chatSessionId: "s 1" });
    expect(push).toHaveBeenCalledWith("/acme/chat?session=s%201");
  });

  it("ignores a click with no resolved workspace", () => {
    click({ slug: "", chatSessionId: "s1" });
    expect(push).not.toHaveBeenCalled();
  });
});
