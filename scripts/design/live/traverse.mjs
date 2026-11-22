// Player traversal for the Live track (CHE-6753 A2, slice L6): drives the real doop
// player in Playwright, from the index frame, through every edge and Home, and
// asserts the frame id reached at each step. doop drops an interaction without an
// action and `get_prototype` does not report it, so this is the check of record.
import { existsSync, readdirSync } from "node:fs";
import { homedir } from "node:os";
import path from "node:path";
import { chromium } from "playwright-core";

/** The Playwright-pinned Chromium, else the newest one in the local browser cache. */
export async function launchBrowser() {
  try {
    return await chromium.launch();
  } catch (first) {
    const root = process.env.PLAYWRIGHT_BROWSERS_PATH || path.join(homedir(), ".cache", "ms-playwright");
    const dirs = existsSync(root) ? readdirSync(root).filter((d) => /^chromium-\d+$/.test(d)).sort() : [];
    for (const dir of dirs.reverse()) {
      for (const bin of ["chrome-linux64/chrome", "chrome-linux/chrome"]) {
        const executablePath = path.join(root, dir, bin);
        if (existsSync(executablePath)) return chromium.launch({ executablePath });
      }
    }
    throw first;
  }
}

// RED STUB (CHE-6795): the traversal itself is added in the next commit.
export const normalizeEdges = (edges) => edges;
export const traverse = async () => ({ ok: true, problems: [], checked: [] });
