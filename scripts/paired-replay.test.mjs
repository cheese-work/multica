import assert from "node:assert/strict";
import test from "node:test";

import { assertReplayActionAllowed, createPairPlan, measureRun } from "./paired-replay.mjs";

const metrics = [
  "input_tokens",
  "cached_input_tokens",
  "cache_write_input_tokens",
  "output_tokens",
  "reasoning_output_tokens",
  "total_tokens",
];

function usage(input, cached, output) {
  return {
    input_tokens: input,
    cached_input_tokens: cached,
    cache_write_input_tokens: 0,
    output_tokens: output,
    reasoning_output_tokens: 0,
    total_tokens: input + output,
  };
}

function record(responseId, ordinal, tokenUsage) {
  return {
    type: "token_usage_record",
    ordinal,
    payload: {
      response_id: responseId,
      root_turn_id: "root-1",
      session_id: "session-1",
      thread_id: "thread-1",
      turn_id: "turn-1",
      usage: tokenUsage,
      turn_token_usage: tokenUsage,
      thread_token_usage: tokenUsage,
    },
  };
}

function tokenCount(ordinal, total) {
  return {
    type: "event_msg",
    ordinal,
    payload: {
      type: "token_count",
      info: { total_token_usage: total },
    },
  };
}

function completeRunEvents() {
  const first = usage(7, 3, 2);
  const second = usage(5, 2, 1);
  return [
    { type: "session_meta", ordinal: 0, payload: { session_id: "session-1" } },
    tokenCount(1, usage(20, 10, 4)),
    { type: "turn_context", ordinal: 2, payload: { root_turn_id: "root-1", model: "gpt-6.1-sol" } },
    record("response-1", 3, first),
    record("response-2", 4, second),
    tokenCount(5, usage(32, 15, 7)),
    tokenCount(6, usage(32, 15, 7)),
  ];
}

test("measures each response and verifies the aggregate snapshot delta", () => {
  const result = measureRun(completeRunEvents(), {
    rootTurnId: "root-1",
    sessionId: "session-1",
    model: "gpt-6.1-sol",
    startOrdinal: 1,
  });

  assert.equal(result.perCall.length, 2);
  assert.equal(result.perCall[0].usage.input_tokens, 7);
  assert.equal(result.perCall[0].usage.cached_input_tokens, 3);
  assert.equal(result.aggregate.input_tokens, 12);
  assert.equal(result.aggregate.cached_input_tokens, 5);
  assert.equal(result.maxContextTokens, 7);
});

test("fails closed when a response usage record is missing", () => {
  const events = completeRunEvents().filter(
    (event) => event.payload?.response_id !== "response-2",
  );

  assert.throws(() => measureRun(events, {
    rootTurnId: "root-1",
    sessionId: "session-1",
    model: "gpt-6.1-sol",
    startOrdinal: 1,
  }), /snapshot delta/);
});

test("fails closed on duplicate response records", () => {
  const events = completeRunEvents();
  events.splice(5, 0, { ...events[3], ordinal: 7 });

  assert.throws(() => measureRun(events, {
    rootTurnId: "root-1",
    sessionId: "session-1",
    model: "gpt-6.1-sol",
    startOrdinal: 1,
  }), /duplicate response_id/);
});

test("fails closed on unbound response records", () => {
  const events = completeRunEvents();
  events[1] = {
    ...events[1],
    payload: { ...events[1].payload, session_id: "other-session" },
  };
  events[3] = {
    ...events[3],
    payload: { ...events[3].payload, session_id: "other-session" },
  };

  assert.throws(() => measureRun(events, {
    rootTurnId: "root-1",
    sessionId: "session-1",
    model: "gpt-6.1-sol",
    startOrdinal: 1,
  }), /session_id/);
});

test("fails closed when a replay log uses another model", () => {
  const events = completeRunEvents();
  events[2] = {
    ...events[2],
    payload: { ...events[2].payload, model: "other-model" },
  };

  assert.throws(() => measureRun(events, {
    rootTurnId: "root-1",
    sessionId: "session-1",
    model: "gpt-6.1-sol",
    startOrdinal: 1,
  }), /model identity/);
});

test("does not add repeated token_count heartbeats", () => {
  const events = completeRunEvents();
  events.push(tokenCount(8, usage(32, 15, 7)));

  const result = measureRun(events, {
    rootTurnId: "root-1",
    sessionId: "session-1",
    model: "gpt-6.1-sol",
    startOrdinal: 1,
  });

  assert.equal(result.aggregate.input_tokens, 12);
  assert.equal(result.aggregate.cached_input_tokens, 5);
});

test("binds both strategies to one fixture and readback set", () => {
  const plan = createPairPlan({
    fixtureSha256: "a".repeat(64),
    inputPrefixSha256: "b".repeat(64),
    readbacksSha256: "c".repeat(64),
    promptId: "prompt-1",
    cutoffOrdinal: 1544,
    model: "gpt-6.1-sol",
    runtimeId: "runtime-1",
    threshold: 80000,
  });

  assert.equal(plan.options.fresh.fixtureSha256, plan.options.resume.fixtureSha256);
  assert.equal(plan.options.fresh.readbacksSha256, plan.options.resume.readbacksSha256);
  assert.equal(plan.options.resume.threshold, 80000);
  assert.equal(plan.shared.cliVersion, "0.155.1");
  assert.equal(plan.shared.historyMode, "paginated");
});

test("denies platform writes and production-session mutations", () => {
  assert.throws(() => assertReplayActionAllowed({
    target: "platform",
    operation: "write",
  }), /denied/);
  assert.throws(() => assertReplayActionAllowed({
    target: "production-session",
    operation: "mutate",
  }), /denied/);
  assert.equal(assertReplayActionAllowed({
    target: "local-sandbox",
    operation: "write",
  }), true);
});
