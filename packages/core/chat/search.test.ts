import { beforeEach, describe, expect, it } from "vitest";
import { ApiError } from "../api/client";
import {
  CHAT_SEARCH_MAX_QUERY_LENGTH,
  chatSearchFailure,
  chatSearchInfiniteOptions,
  isChatSearchQueryTooLong,
  splitHighlight,
  useChatSearchStore,
} from "./search";

describe("splitHighlight", () => {
  it("marks the first case-insensitive match and keeps the surrounding words", () => {
    expect(splitHighlight("Ship the Launch plan today", "launch plan")).toEqual([
      { text: "Ship the ", match: false },
      { text: "Launch plan", match: true },
      { text: " today", match: false },
    ]);
  });

  it("returns the whole text unmarked when the query is absent or blank", () => {
    expect(splitHighlight("nothing here", "launch")).toEqual([{ text: "nothing here", match: false }]);
    expect(splitHighlight("nothing here", "  ")).toEqual([{ text: "nothing here", match: false }]);
  });

  it("does not treat regex characters in the query as a pattern", () => {
    expect(splitHighlight("cost (a+b) total", "(a+b)")).toEqual([
      { text: "cost ", match: false },
      { text: "(a+b)", match: true },
      { text: " total", match: false },
    ]);
  });
});

describe("query length", () => {
  it("flags a trimmed query over the server limit", () => {
    expect(isChatSearchQueryTooLong("a".repeat(CHAT_SEARCH_MAX_QUERY_LENGTH))).toBe(false);
    expect(isChatSearchQueryTooLong("a".repeat(CHAT_SEARCH_MAX_QUERY_LENGTH + 1))).toBe(true);
    expect(isChatSearchQueryTooLong(`  ${"a".repeat(CHAT_SEARCH_MAX_QUERY_LENGTH)}  `)).toBe(false);
  });
});

describe("chatSearchFailure", () => {
  it("maps a 503 to timed_out, a network error to disconnected, the rest to failed", () => {
    expect(chatSearchFailure(new ApiError("timeout", 503, "Service Unavailable"))).toBe("timed_out");
    expect(chatSearchFailure(new TypeError("Failed to fetch"))).toBe("disconnected");
    expect(chatSearchFailure(new ApiError("boom", 500, "Internal Server Error"))).toBe("failed");
  });
});

describe("chatSearchInfiniteOptions", () => {
  it("is disabled for a blank or too long query and pages by next_cursor", () => {
    expect(chatSearchInfiniteOptions("ws-1", "   ").enabled).toBe(false);
    expect(chatSearchInfiniteOptions("ws-1", "a".repeat(201)).enabled).toBe(false);
    const options = chatSearchInfiniteOptions("ws-1", " plan ");
    expect(options.enabled).toBe(true);
    expect(options.queryKey).toEqual(["chat", "ws-1", "search", "plan"]);
    expect(options.getNextPageParam({ results: [], next_cursor: "c2" }, [], null, [])).toBe("c2");
    expect(options.getNextPageParam({ results: [], next_cursor: null }, [], null, [])).toBeUndefined();
  });
});

describe("useChatSearchStore", () => {
  beforeEach(() => useChatSearchStore.setState({ queries: {} }));

  it("keeps one query per scope and clears only that scope", () => {
    useChatSearchStore.getState().setQuery("page", "plan");
    useChatSearchStore.getState().setQuery("popover", "other");
    useChatSearchStore.getState().clear("page");
    expect(useChatSearchStore.getState().queries).toEqual({ popover: "other" });
  });
});
