import type { Page } from "@playwright/test";

/**
 * Scroll-anchor meter (CHE-3231, A0).
 *
 * Every animation frame it reads the viewport offset of each visible row. Between
 * two frames, the reader's anchor is the topmost row both frames show. The
 * anchor moves unasked when its offset changes by more than the scroll the
 * caller commanded in between (none, unless `scrollBy` ran). That is the
 * "jump" a reader sees as the list sliding under them; browser or Virtuoso
 * scroll corrections that cancel a layout shift correctly count as no jump.
 */

/** What the meter reports for one scenario. All deltas are in CSS pixels. */
export interface AnchorSummary {
  /** Frame transitions compared. */
  frames: number;
  /** Transitions where the anchor moved unasked by more than `JUMP_THRESHOLD_PX`. */
  jump_count: number;
  max_delta_px: number;
  /** Transitions with no visible row in common: the reader's place was lost outright. */
  lost_frames: number;
}

export interface AnchorMeterOptions {
  /** CSS selector of the scroll container the reader looks through. */
  scroller: string;
  /** CSS selector of the rows that carry an id. */
  rows: string;
  /** Attribute holding the row id; `id` for DOM ids. */
  idAttr: string;
}

export interface AnchorMeter {
  start(): Promise<void>;
  /** Scrolls the container by `px` over `frames` frames, telling the meter it was asked to. */
  scrollBy(px: number, frames: number): Promise<void>;
  /** Stops sampling and summarizes. */
  stop(): Promise<AnchorSummary>;
}

export const JUMP_THRESHOLD_PX = 1;

interface Page_ {
  __anchorMeter?: {
    summary(): AnchorSummary;
    stop(): void;
    scrollBy(px: number, frames: number): Promise<void>;
  };
}

export function createAnchorMeter(page: Page, options: AnchorMeterOptions): AnchorMeter {
  return {
    start: () =>
      page.evaluate(({ scroller, rows, idAttr, threshold }) => {
        const root = document.querySelector<HTMLElement>(scroller);
        if (!root) throw new Error(`scroll container not found: ${scroller}`);
        const w = window as unknown as Page_;
        w.__anchorMeter?.stop();

        const summary: AnchorSummary = { frames: 0, jump_count: 0, max_delta_px: 0, lost_frames: 0 };
        let previous: Map<string, number> | null = null;
        let commanded = 0;
        let running = true;

        const visibleOffsets = () => {
          const box = root.getBoundingClientRect();
          const found = new Map<string, number>();
          for (const row of root.querySelectorAll<HTMLElement>(rows)) {
            const id = row.getAttribute(idAttr);
            const top = row.getBoundingClientRect().top - box.top;
            if (id && top + row.offsetHeight > 0 && top < root.clientHeight) found.set(id, top);
          }
          return found;
        };

        const sample = () => {
          if (!running) return;
          const current = visibleOffsets();
          if (previous) {
            let anchor: string | null = null;
            let first: string | null = null;
            for (const [id, top] of previous) {
              if (first === null || top < previous.get(first)!) first = id;
              if (current.has(id) && (anchor === null || top < previous.get(anchor)!)) anchor = id;
            }
            summary.frames += 1;
            // The row the reader was on is gone with no scroll to explain it.
            const lostFirst = first !== null && first !== anchor && commanded === 0;
            if (anchor === null) {
              summary.lost_frames += 1;
              summary.jump_count += 1;
              summary.max_delta_px = Math.max(summary.max_delta_px, root.clientHeight);
            } else {
              // Scrolling by `commanded` moves a row by -commanded; anything else is a jump.
              const moved = current.get(anchor)! - previous.get(anchor)! + commanded;
              const delta = Math.abs(moved);
              if (delta > threshold || lostFirst) summary.jump_count += 1;
              summary.max_delta_px = Math.max(summary.max_delta_px, delta);
            }
          }
          previous = current;
          commanded = 0;
          requestAnimationFrame(sample);
        };

        w.__anchorMeter = {
          summary: () => ({ ...summary, max_delta_px: Math.round(summary.max_delta_px * 100) / 100 }),
          stop: () => { running = false; },
          scrollBy: (px, frames) => new Promise<void>((done) => {
            let left = frames;
            const step = () => {
              if (left-- <= 0 || !running) return done();
              const before = root.scrollTop;
              root.scrollTop = before + px / frames;
              commanded += root.scrollTop - before;
              requestAnimationFrame(step);
            };
            requestAnimationFrame(step);
          }),
        };
        requestAnimationFrame(sample);
      }, { ...options, threshold: JUMP_THRESHOLD_PX }),
    scrollBy: (px, frames) =>
      page.evaluate(({ px, frames }) => (window as unknown as Page_).__anchorMeter!.scrollBy(px, frames), { px, frames }),
    stop: () =>
      page.evaluate(() => {
        const meter = (window as unknown as Page_).__anchorMeter!;
        meter.stop();
        return meter.summary();
      }),
  };
}

/** Assert mode: no unasked movement of the reader's anchor. */
export function assertStable(summary: AnchorSummary): void {
  if (summary.jump_count !== 0 || summary.max_delta_px > JUMP_THRESHOLD_PX) {
    throw new Error(`scroll anchor moved: ${JSON.stringify(summary)}`);
  }
}
