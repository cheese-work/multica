import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";

import { assertReplayActionAllowed, createPairPlan, measurePair, measureRun } from "./paired-replay.mjs";

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
    {
      type: "session_meta",
      ordinal: 0,
      payload: {
        session_id: "session-1",
        cli_version: "0.155.1",
        history_mode: "paginated",
      },
    },
    tokenCount(1, usage(20, 10, 4)),
    { type: "turn_context", ordinal: 2, payload: { root_turn_id: "root-1", model: "gpt-6.1-sol" } },
    record("response-1", 3, first),
    record("response-2", 4, second),
    tokenCount(5, usage(32, 15, 7)),
    tokenCount(6, usage(32, 15, 7)),
  ];
}

function testPlan(threshold = 80000) {
  return createPairPlan({
    fixtureSha256: "2ec89b207ebc469831ea7e56be41145048d23b2123bcf175272c8d235815efff",
    inputPrefixSha256: "460f94059f51a5015566539ef410da60ac4c68f13e3e298fb8f9e14385fb96bb",
    readbacksSha256: "c".repeat(64),
    promptId: "msg_01a15d32-e835-7153-a81c-0c3015e3f834",
    cutoffOrdinal: 1544,
    model: "gpt-6.1-sol",
    runtimeId: "bc0835c7-a25d-4a16-a3bc-e2921d092ed9",
    threshold,
  });
}

function runOption(sessionId) {
  const events = completeRunEvents().map((event) => {
    if (event.type === "session_meta" || event.type === "token_usage_record") {
      return { ...event, payload: { ...event.payload, session_id: sessionId } };
    }
    return event;
  });
  return { events, sessionId, rootTurnId: "root-1", startOrdinal: 1 };
}

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function logBytes(option) {
  return Buffer.from(`${option.events.map((event) => JSON.stringify(event)).join("\n")}\n`);
}

function receiptBytes(plan, optionName, option, overrides = {}) {
  const log = logBytes(option);
  const taskId = `task-${optionName}`;
  const runtimeReadbackBytes = Buffer.from(JSON.stringify([{
    id: taskId,
    status: "completed",
    runtime_id: plan.shared.runtimeId,
  }]));
  return {
    logBytes: log,
    runtimeReadbackBytes,
    receiptBytes: Buffer.from(JSON.stringify({
      schemaVersion: 1,
      planSha256: plan.planSha256,
      option: optionName,
      strategy: plan.options[optionName].strategy,
      sessionId: option.sessionId,
      logSha256: sha256(log),
      runtimeId: plan.shared.runtimeId,
      runtimeSource: "executor-attested",
      taskId,
      runtimeReadbackSha256: sha256(runtimeReadbackBytes),
      inputPrefixSha256: plan.shared.inputPrefixSha256,
      ...(optionName === "resume" ? { threshold: plan.threshold } : {}),
      ...overrides,
    })),
  };
}

function artifactOption(plan, optionName, sessionId, overrides = {}) {
  const option = runOption(sessionId);
  return { ...option, ...receiptBytes(plan, optionName, option, overrides) };
}

function replaceReceiptFields(option, fields) {
  const receipt = JSON.parse(option.receiptBytes.toString("utf8"));
  Object.assign(receipt, fields);
  option.receiptBytes = Buffer.from(JSON.stringify(receipt));
}

test("measures each response and verifies the aggregate snapshot delta", () => {
  const result = measureRun(completeRunEvents(), {
    rootTurnId: "root-1",
    sessionId: "session-1",
    model: "gpt-6.1-sol",
    startOrdinal: 1,
    cliVersion: "0.155.1",
    historyMode: "paginated",
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
    cliVersion: "0.155.1",
    historyMode: "paginated",
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
    cliVersion: "0.155.1",
    historyMode: "paginated",
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
    cliVersion: "0.155.1",
    historyMode: "paginated",
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
    cliVersion: "0.155.1",
    historyMode: "paginated",
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
    cliVersion: "0.155.1",
    historyMode: "paginated",
  });

  assert.equal(result.aggregate.input_tokens, 12);
  assert.equal(result.aggregate.cached_input_tokens, 5);
});

test("fails closed when a token_count counter resets during a run", () => {
  const events = completeRunEvents();
  events[4].ordinal = 5;
  events[5] = tokenCount(6, usage(5, 2, 1));
  events[6] = tokenCount(7, usage(5, 2, 1));
  events.splice(4, 0, tokenCount(4, usage(0, 0, 0)));

  assert.throws(() => measureRun(events, {
    rootTurnId: "root-1",
    sessionId: "session-1",
    model: "gpt-6.1-sol",
    startOrdinal: 1,
    runtimeId: "runtime-1",
    cliVersion: "0.155.1",
    historyMode: "paginated",
  }), /token_count counter reset/);
});

