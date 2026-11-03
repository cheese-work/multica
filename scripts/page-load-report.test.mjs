import assert from "node:assert/strict";
import test from "node:test";
import { median, renderComparison, renderReport, summarizeRoute } from "./page-load-report.mjs";

const sample = (n) => ({
  ttfb_ms: 10 * n, lcp_ms: 100 * n, long_task_total_ms: 5 * n, js_transferred_bytes: 2048 * n, api_requests: n,
});

test("median takes the middle of an odd list and the mean of the middle pair of an even one", () => {
  assert.equal(median([5, 1, 3]), 3);
  assert.equal(median([4, 1, 3, 2]), 2.5);
  assert.equal(median([]), null);
});

test("summarizeRoute reports the median of every metric", () => {
  const out = summarizeRoute([5, 1, 3, 2, 4].map(sample));
  assert.deepEqual(out, { n: 5, ttfb_ms: 30, lcp_ms: 300, long_task_total_ms: 15, js_transferred_bytes: 6144, api_requests: 3 });
});

test("summarizeRoute keeps a missing LCP out of the median instead of reading it as zero", () => {
  const out = summarizeRoute([sample(1), { ...sample(2), lcp_ms: null }, sample(3)]);
  assert.equal(out.lcp_ms, 200);
});

test("renderReport emits one table per profile with one row per route", () => {
  const md = renderReport({
    desktop: { issues: summarizeRoute([sample(1)]) },
    phone: { issues: summarizeRoute([sample(2)]) },
  });
  assert.match(md, /### desktop\n\n\| Route \| Samples \| TTFB \(ms\)/);
  assert.match(md, /### phone/);
  assert.match(md, /\| issues \| 1 \| 10 \| 100 \| 5 \| 2 \| 1 \|/);
});

test("renderComparison puts base and head side by side with the delta for API requests and LCP", () => {
  const base = { desktop: { issues: summarizeRoute([sample(4)]), chat: summarizeRoute([sample(2)]) } };
  const head = { desktop: { issues: summarizeRoute([sample(3)]), chat: summarizeRoute([sample(2)]) } };
  const md = renderComparison(base, head);
  assert.match(md, /### desktop/);
  assert.match(md, /\| Route \| API requests base \| API requests head \| Δ \| LCP base \(ms\) \| LCP head \(ms\) \| Δ \(ms\) \|/);
  assert.match(md, /\| issues \| 4 \| 3 \| -1 \| 400 \| 300 \| -100 \|/);
  assert.match(md, /\| chat \| 2 \| 2 \| 0 \| 200 \| 200 \| 0 \|/);
});
