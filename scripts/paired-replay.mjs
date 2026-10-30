import { createHash } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const usageFields = [
  "input_tokens",
  "cached_input_tokens",
  "cache_write_input_tokens",
  "output_tokens",
  "reasoning_output_tokens",
  "total_tokens",
];

const sample = {
  sampleId: "01a15d30-8aed-70f8-9560-db5c6e096ccb",
  sessionId: "01a15c92-de33-77b3-b485-f5c04a71d10a",
  rootTurnId: "01a15d32-e6a7-7170-b7c0-bd8175baaf83",
  promptId: "msg_01a15d32-e835-7153-a81c-0c3015e3f834",
  cutoffOrdinal: 1544,
  fixtureSha256: "2ec89b207ebc469831ea7e56be41145048d23b2123bcf175272c8d235815efff",
  inputPrefixSha256: "460f94059f51a5015566539ef410da60ac4c68f13e3e298fb8f9e14385fb96bb",
  model: "gpt-6.1-sol",
  runtimeId: "bc0835c7-a25d-4a16-a3bc-e2921d092ed9",
  cliVersion: "0.155.1",
  historyMode: "paginated",
};

function fail(message) {
  throw new Error(message);
}

function isNonEmptyString(value) {
  return typeof value === "string" && value.trim().length > 0;
}

function normalizeUsage(value, label) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    fail(`${label} is missing`);
  }
  const result = {};
  for (const field of usageFields) {
    const count = value[field];
    if (!Number.isSafeInteger(count) || count < 0) {
      fail(`${label}.${field} is not a non-negative integer`);
    }
    result[field] = count;
  }
  if (result.cached_input_tokens > result.input_tokens) {
    fail(`${label}.cached_input_tokens exceeds input_tokens`);
  }
  return result;
}

function usageSnapshot(event, label) {
  return normalizeUsage(event?.payload?.info?.total_token_usage, label);
}

function sumUsage(records) {
  const totals = Object.fromEntries(usageFields.map((field) => [field, 0]));
  for (const record of records) {
    for (const field of usageFields) {
      const next = totals[field] + record.usage[field];
      if (!Number.isSafeInteger(next)) fail(`aggregate ${field} exceeds safe integer range`);
      totals[field] = next;
    }
  }
  return totals;
}

export function assertReplayActionAllowed(action) {
  const target = action?.target;
  const operation = action?.operation;
  const allowed =
    (target === "platform" && operation === "read") ||
    (target === "production-session" && operation === "read") ||
    (target === "local-sandbox" && ["read", "write"].includes(operation));
  if (!allowed) {
    fail("replay action denied by offline isolation policy");
  }
  return true;
}

export function createPairPlan({
  fixtureSha256,
  inputPrefixSha256,
  readbacksSha256,
  promptId,
  cutoffOrdinal,
  model,
  runtimeId,
  threshold,
}) {
  for (const [label, digest] of Object.entries({
    fixtureSha256,
    inputPrefixSha256,
    readbacksSha256,
  })) {
    if (typeof digest !== "string" || !/^[a-f0-9]{64}$/i.test(digest)) {
      fail(`${label} must be a SHA-256 digest`);
    }
  }
  if (!isNonEmptyString(promptId) || !isNonEmptyString(model) || !isNonEmptyString(runtimeId)) {
    fail("promptId, model, and runtimeId are required");
  }
  if (!Number.isSafeInteger(cutoffOrdinal) || cutoffOrdinal < 0) {
    fail("cutoffOrdinal must be a non-negative integer");
  }
  if (!Number.isSafeInteger(threshold) || threshold < 1) {
    fail("threshold must be a positive integer token count");
  }

  const shared = {
    fixtureSha256,
    inputPrefixSha256,
    readbacksSha256,
    promptId,
    cutoffOrdinal,
    model,
    runtimeId,
    cliVersion: sample.cliVersion,
    historyMode: sample.historyMode,
  };
  const plan = {
    schemaVersion: 1,
    sampleId: sample.sampleId,
    createdBy: "scripts/paired-replay.mjs",
    threshold,
    shared,
    options: {
      fresh: { ...shared, strategy: "fresh-session-reconstruction" },
      resume: { ...shared, strategy: "resume-and-compact", threshold },
    },
    policy: {
      platformWrites: "denied",
      productionSessionMutations: "denied",
      execution: "not-run",
    },
  };
  plan.planSha256 = digestPlan(plan);
  return plan;
}