test("fails closed when qualified session provenance is missing", () => {
  for (const field of ["cli_version", "history_mode"]) {
    const events = completeRunEvents();
    delete events[0].payload[field];

    assert.throws(() => measureRun(events, {
      rootTurnId: "root-1",
      sessionId: "session-1",
      model: "gpt-6.1-sol",
      startOrdinal: 1,
      cliVersion: "0.155.1",
      historyMode: "paginated",
    }), new RegExp(field));
  }
});

test("accepts Codex session metadata without a runtime_id", () => {
  const events = completeRunEvents();

  assert.doesNotThrow(() => measureRun(events, {
    rootTurnId: "root-1",
    sessionId: "session-1",
    model: "gpt-6.1-sol",
    startOrdinal: 1,
    cliVersion: "0.155.1",
    historyMode: "paginated",
  }));
});

test("fails closed when an executor attestation receipt is missing", () => {
  const fresh = runOption("fresh-session");
  const resume = runOption("resume-session");
  fresh.logBytes = logBytes(fresh);
  resume.logBytes = logBytes(resume);
  assert.throws(() => measurePair(testPlan(), {
    fresh,
    resume,
  }), /fresh executor receipt is required/);
});

test("fails closed when an executor receipt does not match its log and runtime", () => {
  const plan = testPlan();
  const fresh = artifactOption(plan, "fresh", "fresh-session", {
    runtimeId: "different-runtime",
  });
  const resume = artifactOption(plan, "resume", "resume-session");

  assert.throws(() => measurePair(plan, { fresh, resume }), /fresh executor receipt runtimeId/);

  const changedLog = artifactOption(plan, "fresh", "changed-log-session");
  changedLog.logBytes = Buffer.concat([changedLog.logBytes, Buffer.from("\n")]);
  assert.throws(() => measurePair(plan, {
    fresh: changedLog,
    resume: artifactOption(plan, "resume", "resume-session-2"),
  }), /fresh executor receipt logSha256/);
});

test("fails closed when the native task readback disagrees with the receipt", () => {
  const plan = testPlan();
  const fresh = artifactOption(plan, "fresh", "fresh-session");
  fresh.runtimeReadbackBytes = Buffer.from(JSON.stringify([{
    id: "task-fresh",
    status: "completed",
    runtime_id: "different-runtime",
  }]));
  const receipt = JSON.parse(fresh.receiptBytes.toString("utf8"));
  receipt.runtimeReadbackSha256 = sha256(fresh.runtimeReadbackBytes);
  fresh.receiptBytes = Buffer.from(JSON.stringify(receipt));

  assert.throws(() => measurePair(plan, {
    fresh,
    resume: artifactOption(plan, "resume", "resume-session"),
  }), /does not match a completed native task readback/);
});

test("fails closed when the executor receipt plan digest differs", () => {
  const plan = testPlan();
  const fresh = artifactOption(plan, "fresh", "fresh-session");
  replaceReceiptFields(fresh, { planSha256: "d".repeat(64) });

  assert.throws(() => measurePair(plan, {
    fresh,
    resume: artifactOption(plan, "resume", "resume-session"),
  }), /fresh executor receipt planSha256/);
});

test("fails closed when the executor receipt readback digest differs", () => {
  const plan = testPlan();
  const fresh = artifactOption(plan, "fresh", "fresh-session");
  replaceReceiptFields(fresh, { runtimeReadbackSha256: "d".repeat(64) });

  assert.throws(() => measurePair(plan, {
    fresh,
    resume: artifactOption(plan, "resume", "resume-session"),
  }), /fresh executor receipt runtimeReadbackSha256/);
});

test("fails closed when the executor receipt option differs", () => {
  const plan = testPlan();
  const fresh = artifactOption(plan, "fresh", "fresh-session");
  replaceReceiptFields(fresh, { option: "resume" });

  assert.throws(() => measurePair(plan, {
    fresh,
    resume: artifactOption(plan, "resume", "resume-session"),
  }), /fresh executor receipt option/);
});

test("fails closed when the executor receipt strategy differs", () => {
  const plan = testPlan();
  const fresh = artifactOption(plan, "fresh", "fresh-session");
  replaceReceiptFields(fresh, { strategy: "resume-and-compact" });

  assert.throws(() => measurePair(plan, {
    fresh,
    resume: artifactOption(plan, "resume", "resume-session"),
  }), /fresh executor receipt strategy/);
});

test("fails closed when the native task readback is not completed", () => {
  const plan = testPlan();
  const fresh = artifactOption(plan, "fresh", "fresh-session");
  fresh.runtimeReadbackBytes = Buffer.from(JSON.stringify([{
    id: "task-fresh",
    status: "running",
    runtime_id: plan.shared.runtimeId,
  }]));
  replaceReceiptFields(fresh, { runtimeReadbackSha256: sha256(fresh.runtimeReadbackBytes) });

  assert.throws(() => measurePair(plan, {
    fresh,
    resume: artifactOption(plan, "resume", "resume-session"),
  }), /does not match a completed native task readback/);
});

