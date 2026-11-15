import { describe, expect, it } from "vitest";
import { ApiError } from "../api/client";
import {
  CHAT_SEARCH_MAX_QUERY_LENGTH,
  chatSearchFailure,
  chatSearchInfiniteOptions,
  isChatSearchQueryTooLong,
} from "./search";

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