export function measureRun(events, {
  rootTurnId,
  sessionId,
  model,
  startOrdinal,
  cliVersion,
  historyMode,
}) {
  if (!Array.isArray(events)) fail("events must be an array");
  if (!isNonEmptyString(rootTurnId) || !isNonEmptyString(sessionId) || !isNonEmptyString(model)) {
    fail("rootTurnId, sessionId, and model are required");
  }
  if (!Number.isSafeInteger(startOrdinal) || startOrdinal < -1) {
    fail("startOrdinal must be -1 or a non-negative integer");
  }

  const ordinals = new Set();
  for (const event of events) {
    if (!Number.isSafeInteger(event?.ordinal) || event.ordinal < 0) {
      fail("every event must have a non-negative ordinal");
    }
    if (ordinals.has(event.ordinal)) fail(`duplicate event ordinal ${event.ordinal}`);
    ordinals.add(event.ordinal);
  }

  const sessionMetadata = events
    .filter((event) => event.type === "session_meta")
    .map((event) => event.payload);
  const sessionIds = sessionMetadata.map((metadata) => metadata?.session_id);
  if (sessionIds.length === 0 || sessionIds.some((id) => id !== sessionId)) {
    fail("session metadata does not match the run session_id");
  }
  for (const [field, expected] of [["cli_version", cliVersion], ["history_mode", historyMode]]) {
    if (!isNonEmptyString(expected) || sessionMetadata.some((metadata) => metadata?.[field] !== expected)) {
      fail(`session metadata ${field} does not match the qualified session`);
    }
  }
  const runModels = new Set(
    events
      .filter((event) => event.type === "turn_context" && event.ordinal > startOrdinal)
      .filter((event) => event.payload?.root_turn_id === rootTurnId)
      .map((event) => event.payload.model),
  );
  if (runModels.size !== 1 || !runModels.has(model)) {
    fail("run model identity does not match the qualified plan");
  }

  const heartbeatEvents = events
    .filter((event) => event.type === "event_msg" && event.payload?.type === "token_count")
    .sort((left, right) => left.ordinal - right.ordinal);
  const baselineEvent = startOrdinal === -1
    ? null
    : heartbeatEvents.filter((event) => event.ordinal <= startOrdinal).at(-1);
  if (startOrdinal >= 0 && !baselineEvent) {
    fail("baseline token_count snapshot is missing");
  }
  const afterEvent = heartbeatEvents.filter((event) => event.ordinal > startOrdinal).at(-1);
  if (!afterEvent) fail("final token_count snapshot is missing");

  const baseline = baselineEvent
    ? usageSnapshot(baselineEvent, "baseline token_count")
    : Object.fromEntries(usageFields.map((field) => [field, 0]));
  const after = usageSnapshot(afterEvent, "final token_count");
  let previousSnapshot = baseline;
  for (const event of heartbeatEvents.filter((item) => item.ordinal > startOrdinal)) {
    const currentSnapshot = usageSnapshot(event, "token_count");
    for (const field of usageFields) {
      if (currentSnapshot[field] < previousSnapshot[field]) {
        fail(`token_count counter reset for ${field}; measurement fails closed`);
      }
    }
    previousSnapshot = currentSnapshot;
  }
  const newRecords = events
    .filter((event) => event.type === "token_usage_record" && event.ordinal > startOrdinal)
    .sort((left, right) => left.ordinal - right.ordinal);
  if (newRecords.length === 0) fail("response-linked token_usage_record signal is missing");

  const responseIds = new Set();
  const records = newRecords.map((event) => {
    const payload = event.payload;
    if (!payload || typeof payload !== "object") fail("token_usage_record payload is missing");
    if (payload.root_turn_id !== rootTurnId) fail("token_usage_record root_turn_id is unbound");
    if (payload.session_id !== sessionId) fail("token_usage_record session_id is unbound");
    for (const field of ["response_id", "thread_id", "turn_id"]) {
      if (!isNonEmptyString(payload[field])) fail(`token_usage_record ${field} is unbound`);
    }
    if (responseIds.has(payload.response_id)) {
      fail(`duplicate response_id ${payload.response_id}`);
    }
    responseIds.add(payload.response_id);
    return {
      responseId: payload.response_id,
      turnId: payload.turn_id,
      usage: normalizeUsage(payload.usage, "token_usage_record.usage"),
    };
  });

  const aggregate = sumUsage(records);
  const snapshotDelta = Object.fromEntries(
    usageFields.map((field) => [field, after[field] - baseline[field]]),
  );
  for (const field of usageFields) {
    if (!Number.isSafeInteger(snapshotDelta[field]) || snapshotDelta[field] < 0 || snapshotDelta[field] !== aggregate[field]) {
      fail(`token_usage_record snapshot delta mismatch for ${field}`);
    }
  }

  return {
    sessionId,
    rootTurnId,
    responseCount: records.length,
    perCall: records,
    aggregate,
    maxContextTokens: Math.max(...records.map((record) => record.usage.input_tokens)),
    snapshotOrdinals: {
      baseline: baselineEvent?.ordinal ?? null,
      final: afterEvent.ordinal,
    },
  };
}