test("fails closed when the native task readback has duplicate matching records", () => {
  const plan = testPlan();
  const fresh = artifactOption(plan, "fresh", "fresh-session");
  const taskRecord = {
    id: "task-fresh",
    status: "completed",
    runtime_id: plan.shared.runtimeId,
  };
  fresh.runtimeReadbackBytes = Buffer.from(JSON.stringify([taskRecord, taskRecord]));
  replaceReceiptFields(fresh, { runtimeReadbackSha256: sha256(fresh.runtimeReadbackBytes) });

  assert.throws(() => measurePair(plan, {
    fresh,
    resume: artifactOption(plan, "resume", "resume-session"),
  }), /does not match a completed native task readback/);
});

test("fails closed when receipt prefix or threshold differs from the plan", () => {
  const plan = testPlan();
  const fresh = artifactOption(plan, "fresh", "fresh-session", {
    inputPrefixSha256: "e".repeat(64),
  });
  const resume = artifactOption(plan, "resume", "resume-session");
  assert.throws(() => measurePair(plan, { fresh, resume }), /fresh executor receipt inputPrefixSha256/);

  const matchingFresh = artifactOption(plan, "fresh", "fresh-session-2");
  const mismatchedResume = artifactOption(plan, "resume", "resume-session-2", { threshold: 80001 });
  assert.throws(() => measurePair(plan, {
    fresh: matchingFresh,
    resume: mismatchedResume,
  }), /resume executor receipt threshold/);
});

test("reports prefix and resume-threshold binding as not verified when absent", () => {
  const plan = testPlan();
  const fresh = artifactOption(plan, "fresh", "fresh-session", {
    inputPrefixSha256: undefined,
  });
  const resume = artifactOption(plan, "resume", "resume-session", {
    inputPrefixSha256: undefined,
    threshold: undefined,
  });
  const result = measurePair(plan, { fresh, resume });

  assert.equal(result.binding.status, "NOT-VERIFIED");
  assert.equal(result.resumeContextCap, "NOT-VERIFIED");
  assert.equal(result.pairedEquivalence, "NOT-CLAIMED");
});

test("reports recomputed artifact digests and gates the resume cap on binding", () => {
  const plan = testPlan();
  const fresh = artifactOption(plan, "fresh", "fresh-session");
  const resume = artifactOption(plan, "resume", "resume-session");
  const result = measurePair(plan, { fresh, resume });

  assert.equal(result.binding.status, "EXECUTOR-ATTESTED");
  assert.equal(result.resumeContextCap, "PASS");
  assert.equal(result.pairedEquivalence, "NOT-CLAIMED");
  assert.equal(result.evidence.fresh.logSha256, sha256(fresh.logBytes));
  assert.equal(result.evidence.fresh.executorReceiptSha256, sha256(fresh.receiptBytes));
  assert.equal(result.evidence.fresh.runtimeReadbackSha256, sha256(fresh.runtimeReadbackBytes));
});

test("reports resume context cap FAIL when an attested call exceeds the threshold", () => {
  const plan = testPlan(6);
  const result = measurePair(plan, {
    fresh: artifactOption(plan, "fresh", "fresh-session"),
    resume: artifactOption(plan, "resume", "resume-session"),
  });

  assert.equal(result.binding.status, "EXECUTOR-ATTESTED");
  assert.equal(result.resume.maxContextTokens, 7);
  assert.equal(result.resumeContextCap, "FAIL");
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

test("validates the pair plan digest and requires distinct option sessions", async () => {
  const { measurePair, validatePairPlan } = await import("./paired-replay.mjs");
  assert.equal(typeof measurePair, "function");
  assert.equal(typeof validatePairPlan, "function");

  const plan = createPairPlan({
    fixtureSha256: "2ec89b207ebc469831ea7e56be41145048d23b2123bcf175272c8d235815efff",
    inputPrefixSha256: "460f94059f51a5015566539ef410da60ac4c68f13e3e298fb8f9e14385fb96bb",
    readbacksSha256: "c".repeat(64),
    promptId: "msg_01a15d32-e835-7153-a81c-0c3015e3f834",
    cutoffOrdinal: 1544,
    model: "gpt-6.1-sol",
    runtimeId: "bc0835c7-a25d-4a16-a3bc-e2921d092ed9",
    threshold: 80000,
  });
  assert.equal(validatePairPlan(plan), true);
  assert.throws(() => validatePairPlan({ ...plan, threshold: 80001 }), /plan SHA-256/);

  const sameSession = {
    events: completeRunEvents(),
    sessionId: "session-1",
    rootTurnId: "root-1",
    startOrdinal: 1,
  };
  assert.throws(() => measurePair(plan, {
    fresh: sameSession,
    resume: sameSession,
  }), /distinct session IDs/);
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
