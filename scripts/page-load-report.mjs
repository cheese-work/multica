#!/usr/bin/env node
/**
 * Page-load baseline report (CHE-2976). Reads the samples that
 * `e2e/perf/page-load-baseline.spec.ts` writes and prints one median table per
 * profile.
 *
 *   node scripts/page-load-report.mjs <page-load-desktop.json> <page-load-phone.json>
 *   node scripts/page-load-report.mjs --base <base files...> --head <head files...>
 */
import { readFileSync } from "node:fs";

const METRICS = ["ttfb_ms", "lcp_ms", "long_task_total_ms", "js_transferred_bytes", "api_requests"];

export function median(values) {
  const sorted = values.filter((v) => typeof v === "number").sort((a, b) => a - b);
  if (!sorted.length) return null;
  const mid = sorted.length >> 1;
  return sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
}

/** A missing metric (e.g. no LCP candidate) is dropped, not read as zero. */
export function summarizeRoute(samples) {
  return {
    n: samples.length,
    ...Object.fromEntries(METRICS.map((m) => [m, median(samples.map((s) => s[m]))])),
  };
}

const cell = (v, digits = 0) => (v === null ? "n/a" : Number(v.toFixed(digits)));

export function renderReport(profiles) {
  return Object.entries(profiles)
    .map(([profile, routes]) => {
      const rows = Object.entries(routes).map(
        ([route, s]) =>
          `| ${route} | ${s.n} | ${cell(s.ttfb_ms)} | ${cell(s.lcp_ms)} | ${cell(s.long_task_total_ms)} | ${cell(s.js_transferred_bytes / 1024)} | ${cell(s.api_requests)} |`,
      );
      return [
        `### ${profile}`,
        "",
        "| Route | Samples | TTFB (ms) | LCP (ms) | Long-task total (ms) | JS transferred (KiB) | API requests |",
        "|---|---|---|---|---|---|---|",
        ...rows,
      ].join("\n");
    })
    .join("\n\n");
}

const signed = (v) => (v > 0 ? `+${v}` : `${v}`);
const delta = (base, head) => (base === null || head === null ? "n/a" : signed(Number((head - base).toFixed(0))));

/** Base and head medians side by side, per profile, for the two metrics this step targets. */
export function renderComparison(base, head) {
  return Object.entries(head)
    .map(([profile, routes]) => {
      const rows = Object.entries(routes).map(([route, h]) => {
        const b = base[profile]?.[route] ?? {};
        const [bApi, bLcp] = [b.api_requests ?? null, b.lcp_ms ?? null];
        return `| ${route} | ${cell(bApi)} | ${cell(h.api_requests)} | ${delta(bApi, h.api_requests)} | ${cell(bLcp)} | ${cell(h.lcp_ms)} | ${delta(bLcp, h.lcp_ms)} |`;
      });
      return [
        `### ${profile}`,
        "",
        "| Route | API requests base | API requests head | Δ | LCP base (ms) | LCP head (ms) | Δ (ms) |",
        "|---|---|---|---|---|---|---|",
        ...rows,
      ].join("\n");
    })
    .join("\n\n");
}

export const summarizeFiles = (files) => {
  const merged = Object.assign({}, ...files.map((file) => JSON.parse(readFileSync(file, "utf8")).profiles));
  return Object.fromEntries(
    Object.entries(merged).map(([p, routes]) => [
      p,
      Object.fromEntries(Object.entries(routes).map(([r, samples]) => [r, summarizeRoute(samples)])),
    ]),
  );
};

if (import.meta.url === `file://${process.argv[1]}`) {
  const args = process.argv.slice(2);
  const split = args.indexOf("--head");
  if (args[0] === "--base" && split > 0) {
    console.log(renderComparison(summarizeFiles(args.slice(1, split)), summarizeFiles(args.slice(split + 1))));
  } else {
    console.log(renderReport(summarizeFiles(args)));
  }
}
