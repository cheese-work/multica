import assert from "node:assert/strict";
import { test } from "node:test";
import { documentFasterThanLink, responseDelayMs } from "../e2e/perf/phone-network.mjs";

const phone = { rttMs: 150, downBytesPerSec: 200_000 };

test("an empty body waits one RTT", () => {
  assert.equal(responseDelayMs(phone, 0), 150);
});

test("a body also waits its transfer time at the profile bandwidth", () => {
  // 400 kB at 200 kB/s = 2000 ms, plus the 150 ms RTT.
  assert.equal(responseDelayMs(phone, 400_000), 2150);
});

test("a document that arrived faster than the link allows was not throttled", () => {
  // 105 kB at 200 kB/s needs at least 525 ms; 36 ms is the unthrottled time.
  assert.equal(documentFasterThanLink(phone, 105_000, 36), true);
});

test("a document that took at least its transfer time was throttled", () => {
  assert.equal(documentFasterThanLink(phone, 105_000, 525), false);
  assert.equal(documentFasterThanLink(phone, 105_000, 2150), false);
});
