import "./env";

import { expect, test, type Page } from "@playwright/test";
import pg from "pg";
import { createTestApi } from "./helpers";

// Where setup parks the target message: below the 80 px `onScreen` floor, and
// far enough down that the two rows above it end above the viewport top.
const PARKED_TARGET_TOP = 110;

type Dm = { slug: string; sessionId: string; token: string; linked: { id: string; identifier: string; title: string } };

// Seeds a DM with 80 messages (message 40 links an issue and renders inline <strong>), opens it and scrolls
// up until message 40 is on screen. The reader is then still.
async function withScrolledDm(page: Page, run: (dm: Dm) => Promise<void>) {
  const databaseUrl = process.env.DATABASE_URL;
  if (!databaseUrl) throw new Error("DATABASE_URL missing");
  const api = await createTestApi();
  const database = new pg.Client(databaseUrl);
  await database.connect();
  let sessionId: string | undefined;
  let agentId: string | undefined;
  let runtimeId: string | undefined;
  try {
    const workspace = (await api.getWorkspaces())[0]!;
    api.setWorkspaceId(workspace.id);
    api.setWorkspaceSlug(workspace.slug);
    const linked = await api.createIssue(`DM scroll linked ${Date.now()}`);
    const user = await database.query<{ id: string }>(`SELECT id::text FROM "user" WHERE email = $1`, [api.getEmail()]);
    const runtime = await database.query<{ id: string }>(
      `INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, last_seen_at)
       VALUES ($1, 'Scroll E2E', 'cloud', 'e2e_scroll', 'online', 'Scroll E2E', '{}'::jsonb, now()) RETURNING id::text`,
      [workspace.id],
    );
    runtimeId = runtime.rows[0]!.id;
    const agent = await database.query<{ id: string }>(
      `INSERT INTO agent (workspace_id, name, description, instructions, runtime_mode, runtime_config, runtime_id, visibility, max_concurrent_tasks, owner_id)
       VALUES ($1, 'Scroll Copilot', '', '', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3) RETURNING id::text`,
      [workspace.id, runtimeId, user.rows[0]!.id],
    );
    agentId = agent.rows[0]!.id;
    const session = await database.query<{ id: string }>(
      `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, status, explicitly_created_at)
       VALUES ($1, $2, $3, 'Scroll restore', 'active', now()) RETURNING id::text`,
      [workspace.id, agentId, user.rows[0]!.id],
    );
    sessionId = session.rows[0]!.id;
    const link = `[${linked.identifier}](mention://issue/${linked.id})`;
    await database.query(
      `INSERT INTO chat_message (chat_session_id, role, content, created_at)
       SELECT $1, 'assistant',
         CASE WHEN step = 40 THEN 'Target message 40: see ' || $2 || ' and **bold lead** words' || E'\n\n' || repeat('filler text ', 30) || ' then **late bold** end'
              ELSE 'Message ' || step || ': ' || repeat('filler text ', 30) END,
         now() - (80 - step) * interval '1 second'
       FROM generate_series(1, 80) AS step`,
      [sessionId, link],
    );

    await page.addInitScript((authToken) => {
      localStorage.setItem("multica_token", authToken);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, api.getToken()!);
    await page.goto(`/${workspace.slug}/chat?session=${sessionId}`, { waitUntil: "domcontentloaded" });
    await expect(page.getByText("Message 80:")).toBeVisible({ timeout: 30_000 });

    for (let i = 0; i < 120 && !(await onScreen(page)); i++) {
      await dragUp(page, 300);
      await page.waitForTimeout(80);
    }
    expect(await onScreen(page)).toBe(true);
    // The drag stride lands the target at a different offset for each seed, so
    // park it: the two rows above it then sit wholly above the viewport top (a
    // row that straddles it is the reader's row, and growing it is growth below
    // the reader, not above).
    await dragUp(page, -((await targetTop(page)) - PARKED_TARGET_TOP));
    await expect.poll(() => targetTop(page)).toBeCloseTo(PARKED_TARGET_TOP, -1);
    await settle(page);
    await run({ slug: workspace.slug, sessionId, token: api.getToken()!, linked });
  } finally {
    if (sessionId) {
      await database.query(`DELETE FROM chat_message WHERE chat_session_id = $1`, [sessionId]);
      await database.query(`DELETE FROM chat_session WHERE id = $1`, [sessionId]);
    }
    if (agentId) await database.query(`DELETE FROM agent WHERE id = $1`, [agentId]);
    if (runtimeId) await database.query(`DELETE FROM agent_runtime WHERE id = $1`, [runtimeId]);
    await database.end();
    await api.cleanup();
  }
}

// iPhone: scroll up in a DM, open an issue linked from it, go Back. The DM must
// come back at the same place, and must not move while the reader is still.
test.describe("DM scroll position on iPhone (CHE-4638)", () => {
  test("Back from a linked issue restores the DM offset and the offset holds", async ({ page }, testInfo) => {
    await withScrolledDm(page, async ({ linked }) => {
      const before = await targetTop(page);
      await testInfo.attach("before", { body: await page.screenshot(), contentType: "image/png" });

      await page.getByRole("link", { name: linked.identifier }).first().click();
      await expect(page.getByRole("button", { name: linked.title, exact: true })).toBeVisible({ timeout: 30_000 });
      await page.goBack();
      await expect.poll(() => onScreen(page), { timeout: 15_000 }).toBe(true);
      await settle(page);
      const after = await targetTop(page);
      await testInfo.attach("after", { body: await page.screenshot(), contentType: "image/png" });
      expect(Math.abs(after - before)).toBeLessThanOrEqual(2);

      // Still reader: the offset must not drift on its own.
      await page.waitForTimeout(3000);
      expect(Math.abs((await targetTop(page)) - after)).toBeLessThanOrEqual(2);
    });
  });

  test("after Back, a row above that grows late does not move the restored message", async ({ page }) => {
    await withScrolledDm(page, async ({ linked }) => {
      const before = await targetTop(page);
      await page.getByRole("link", { name: linked.identifier }).first().click();
      await expect(page.getByRole("button", { name: linked.title, exact: true })).toBeVisible({ timeout: 30_000 });
      await page.goBack();
      await expect.poll(() => onScreen(page), { timeout: 15_000 }).toBe(true);
      await settle(page);

      // No wheel, touch or key input since Back: the reader is still.
      await growRow(page, -2);
      await page.waitForTimeout(1500);
      expect(Math.abs((await targetTop(page)) - before)).toBeLessThanOrEqual(2);
    });
  });

  test("right after Back, a row above that grows before the list settles does not move the restored message", async ({ page }) => {
    await withScrolledDm(page, async ({ linked }) => {
      const before = await targetTop(page);
      await page.getByRole("link", { name: linked.identifier }).first().click();
      await expect(page.getByRole("button", { name: linked.title, exact: true })).toBeVisible({ timeout: 30_000 });
      // Grow the row one frame after the restored list first renders it: after the
      // row was measured, inside the restore window, with no input from the reader.
      await page.evaluate(() => {
        const w = window as unknown as { __grownAt?: number };
        const watch = new MutationObserver(() => {
          const rows = [...document.querySelectorAll<HTMLElement>("[data-message-id]")];
          const i = rows.findIndex((n) => n.textContent?.includes("Target message 40"));
          if (i < 2) return;
          watch.disconnect();
          const row = rows[i - 2]!;
          // Second frame: the list has measured the row once, so this is late growth.
          requestAnimationFrame(() =>
            requestAnimationFrame(() => {
              row.style.minHeight = "260px";
              w.__grownAt = performance.now();
            }),
          );
        });
        watch.observe(document.body, { childList: true, subtree: true });
      });
      await page.goBack();
      await expect
        .poll(() => page.evaluate(() => (window as unknown as { __grownAt?: number }).__grownAt), { timeout: 15_000 })
        .toBeDefined();
      await settle(page);
      await page.waitForTimeout(1500);
      expect(Math.abs((await targetTop(page)) - before)).toBeLessThanOrEqual(2);
    });
  });

  test("the offset holds while a message arrives and rows above and below resize", async ({ page }, testInfo) => {
    await withScrolledDm(page, async ({ sessionId, token }) => {
      const before = await targetTop(page);

      // A new message lands at the live end while the reader is scrolled up.
      const api = process.env.NEXT_PUBLIC_API_URL || `http://localhost:${process.env.PORT || "8080"}`;
      const slug = new URL(page.url()).pathname.split("/")[1]!;
      const sent = await fetch(`${api}/api/chat/sessions/${sessionId}/messages`, {
        method: "POST",
        headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}`, "X-Workspace-Slug": slug },
        body: JSON.stringify({ content: "Late arrival from the reader" }),
      });
      expect(sent.ok).toBe(true);

      const drift = async () => Math.abs((await targetTop(page)) - before);
      await page.waitForTimeout(2000);
      const afterMessage = await drift();

      // Late height changes, as an image or a code block finishing layout does.
      await growRow(page, 2);
      await page.waitForTimeout(1500);
      const afterBelow = await drift();
      await growRow(page, -2);
      await page.waitForTimeout(1500);
      const afterAbove = await drift();

      await testInfo.attach("after-late-content", { body: await page.screenshot(), contentType: "image/png" });
      expect(afterMessage).toBeLessThanOrEqual(2);
      expect(afterBelow).toBeLessThanOrEqual(2);
      expect(afterAbove).toBeLessThanOrEqual(2);
    });
  });
});

// Sol's invariant (CHE-4638 design review): with no scroll input, the reading
// point (the row at the viewport top, as an offset from it) stays within 2 px
// on every animation frame from the first revealed restored frame, including
// the first second after Back. `rowOffset` is relative to the reading row.
type Growth = { rowOffset: number; afterMs: number };
type Frame = { t: number; id: string; top: number };
const READING_ROW = `(() => {
  const root = document.querySelector("[data-tab-scroll-root]");
  const rootTop = root.getBoundingClientRect().top;
  const rows = [...document.querySelectorAll("[data-message-id]")];
  const i = rows.findIndex((n) => n.getBoundingClientRect().bottom > rootTop + 1);
  return { rows, i, rootTop };
})()`;

async function readingPoint(page: Page) {
  return page.evaluate(`(() => { const { rows, i, rootTop } = ${READING_ROW}; return { id: rows[i].dataset.messageId, top: rows[i].getBoundingClientRect().top - rootTop }; })()`) as Promise<{ id: string; top: number }>;
}

async function backAndSample(page: Page, linked: Dm["linked"], growths: Growth[]) {
  await page.getByRole("link", { name: linked.identifier }).first().click();
  await expect(page.getByRole("button", { name: linked.title, exact: true })).toBeVisible({ timeout: 30_000 });
  await page.evaluate(`((plan) => {
    const w = window;
    w.__frames = [];
    w.__done = false;
    let t0 = 0;
    const loop = (now) => {
      const { rows, i, rootTop } = ${READING_ROW};
      const target = rows.find((n) => n.textContent && n.textContent.includes("Target message 40"));
      if (target && getComputedStyle(target).visibility !== "hidden" && i >= 0) {
        if (!t0) t0 = now;
        w.__frames.push({ t: now - t0, id: rows[i].dataset.messageId, top: rows[i].getBoundingClientRect().top - rootTop });
        // Growth is applied inside the frame, after this frame's sample: the
        // resize observers run before paint, so the next sample reads what the
        // reader saw. A timer would land between frames and read a state that
        // is never painted.
        if (w.__frames.length >= 3) {
          for (const g of plan) {
            if (g.done || now - t0 < g.afterMs) continue;
            g.done = true;
            const r = rows[i + g.rowOffset];
            if (r) r.style.minHeight = "260px";
          }
        }
        if (now - t0 > 2600) { w.__done = true; return; }
      }
      requestAnimationFrame(loop);
    };
    requestAnimationFrame(loop);
  })(${JSON.stringify(growths)})`);
  await page.goBack();
  await expect.poll(() => page.evaluate(() => (window as unknown as { __done: boolean }).__done), { timeout: 20_000 }).toBe(true);
  return page.evaluate(() => (window as unknown as { __frames: Frame[] }).__frames);
}

// Frames where the reading point moved, for a readable failure message.
function changes(frames: Frame[]) {
  return frames
    .filter((f, i) => i === 0 || f.id !== frames[i - 1]!.id || Math.abs(f.top - frames[i - 1]!.top) > 0.5)
    .map((f) => `${Math.round(f.t)}ms:${f.id.slice(0, 4)}@${Math.round(f.top)}`)
    .join(" ");
}

function maxDrift(frames: Frame[], expected: { id: string; top: number }) {
  return Math.max(...frames.map((f) => (f.id === expected.id ? Math.abs(f.top - expected.top) : Infinity)));
}

test.describe("DM reading point per frame after Back (CHE-4638 design)", () => {
  for (const [name, growths] of [
    ["a row wholly above grows in the first second", [{ rowOffset: -2, afterMs: 120 }]],
    ["several rows above grow at different times", [{ rowOffset: -2, afterMs: 60 }, { rowOffset: -3, afterMs: 400 }, { rowOffset: -1, afterMs: 900 }]],
    ["a row wholly below grows in the first second", [{ rowOffset: 2, afterMs: 120 }]],
  ] as [string, Growth[]][]) {
    test(`${name}: no frame drifts more than 2 px`, async ({ page }, testInfo) => {
      await withScrolledDm(page, async ({ linked }) => {
        const before = await readingPoint(page);
        const frames = await backAndSample(page, linked, growths);
        await testInfo.attach("frames", { body: JSON.stringify(frames), contentType: "application/json" });
        expect(frames.length).toBeGreaterThan(10);
        expect(maxDrift(frames, before), `expected ${before.id.slice(0, 4)}@${Math.round(before.top)}; changes: ${changes(frames)}`).toBeLessThanOrEqual(2);
      });
    });
  }

  test("a tall message partly above the viewport that grows below the reader does not shift it", async ({ page }) => {
    await withScrolledDm(page, async () => {
      // Park a row so its top is 30 px above the viewport top, then append height to it.
      const result = await page.evaluate(async () => {
        const root = document.querySelector<HTMLElement>("[data-tab-scroll-root]")!;
        const row = [...document.querySelectorAll<HTMLElement>("[data-message-id]")].find((n) =>
          n.textContent?.includes("Target message 40"),
        )!;
        const rootTop = root.getBoundingClientRect().top;
        root.dispatchEvent(new WheelEvent("wheel", { deltaY: 1, bubbles: true }));
        root.scrollTop += row.getBoundingClientRect().top - rootTop + 30;
        await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
        const before = row.getBoundingClientRect().top - rootTop;
        row.style.minHeight = "420px";
        const samples: number[] = [];
        for (let i = 0; i < 20; i++) {
          await new Promise((r) => requestAnimationFrame(r));
          samples.push(row.getBoundingClientRect().top - rootTop);
        }
        return { before, samples };
      });
      expect(result.before).toBeLessThan(0);
      expect(Math.max(...result.samples.map((s) => Math.abs(s - result.before)))).toBeLessThanOrEqual(2);
    });
  });
});

test.describe("DM reading point inside one tall message (CHE-4638 design)", () => {
  test("a block above the visible text grows inside the same message: no frame moves the text", async ({ page }) => {
    await withScrolledDm(page, async () => {
      // Park the target message so its first paragraph ends 8 px above the
      // viewport top; its second paragraph is the text the reader is on.
      const result = await page.evaluate(async () => {
        const root = document.querySelector<HTMLElement>("[data-tab-scroll-root]")!;
        const row = [...document.querySelectorAll<HTMLElement>("[data-message-id]")].find((n) =>
          n.textContent?.includes("Target message 40"),
        )!;
        const [first, marker] = [...row.querySelectorAll("p")] as HTMLElement[];
        if (!first || !marker) throw new Error("target message needs two paragraphs");
        const rootTop = root.getBoundingClientRect().top;
        root.dispatchEvent(new WheelEvent("wheel", { deltaY: 1, bubbles: true }));
        root.scrollTop += first.getBoundingClientRect().bottom - rootTop + 8;
        await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
        const markerTop = () => marker.getBoundingClientRect().top - rootTop;
        const before = markerTop();
        first.style.minHeight = `${first.getBoundingClientRect().height + 120}px`;
        const samples: number[] = [];
        for (let i = 0; i < 20; i++) {
          await new Promise((r) => requestAnimationFrame(r));
          samples.push(markerTop());
        }
        return { before, samples };
      });
      expect(result.before).toBeLessThan(40);
      expect(Math.max(...result.samples.map((s) => Math.abs(s - result.before)))).toBeLessThanOrEqual(2);
    });
  });

  test("plain text below the reader grows before a later inline element: no frame moves the text", async ({ page }) => {
    await withScrolledDm(page, async () => {
      // Make the second paragraph plain text followed by <strong>. Park the
      // viewport top inside the plain text; the text then grows. Only the
      // <strong> moves, so the reader's text must stay where it is.
      const result = await page.evaluate(async () => {
        const root = document.querySelector<HTMLElement>("[data-tab-scroll-root]")!;
        const row = [...document.querySelectorAll<HTMLElement>("[data-message-id]")].find((n) =>
          n.textContent?.includes("Target message 40"),
        )!;
        const paragraph = row.querySelectorAll("p")[1]!;
        const text = paragraph.firstChild as Text;
        if (text.nodeType !== Node.TEXT_NODE) throw new Error("paragraph must start with plain text");
        paragraph.append(Object.assign(document.createElement("strong"), { textContent: "late emphasis" }));
        const rootTop = root.getBoundingClientRect().top;
        root.dispatchEvent(new WheelEvent("wheel", { deltaY: 1, bubbles: true }));
        root.scrollTop += paragraph.getBoundingClientRect().top - rootTop + 8;
        await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)));
        // The first line of the plain text, measured without adding an element.
        const textTop = () => {
          const range = document.createRange();
          range.setStart(text, 0);
          range.setEnd(text, 1);
          return range.getBoundingClientRect().top - rootTop;
        };
        const before = textTop();
        text.data += " more words".repeat(120);
        const samples: number[] = [];
        for (let i = 0; i < 20; i++) {
          await new Promise((r) => requestAnimationFrame(r));
          samples.push(textTop());
        }
        return { before, samples };
      });
      expect(Math.max(...result.samples.map((s) => Math.abs(s - result.before)))).toBeLessThanOrEqual(2);
    });
  });
});

// One long plain text node that straddles the viewport top. The reader's
// character is the first one whose line starts below the viewport top. `mutate`
// changes the layout before that character, inside the same text node; the
// character's top is then sampled on every frame. Returns drift in px per frame.
async function characterDriftAfter(page: Page, mutate: "reflow" | "prepend") {
  return page.evaluate(async (mode) => {
    const root = document.querySelector<HTMLElement>("[data-tab-scroll-root]")!;
    const row = [...document.querySelectorAll<HTMLElement>("[data-message-id]")].find((n) =>
      n.textContent?.includes("Target message 40"),
    )!;
    const paragraph = row.querySelectorAll("p")[1]!;
    const text = paragraph.firstChild as Text;
    if (text.nodeType !== Node.TEXT_NODE) throw new Error("paragraph must start with plain text");
    const frame = () => new Promise((r) => requestAnimationFrame(r));
    text.data = "reflow words ".repeat(160);
    await frame();
    await frame();
    const rootTop = root.getBoundingClientRect().top;
    root.dispatchEvent(new WheelEvent("wheel", { deltaY: 1, bubbles: true }));
    root.scrollTop += paragraph.getBoundingClientRect().top - rootTop + 120;
    await frame();
    await frame();
    const charTop = (i: number) => {
      const range = document.createRange();
      range.setStart(text, i);
      range.setEnd(text, i + 1);
      return range.getBoundingClientRect().top - rootTop;
    };
    let at = 0;
    while (at < text.data.length && (text.data[at] === " " || charTop(at) < 10)) at++;
    const before = charTop(at);
    if (mode === "reflow") {
      paragraph.style.fontSize = "22px";
    } else {
      const added = "prepended words ".repeat(120);
      text.insertData(0, added);
      at += added.length;
    }
    const drift: number[] = [];
    for (let i = 0; i < 20; i++) {
      await frame();
      drift.push(Math.abs(charTop(at) - before));
    }
    return { before, drift };
  }, mutate);
}

test.describe("DM reading character inside one long text node (CHE-4638 design)", () => {
  for (const [name, mutate] of [
    ["the text reflows", "reflow"],
    ["text is prepended before the character", "prepend"],
  ] as const) {
    test(`${name}: no frame moves the visible character`, async ({ page }, testInfo) => {
      await withScrolledDm(page, async () => {
        const { before, drift } = await characterDriftAfter(page, mutate);
        await testInfo.attach("drift-px-per-frame", { body: JSON.stringify(drift), contentType: "application/json" });
        console.log(`${name}: character at ${before.toFixed(1)} px; drift per frame ${drift.map((d) => d.toFixed(1)).join(" ")}`);
        expect(before).toBeGreaterThanOrEqual(10);
        expect(Math.max(...drift)).toBeLessThanOrEqual(2);
      });
    });
  }
});

// A late height change, as an image or a code block finishing layout does.
// `offset` is the row's distance from the target message (negative = above).
async function growRow(page: Page, offset: number) {
  await page.evaluate((delta) => {
    const rows = [...document.querySelectorAll<HTMLElement>("[data-message-id]")];
    const i = rows.findIndex((n) => n.textContent?.includes("Target message 40"));
    rows[i + delta]!.style.minHeight = "260px";
  }, offset);
}

async function settle(page: Page) {
  await page.waitForTimeout(1500);
}

async function rowTop(page: Page) {
  return page.evaluate(
    () =>
      [...document.querySelectorAll("[data-message-id]")]
        .find((n) => n.textContent?.includes("Target message 40"))!
        .getBoundingClientRect().top,
  );
}

async function targetTop(page: Page) {
  return page.getByText("Target message 40").evaluate((el) => el.getBoundingClientRect().top);
}

// Mobile WebKit has no mouse.wheel and no touch-swipe API. Stage the same input
// a finger drag does (a wheel delta the follow latch reads), then move the list
// in the same task, so a scroll counts as the reader's and is not pinned back.
async function dragUp(page: Page, px: number) {
  await page.evaluate((delta) => {
    const el = document.querySelector("[data-tab-scroll-root]")!;
    el.dispatchEvent(new WheelEvent("wheel", { deltaY: -delta, bubbles: true }));
    el.scrollTop -= delta;
  }, px);
}

// Virtuoso renders rows above and below the fold, so "visible" is not "on screen".
async function onScreen(page: Page) {
  return page.evaluate(() => {
    const row = [...document.querySelectorAll("[data-message-id]")].find((n) => n.textContent?.includes("Target message 40"));
    if (!row) return false;
    const { top } = row.getBoundingClientRect();
    return top > 80 && top < 400;
  });
}
