import { devices, expect, test } from "@playwright/test";
import { keyboardComposerMaxHeight } from "../packages/views/chat/components/keyboard-composer-cap";

/**
 * Self-contained layout test (CHE-3534): no app server, `page.setContent`
 * with the composer's flex structure and Tailwind caps written as plain CSS.
 * The only production code in the loop is `keyboardComposerMaxHeight`, which
 * supplies the inline cap. jsdom has no layout, so this pins what it cannot:
 * with a 40-line draft and the keyboard open, the composer stays capped and
 * scrolls inside itself, and the message list keeps most of the visible strip.
 * Runs in Chromium with an iPhone 15 Pro profile (emulation, not WebKit).
 */
test.use({ ...devices["iPhone 15 Pro"], browserName: "chromium", channel: undefined });

// Visible strip above the soft keyboard on an iPhone 15 Pro (852pt layout, ~336pt keyboard).
const VISIBLE_HEIGHT = 516;

function page(maxHeight: number | undefined): string {
  const lines = Array.from({ length: 40 }, (_, i) => `<p style="margin:0">draft line ${i + 1}</p>`).join("");
  const messages = Array.from({ length: 30 }, (_, i) => `<div style="height:40px">message ${i + 1}</div>`).join("");
  return `<!doctype html><html><style>*{box-sizing:border-box}</style><body style="margin:0">
<div id="surface" style="display:flex;flex-direction:column;height:${VISIBLE_HEIGHT}px;min-height:0">
  <div id="list" style="flex:1;min-height:0;overflow-y:auto">${messages}</div>
  <div id="composer" style="display:flex;flex-direction:column;min-height:0;max-height:50%;${maxHeight === undefined ? "" : `max-height:${maxHeight}px;`}padding-bottom:12px">
    <div id="card" style="position:relative;display:flex;flex-direction:column;min-height:64px;max-height:24rem;padding-bottom:36px;border:1px solid #ccc">
      <div id="editor" style="flex:1;min-height:0;overflow-y:auto;padding:8px 12px">${lines}</div>
    </div>
  </div>
</div></body></html>`;
}

async function measure(pg: import("@playwright/test").Page, maxHeight: number | undefined) {
  await pg.setContent(page(maxHeight));
  return pg.evaluate(() => {
    const h = (id: string) => document.getElementById(id)!.getBoundingClientRect().height;
    const list = document.getElementById("list")!;
    const editor = document.getElementById("editor")!;
    return {
      composer: h("composer"),
      list: h("list"),
      listScrolls: list.scrollHeight > list.clientHeight,
      editorScrolls: editor.scrollHeight > editor.clientHeight,
    };
  });
}

test("keyboard open: composer is capped at 40% and the message list keeps the rest", async ({ page: pg }) => {
  const m = await measure(pg, keyboardComposerMaxHeight(VISIBLE_HEIGHT));
  expect(m.composer).toBeLessThanOrEqual(VISIBLE_HEIGHT * 0.4);
  expect(m.list).toBeGreaterThanOrEqual(VISIBLE_HEIGHT * 0.55);
  expect(m.listScrolls).toBe(true);
  expect(m.editorScrolls).toBe(true);
});
