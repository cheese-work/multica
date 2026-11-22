import type { Page, Route } from "@playwright/test";

/**
 * Fault presets for the Live route manifest (CHE-6753 A1, `fault` entry field).
 *
 * A preset overrides ONE API response inside the page's own browser context. It
 * adds no data: the request never reaches the server for the matched pattern, and
 * nothing is written to the seeded instance.
 */
export const FAULT_PRESETS = ["http-500", "http-403", "http-404", "hold"] as const;
export type FaultPreset = (typeof FAULT_PRESETS)[number];

const FAULT_STATUS: Record<Exclude<FaultPreset, "hold">, { status: number; error: string }> = {
  "http-500": { status: 500, error: "internal server error" },
  "http-403": { status: 403, error: "forbidden" },
  "http-404": { status: 404, error: "not found" },
};

export interface FaultHandle {
  /** Number of requests the fault has answered or is holding. */
  hits(): number;
  /** `hold` only: let every held request through to the server. No effect on the other presets. */
  release(): Promise<void>;
  /** Remove the override; later requests reach the server again. */
  clear(): Promise<void>;
}

export function isFaultPreset(value: unknown): value is FaultPreset {
  return typeof value === "string" && (FAULT_PRESETS as readonly string[]).includes(value);
}

/**
 * Apply `preset` to every `fetch`/`xhr` request of `page` whose URL matches `pattern`
 * (a Playwright glob or a RegExp such as /\/api\/issues\/table\/rows/). Page navigations and
 * assets never match, so a pattern cannot break the document itself.
 */
export async function applyFault(
  page: Page,
  preset: FaultPreset,
  pattern: string | RegExp,
): Promise<FaultHandle> {
  if (!isFaultPreset(preset)) {
    throw new Error(`unknown fault preset "${String(preset)}"; expected one of ${FAULT_PRESETS.join(", ")}`);
  }
  let hits = 0;
  const held: Route[] = [];

  const handler = async (route: Route) => {
    const type = route.request().resourceType();
    if (type !== "fetch" && type !== "xhr") {
      await route.fallback();
      return;
    }
    hits += 1;
    if (preset === "hold") {
      held.push(route);
      return;
    }
    const { status, error } = FAULT_STATUS[preset];
    await route.fulfill({
      status,
      contentType: "application/json",
      body: JSON.stringify({ error }),
    });
  };

  await page.route(pattern, handler);

  return {
    hits: () => hits,
    async release() {
      for (const route of held.splice(0)) await route.continue().catch(() => {});
    },
    async clear() {
      await page.unroute(pattern, handler);
      for (const route of held.splice(0)) await route.continue().catch(() => {});
    },
  };
}