function sha256(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function digestPlan(plan) {
  const payload = { ...plan };
  delete payload.planSha256;
  return sha256(Buffer.from(JSON.stringify(payload)));
}

export function validatePairPlan(plan) {
  if (!plan || typeof plan !== "object" || Array.isArray(plan) || plan.schemaVersion !== 1) {
    fail("plan schema is invalid");
  }
  if (!/^[a-f0-9]{64}$/i.test(plan.planSha256 ?? "") || digestPlan(plan) !== plan.planSha256) {
    fail("plan SHA-256 does not match its contents");
  }
  if (
    plan.sampleId !== sample.sampleId ||
    plan.createdBy !== "scripts/paired-replay.mjs" ||
    !Number.isSafeInteger(plan.threshold) ||
    plan.threshold < 1 ||
    plan.policy?.platformWrites !== "denied" ||
    plan.policy?.productionSessionMutations !== "denied" ||
    plan.policy?.execution !== "not-run"
  ) {
    fail("plan identity, threshold, or execution policy is invalid");
  }

  const qualified = {
    fixtureSha256: sample.fixtureSha256,
    inputPrefixSha256: sample.inputPrefixSha256,
    promptId: sample.promptId,
    cutoffOrdinal: sample.cutoffOrdinal,
    model: sample.model,
    runtimeId: sample.runtimeId,
    cliVersion: sample.cliVersion,
    historyMode: sample.historyMode,
  };
  for (const [field, expected] of Object.entries(qualified)) {
    if (plan.shared?.[field] !== expected) fail(`plan ${field} does not match the qualified sample`);
  }
  if (!/^[a-f0-9]{64}$/i.test(plan.shared?.readbacksSha256 ?? "")) {
    fail("plan readbacksSha256 must be a SHA-256 digest");
  }

  const optionFields = [...Object.keys(qualified), "readbacksSha256"];
  for (const optionName of ["fresh", "resume"]) {
    const option = plan.options?.[optionName];
    if (!option || optionFields.some((field) => option[field] !== plan.shared[field])) {
      fail(`plan ${optionName} option does not match shared provenance`);
    }
  }
  if (
    plan.options.fresh.strategy !== "fresh-session-reconstruction" ||
    plan.options.resume.strategy !== "resume-and-compact" ||
    plan.options.resume.threshold !== plan.threshold
  ) {
    fail("plan option strategy or threshold is invalid");
  }
  return true;
}

export function measurePair(plan, pair = {}) {
  if (!pair || typeof pair !== "object" || Array.isArray(pair)) {
    fail("fresh and resume options are required");
  }
  const { fresh, resume } = pair;
  validatePairPlan(plan);
  if (!fresh || !resume || fresh.sessionId === resume.sessionId) {
    fail("fresh and resume options require distinct session IDs");
  }

  const measureOption = (optionName, option) => {
    const logBytes = option.logBytes;
    if (!Buffer.isBuffer(logBytes) && !(logBytes instanceof Uint8Array)) {
      fail(`${optionName} log bytes are required`);
    }
    const receiptBytes = option.receiptBytes;
    if (!Buffer.isBuffer(receiptBytes) && !(receiptBytes instanceof Uint8Array)) {
      fail(`${optionName} executor receipt is required`);
    }
    const runtimeReadbackBytes = option.runtimeReadbackBytes;
    if (!Buffer.isBuffer(runtimeReadbackBytes) && !(runtimeReadbackBytes instanceof Uint8Array)) {
      fail(`${optionName} native runtime/task readback is required`);
    }

    let receipt;
    try {
      receipt = JSON.parse(Buffer.from(receiptBytes).toString("utf8"));
    } catch {
      fail(`${optionName} executor receipt is invalid JSON`);
    }
    const logSha256 = sha256(logBytes);
    const receiptSha256 = sha256(receiptBytes);
    const runtimeReadbackSha256 = sha256(runtimeReadbackBytes);
    if (!receipt || typeof receipt !== "object" || Array.isArray(receipt)) {
      fail(`${optionName} executor receipt must be an object`);
    }
    for (const [field, expected] of [
      ["schemaVersion", 1],
      ["planSha256", plan.planSha256],
      ["option", optionName],
      ["strategy", plan.options[optionName].strategy],
      ["sessionId", option.sessionId],
      ["logSha256", logSha256],
      ["runtimeId", plan.shared.runtimeId],
      ["runtimeSource", "executor-attested"],
      ["runtimeReadbackSha256", runtimeReadbackSha256],
    ]) {
      if (receipt[field] !== expected) {
        fail(`${optionName} executor receipt ${field} does not match the plan or artifact`);
      }
    }
    if (!isNonEmptyString(receipt.taskId)) {
      fail(`${optionName} executor receipt taskId is missing`);
    }
    let runtimeReadback;
    try {
      runtimeReadback = JSON.parse(Buffer.from(runtimeReadbackBytes).toString("utf8"));
    } catch {
      fail(`${optionName} native runtime/task readback is invalid JSON`);
    }
    const taskRecords = Array.isArray(runtimeReadback) ? runtimeReadback : [runtimeReadback];
    const matchingTaskRecords = taskRecords.filter((record) => record?.id === receipt.taskId);
    const taskRecord = matchingTaskRecords[0];
    if (
      matchingTaskRecords.length !== 1 ||
      taskRecord.status !== "completed" ||
      taskRecord.runtime_id !== plan.shared.runtimeId
    ) {
      fail(`${optionName} executor receipt taskId/runtimeId does not match a completed native task readback`);
    }

    const prefixBound = receipt.inputPrefixSha256 !== undefined;
    if (prefixBound) {
      if (!/^[a-f0-9]{64}$/i.test(receipt.inputPrefixSha256)) {
        fail(`${optionName} executor receipt inputPrefixSha256 is invalid`);
      }
      if (receipt.inputPrefixSha256 !== plan.shared.inputPrefixSha256) {
        fail(`${optionName} executor receipt inputPrefixSha256 does not match the plan`);
      }
    }
    const thresholdBound = receipt.threshold !== undefined;
    if (thresholdBound) {
      if (!Number.isSafeInteger(receipt.threshold) || receipt.threshold < 1) {
        fail(`${optionName} executor receipt threshold is invalid`);
      }
      if (receipt.threshold !== plan.threshold) {
        fail(`${optionName} executor receipt threshold does not match the plan`);
      }
    }

    const events = Buffer.from(logBytes).toString("utf8").split(/\r?\n/).filter(Boolean).map((line, index) => {
      try {
        return JSON.parse(line);
      } catch {
        fail(`${optionName} log contains invalid JSON on line ${index + 1}`);
      }
    });
    return {
      result: measureRun(events, {
        rootTurnId: option.rootTurnId,
        sessionId: option.sessionId,
        model: plan.shared.model,
        startOrdinal: option.startOrdinal,
        cliVersion: plan.shared.cliVersion,
        historyMode: plan.shared.historyMode,
      }),
      prefixBound,
      thresholdBound,
      evidence: {
        logSha256,
        executorReceiptSha256: receiptSha256,
        runtimeReadbackSha256,
        taskId: receipt.taskId,
        runtimeSource: receipt.runtimeSource,
      },
    };
  };

  const freshMeasurement = measureOption("fresh", fresh);
  const resumeMeasurement = measureOption("resume", resume);
  const prefixBound = freshMeasurement.prefixBound && resumeMeasurement.prefixBound;
  const resumeThresholdAttested = resumeMeasurement.thresholdBound;
  const bindingAttested = prefixBound && resumeThresholdAttested;
  return {
    fresh: freshMeasurement.result,
    resume: resumeMeasurement.result,
    binding: {
      status: bindingAttested ? "EXECUTOR-ATTESTED" : "NOT-VERIFIED",
      inputPrefix: prefixBound ? "EXECUTOR-ATTESTED" : "NOT-VERIFIED",
      resumeThreshold: resumeThresholdAttested ? "EXECUTOR-ATTESTED" : "NOT-VERIFIED",
      inputPrefixSha256: plan.shared.inputPrefixSha256,
      threshold: plan.threshold,
    },
    resumeContextCap: bindingAttested
      ? (resumeMeasurement.result.maxContextTokens <= plan.threshold ? "PASS" : "FAIL")
      : "NOT-VERIFIED",
    pairedEquivalence: "NOT-CLAIMED",
    evidence: {
      fresh: freshMeasurement.evidence,
      resume: resumeMeasurement.evidence,
    },
  };
}

function prefixAtOrdinal(bytes, cutoffOrdinal) {
  const lines = bytes.toString("utf8").split("\n");
  const prefixLines = [];
  for (const line of lines) {
    if (!line) continue;
    const event = JSON.parse(line);
    if (event.ordinal > cutoffOrdinal) break;
    prefixLines.push(line);
  }
  if (prefixLines.length === 0 || prefixLines.at(-1) === "") {
    fail("fixture does not contain the qualified prefix");
  }
  const prefix = Buffer.from(`${prefixLines.join("\n")}\n`);
  const events = prefixLines.map((line) => JSON.parse(line));
  if (events.at(-1)?.ordinal !== cutoffOrdinal) {
    fail(`fixture cutoff ordinal ${cutoffOrdinal} is missing`);
  }
  return { prefix, events };
}

async function buildSamplePlan({ fixturePath, readbacksPath, threshold }) {
  const fixtureBytes = await readFile(fixturePath);
  if (sha256(fixtureBytes) !== sample.fixtureSha256) {
    fail("fixture SHA-256 does not match the qualified source");
  }
  const { prefix, events } = prefixAtOrdinal(fixtureBytes, sample.cutoffOrdinal);
  if (sha256(prefix) !== sample.inputPrefixSha256) {
    fail("fixture input-prefix SHA-256 does not match the qualified source");
  }
  const prompt = events.find((event) => event.ordinal === sample.cutoffOrdinal);
  if (prompt?.type !== "response_item" || prompt.payload?.id !== sample.promptId) {
    fail("fixture cutoff is not the qualified incoming prompt");
  }
  const session = events.find((event) => event.type === "session_meta")?.payload;
  if (
    session?.session_id !== sample.sessionId ||
    session?.cli_version !== sample.cliVersion ||
    session?.history_mode !== sample.historyMode
  ) {
    fail("fixture session metadata does not match the qualified runtime");
  }
  const models = new Set(
    events
      .filter((event) => event.type === "turn_context" && event.payload?.root_turn_id === sample.rootTurnId)
      .map((event) => event.payload.model),
  );
  if (models.size !== 1 || !models.has(sample.model)) {
    fail("fixture model identity does not match the qualified source");
  }

  const readbacks = await readFile(readbacksPath);
  if (readbacks.length === 0) fail("Verification Invariant readbacks are empty");
  return createPairPlan({
    fixtureSha256: sha256(fixtureBytes),
    inputPrefixSha256: sha256(prefix),
    readbacksSha256: sha256(readbacks),
    promptId: sample.promptId,
    cutoffOrdinal: sample.cutoffOrdinal,
    model: sample.model,
    runtimeId: sample.runtimeId,
    threshold,
  });
}

function parseFlags(args) {
  const flags = new Map();
  for (let index = 0; index < args.length; index += 2) {
    const key = args[index];
    const value = args[index + 1];
    if (!key?.startsWith("--") || value === undefined) {
      fail("flags must use --name value pairs");
    }
    if (flags.has(key)) fail(`duplicate flag ${key}`);
    flags.set(key, value);
  }
  return flags;
}

function requiredFlag(flags, name) {
  const value = flags.get(name);
  if (!isNonEmptyString(value)) fail(`${name} is required`);
  return value;
}

function integerFlag(flags, name, minimum) {
  const value = Number(requiredFlag(flags, name));
  if (!Number.isSafeInteger(value) || value < minimum) {
    fail(`${name} must be an integer >= ${minimum}`);
  }
  return value;
}

function assertOutputIsNotInput(outputPath, inputPaths) {
  const output = path.resolve(outputPath);
  if (inputPaths.some((inputPath) => path.resolve(inputPath) === output)) {
    fail("output path must not overwrite an input artifact");
  }
}

async function runCli(args) {
  const [command, ...rawFlags] = args;
  const flags = parseFlags(rawFlags);
  if (command === "plan") {
    const fixturePath = requiredFlag(flags, "--fixture");
    const readbacksPath = requiredFlag(flags, "--readbacks");
    const outputPath = requiredFlag(flags, "--output");
    assertOutputIsNotInput(outputPath, [fixturePath, readbacksPath]);
    const plan = await buildSamplePlan({
      fixturePath,
      readbacksPath,
      threshold: integerFlag(flags, "--threshold", 1),
    });
    await writeFile(outputPath, `${JSON.stringify(plan, null, 2)}\n`, { flag: "wx" });
    process.stdout.write(`Wrote metadata-only plan: ${outputPath}\n`);
    return;
  }
  if (command === "measure") {
    const planPath = requiredFlag(flags, "--plan");
    const freshPath = requiredFlag(flags, "--fresh-log");
    const freshReceiptPath = requiredFlag(flags, "--fresh-receipt");
    const freshReadbackPath = requiredFlag(flags, "--fresh-runtime-readback");
    const resumePath = requiredFlag(flags, "--resume-log");
    const resumeReceiptPath = requiredFlag(flags, "--resume-receipt");
    const resumeReadbackPath = requiredFlag(flags, "--resume-runtime-readback");
    const outputPath = requiredFlag(flags, "--output");
    assertOutputIsNotInput(outputPath, [
      planPath,
      freshPath,
      freshReceiptPath,
      freshReadbackPath,
      resumePath,
      resumeReceiptPath,
      resumeReadbackPath,
    ]);
    const plan = JSON.parse(await readFile(planPath, "utf8"));
    const afterValues = measurePair(plan, {
      fresh: {
        logBytes: await readFile(freshPath),
        receiptBytes: await readFile(freshReceiptPath),
        runtimeReadbackBytes: await readFile(freshReadbackPath),
        sessionId: requiredFlag(flags, "--fresh-session-id"),
        rootTurnId: requiredFlag(flags, "--fresh-root-turn-id"),
        startOrdinal: integerFlag(flags, "--fresh-start-ordinal", -1),
      },
      resume: {
        logBytes: await readFile(resumePath),
        receiptBytes: await readFile(resumeReceiptPath),
        runtimeReadbackBytes: await readFile(resumeReadbackPath),
        sessionId: requiredFlag(flags, "--resume-session-id"),
        rootTurnId: requiredFlag(flags, "--resume-root-turn-id"),
        startOrdinal: integerFlag(flags, "--resume-start-ordinal", -1),
      },
    });
    const report = {
      schemaVersion: 1,
      sampleId: plan.sampleId,
      planSha256: plan.planSha256,
      provenance: { ...plan.shared, runtimeSource: "executor-attested" },
      threshold: plan.threshold,
      afterValues,
      digestSemantics: "SHA-256 digests provide integrity, not authenticity",
      acceptance: "NOT-RUN: offline instrumentation does not accept CHE-1931",
    };
    await writeFile(outputPath, `${JSON.stringify(report, null, 2)}\n`, { flag: "wx" });
    process.stdout.write(`Wrote response-linked usage report: ${outputPath}\n`);
    return;
  }
  fail("usage: node scripts/paired-replay.mjs <plan|measure> --name value ...");
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  runCli(process.argv.slice(2)).catch((error) => {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  });
}
