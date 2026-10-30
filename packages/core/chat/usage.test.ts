// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { ChatSessionUsage } from "../types/chat";
import { summarizeChatSessionUsage } from "./usage";

const TICKS_PER_USD = 10_000_000_000;
const zero = { input: 0, output: 0, cache_read: 0, cache_write: 0 };

function usage(over: Partial<ChatSessionUsage> = {}): ChatSessionUsage {
  return {
    session_id: "s1",
    tokens: zero,
    uncosted_tokens: zero,
    cost_usd_ticks: 0,
    runs: { by_status: {}, terminal: 0, metered: 0, unreported: 0, total_run_ms: 0 },
    messages: { by_role: {} },
    by_model: {},
    continued_from_session_id: null,
    continued_into_session_id: null,
    forked_from_session_id: null,
    ...over,
  };
}

describe("summarizeChatSessionUsage", () => {
  it("uses the provider's own price and reports the total as complete", () => {
    const s = summarizeChatSessionUsage(
      usage({
        tokens: { input: 1000, output: 500, cache_read: 0, cache_write: 0 },
        cost_usd_ticks: 0.4 * TICKS_PER_USD,
        by_model: {
          "claude-opus-4-7": {
            tokens: { input: 1000, output: 500, cache_read: 0, cache_write: 0 },
            cost_usd_ticks: 0.4 * TICKS_PER_USD,
          },
        },
      }),
    );
    expect(s.costUsd).toBeCloseTo(0.4, 6);
    expect(s.partial).toBe(false);
    expect(s.tokenKinds).toEqual({ input: 1000, output: 500, cache_read: 0, cache_write: 0 });
    expect(s.models).toEqual([
      {
        model: "claude-opus-4-7",
        tokens: 1500,
        tokenKinds: { input: 1000, output: 500, cache_read: 0, cache_write: 0 },
        costUsd: 0.4,
      },
    ]);
  });

  it("estimates a model the provider priced none of from the rate table", () => {
    const s = summarizeChatSessionUsage(
      usage({
        tokens: { input: 1_000_000, output: 0, cache_read: 0, cache_write: 0 },
        uncosted_tokens: { input: 1_000_000, output: 0, cache_read: 0, cache_write: 0 },
        by_model: {
          "claude-sonnet-4-5": {
            tokens: { input: 1_000_000, output: 0, cache_read: 0, cache_write: 0 },
            cost_usd_ticks: 0,
          },
        },
      }),
    );
    expect(s.costUsd).toBeCloseTo(3, 6);
    expect(s.partial).toBe(false);
  });

  it("shows a dash for a model with no price and marks the total partial", () => {
    const s = summarizeChatSessionUsage(
      usage({
        tokens: { input: 2000, output: 0, cache_read: 0, cache_write: 0 },
        uncosted_tokens: { input: 1000, output: 0, cache_read: 0, cache_write: 0 },
        cost_usd_ticks: 0.1 * TICKS_PER_USD,
        by_model: {
          "claude-opus-4-7": {
            tokens: { input: 1000, output: 0, cache_read: 0, cache_write: 0 },
            cost_usd_ticks: 0.1 * TICKS_PER_USD,
          },
          "mystery-model": {
            tokens: { input: 1000, output: 0, cache_read: 0, cache_write: 0 },
            cost_usd_ticks: 0,
          },
        },
      }),
    );
    expect(s.models.find((m) => m.model === "mystery-model")?.costUsd).toBeNull();
    expect(s.costUsd).toBeCloseTo(0.1, 6);
    expect(s.partial).toBe(true);
  });

  it("marks the total partial when finished runs reported no usage", () => {
    const s = summarizeChatSessionUsage(
      usage({
        tokens: { input: 1000, output: 0, cache_read: 0, cache_write: 0 },
        cost_usd_ticks: 0.1 * TICKS_PER_USD,
        runs: { by_status: { completed: 3 }, terminal: 3, metered: 1, unreported: 2, total_run_ms: 0 },
        by_model: {
          "claude-opus-4-7": {
            tokens: { input: 1000, output: 0, cache_read: 0, cache_write: 0 },
            cost_usd_ticks: 0.1 * TICKS_PER_USD,
          },
        },
      }),
    );
    expect(s.partial).toBe(true);
    expect(s.unreportedRuns).toBe(2);
  });

  it("never reports a zero cost when nothing was recorded", () => {
    const s = summarizeChatSessionUsage(usage({ messages: { by_role: { user: 1 } } }));
    expect(s.costUsd).toBeNull();
    expect(s.hasUsage).toBe(false);
    expect(s.messages).toEqual({ user: 1, assistant: 0 });
  });

  it("counts finished and unfinished runs from the open status map", () => {
    const s = summarizeChatSessionUsage(
      usage({
        runs: {
          by_status: { completed: 12, failed: 1, cancelled: 2, running: 1, queued: 1, deferred: 1 },
          terminal: 15,
          metered: 15,
          unreported: 0,
          total_run_ms: 18 * 60_000 + 4_000,
        },
      }),
    );
    expect(s.runs).toEqual({ completed: 12, failed: 1, cancelled: 2, inProgress: 3 });
    expect(s.totalRunMs).toBe(1_084_000);
  });
});
