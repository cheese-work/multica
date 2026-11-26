import { forwardRef, useImperativeHandle, type ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/react";
import type { ChatSession } from "@multica/core/types";

const { scrollToIndex } = vi.hoisted(() => ({ scrollToIndex: vi.fn() }));

vi.mock("react-virtuoso", () => ({
  Virtuoso: forwardRef(function Virtuoso(
    { data, itemContent }: { data: ChatSession[]; itemContent: (i: number, s: ChatSession) => ReactNode },
    ref,
  ) {
    useImperativeHandle(ref, () => ({ scrollToIndex }));
    return <div>{data.slice(0, 2).map((s, i) => <div key={s.id}>{itemContent(i, s)}</div>)}</div>;
  }),
}));

import { SessionHistoryList } from "./session-history-list";

const sessions = ["s1", "s2", "s3", "s4"].map((id) => ({ id, title: id }) as ChatSession);
const renderRow = (s: ChatSession) => <span>{s.title}</span>;

beforeEach(() => scrollToIndex.mockClear());

describe("SessionHistoryList reveal", () => {
  it("scrolls a session outside the rendered window to the middle of the list", () => {
    render(<SessionHistoryList sessions={sessions} renderRow={renderRow} revealSessionId="s4" />);
    expect(scrollToIndex).toHaveBeenCalledWith({ index: 3, align: "center" });
  });

  it("follows a new request and ignores an id that is not in the list", () => {
    const { rerender } = render(<SessionHistoryList sessions={sessions} renderRow={renderRow} />);
    expect(scrollToIndex).not.toHaveBeenCalled();
    rerender(<SessionHistoryList sessions={sessions} renderRow={renderRow} revealSessionId="s3" />);
    expect(scrollToIndex).toHaveBeenLastCalledWith({ index: 2, align: "center" });
    scrollToIndex.mockClear();
    rerender(<SessionHistoryList sessions={sessions} renderRow={renderRow} revealSessionId="missing" />);
    expect(scrollToIndex).not.toHaveBeenCalled();
  });
});
