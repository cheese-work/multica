import {
  expect,
  test,
  type Page,
  type Request,
  type Response,
} from "@playwright/test";
import { createTestApi, loginAsDefault, reloadAppPage } from "./helpers";
import type { TestApiClient, TestTableIssueSeed } from "./fixtures";

type TableRequestBody = {
  query?: { sort?: { field?: string; direction?: string } };
  group?: { kind?: string };
  group_key?: string | null;
  parent_id?: string | null;
  page?: { limit?: number; cursor?: string | null };
};

type TableGroupsResponse = {
  total: number;
  groups: Array<{
    key: string;
    count: number;
    value: { kind: string; status?: string };
  }>;
};

type TableRowsResponse = {
  total: number;
  next_cursor: string | null;
  rows: Array<{
    issue: { id: string; title: string; status: string };
    direct_child_count: number;
  }>;
};

function apiPath(request: Request) {
  return new URL(request.url()).pathname;
}

function tableBody(request: Request): TableRequestBody {
  return (request.postDataJSON() ?? {}) as TableRequestBody;
}

async function switchToTable(page: Page) {
  const currentView = page.getByRole("button", { name: "Board", exact: true });
  await expect(currentView).toBeVisible();
  await currentView.click();
  const tableOption = page.getByRole("menuitemradio", {
    name: "Table",
    exact: true,
  });
  await tableOption.click();
  await expect(
    page.getByRole("button", { name: "Table", exact: true }),
  ).toBeVisible();
  await expect(tableOption).toBeHidden();
}

async function groupByStatus(page: Page) {
  await page.getByRole("button", { name: "Group", exact: true }).click();
  const statusOption = page.getByRole("menuitemradio", {
    name: "Status",
    exact: true,
  });
  await statusOption.click();
  await expect(
    page.getByRole("button", { name: "Group: Status", exact: true }),
  ).toBeVisible();
  await expect(statusOption).toBeHidden();
}

/**
 * What the rows API has returned, read from its responses. The table shows no
 * "Loaded N of M" text, so this is the signal: the loaded rows and the first
 * title of each group. `reset()` drops everything seen so far, so a check made
 * afterwards can only be satisfied by responses that arrive after it. The Board
 * that mounts first fetches through the same endpoint.
 */
function collectTableRows(page: Page) {
  const loadedRows = new Map<string, string>();
  const firstTitleOfGroup = new Map<string, string>();
  const handler = async (response: Response) => {
    if (apiPath(response.request()) !== "/api/issues/table/rows" || response.status() !== 200) return;
    const key = tableBody(response.request()).group_key ?? "";
    const payload = (await response.json()) as TableRowsResponse;
    for (const row of payload.rows) loadedRows.set(row.issue.id, row.issue.title);
    const first = payload.rows[0];
    if (first && !firstTitleOfGroup.has(key)) firstTitleOfGroup.set(key, first.issue.title);
  };
  page.on("response", handler);
  return {
    loadedRows,
    firstTitleOfGroup,
    reset() {
      loadedRows.clear();
      firstTitleOfGroup.clear();
    },
    stop() {
      page.off("response", handler);
    },
  };
}

test.describe("Issue Table server grouping", () => {
  test.describe.configure({ timeout: 120000 });

  let api: TestApiClient;

  test.beforeEach(async ({ page }) => {
    api = await createTestApi();
    await loginAsDefault(page);
  });

  test.afterEach(async () => {
    await api?.cleanup();
  });

  test("groups 1,001 issues exactly without materializing the full result", async ({
    page,
  }) => {
    const run = Date.now();
    const seeds: TestTableIssueSeed[] = Array.from({ length: 1001 }, (_, index) => ({
      title: `E2E Table Large ${run} ${index.toString().padStart(4, "0")}`,
      status: index < 501 ? "backlog" : index < 801 ? "todo" : "done",
      position: index + 1,
    }));
    await api.seedTableIssues(seeds);
    await reloadAppPage(page);

    const tableRowRequests: TableRequestBody[] = [];
    const legacyMembershipRequests: string[] = [];
    let lastObservedRequestAt = Date.now();
    const collectRequest = (request: Request) => {
      const path = apiPath(request);
      if (path === "/api/issues/table/rows") {
        lastObservedRequestAt = Date.now();
        tableRowRequests.push(tableBody(request));
      }
      if (path === "/api/issues" || path === "/api/issues/query") {
        lastObservedRequestAt = Date.now();
        legacyMembershipRequests.push(request.url());
      }
    };

    const table = collectTableRows(page);
    page.on("request", collectRequest);
    // The page initially mounts Board, whose seven status buckets may still
    // be finishing when the header becomes interactive. Observe that startup
    // traffic, wait for it to settle, then clear it while keeping the listener
    // installed so any request caused by the Table switch is captured.
    await expect
      .poll(() => Date.now() - lastObservedRequestAt, {
        timeout: 5000,
        intervals: [100, 200, 300],
      })
      .toBeGreaterThan(750);
    tableRowRequests.length = 0;
    legacyMembershipRequests.length = 0;
    lastObservedRequestAt = Date.now();
    // Whatever the Board loaded does not count towards the Table's progress.
    table.reset();
    await switchToTable(page);
    const groupsResponsePromise = page.waitForResponse(
      (response) =>
        apiPath(response.request()) === "/api/issues/table/groups" &&
        response.status() === 200,
    );
    await groupByStatus(page);

    const groupsResponse = await groupsResponsePromise;
    const groups = (await groupsResponse.json()) as TableGroupsResponse;
    expect(groups.total).toBe(1001);
    expect(
      Object.fromEntries(groups.groups.map((group) => [group.key, group.count])),
    ).toEqual({
      "status:backlog": 501,
      "status:todo": 300,
      "status:done": 200,
    });

    const backlogGroup = page
      .getByRole("row")
      .filter({ hasText: "Backlog" })
      .first();
    await expect(backlogGroup).toContainText("501");
    // Completion: the Table's own backlog request has returned its first page
    // and that page's first row is on screen.
    await expect.poll(() => table.firstTitleOfGroup.get("status:backlog")).toBeTruthy();
    await expect(
      page.getByRole("row").filter({ hasText: table.firstTitleOfGroup.get("status:backlog")! }),
    ).toBeVisible();
    // Loaded of total: some rows, never the whole result, of exactly 1001.
    expect(table.loadedRows.size).toBeGreaterThan(0);
    expect(table.loadedRows.size).toBeLessThan(groups.total);
    table.stop();
    await expect(
      page.getByText(/Grouping and hierarchy are paused/),
    ).toHaveCount(0);

    await expect
      .poll(() => Date.now() - lastObservedRequestAt, {
        timeout: 5000,
        intervals: [100, 200, 300],
      })
      .toBeGreaterThan(750);
    page.off("request", collectRequest);
    expect(legacyMembershipRequests).toEqual([]);
    expect(tableRowRequests.length).toBeGreaterThan(0);
    expect(tableRowRequests.length).toBeLessThan(8);
    expect(
      tableRowRequests.every(
        (body) =>
          (body.page?.limit ?? 0) <= 50 &&
          (body.page?.cursor === null || body.page?.cursor === undefined),
      ),
    ).toBe(true);
  });

  // Negative control for the Table progress signal above: with the Board's
  // data present and the Table's rows withheld, the signal must stay false.
  // Without the reset, the Board's responses alone would satisfy it.
  test("the Table progress signal ignores the Board's rows when the Table's rows are withheld", async ({
    page,
  }) => {
    const run = Date.now();
    await api.seedTableIssues(
      Array.from({ length: 60 }, (_, index) => ({
        title: `E2E Table Control ${run} ${index.toString().padStart(2, "0")}`,
        status: "todo" as const,
        position: index + 1,
      })),
    );
    await reloadAppPage(page);

    const table = collectTableRows(page);
    let lastRowsResponseAt = Date.now();
    page.on("response", (response) => {
      if (apiPath(response.request()) === "/api/issues/table/rows") lastRowsResponseAt = Date.now();
    });
    await expect
      .poll(() => Date.now() - lastRowsResponseAt, { timeout: 8000, intervals: [100, 200, 300] })
      .toBeGreaterThan(1000);
    // The Board is up and has loaded rows through the same endpoint.
    const boardRows = table.loadedRows.size;
    expect(boardRows).toBeGreaterThan(0);

    // From here the Table's rows are withheld.
    await page.route("**/api/issues/table/rows", (route) => route.abort());
    table.reset();
    await switchToTable(page);
    await groupByStatus(page);
    await page.waitForTimeout(3000);

    expect(table.loadedRows.size).toBe(0);
    expect(table.firstTitleOfGroup.size).toBe(0);
    table.stop();
  });

  test("keeps same-group children nested and cross-group children at the group root", async ({
    page,
  }) => {
    const run = Date.now();
    const parentTitle = `E2E Hierarchy Parent ${run}`;
    const sameGroupTitle = `E2E Hierarchy Same Group ${run}`;
    const crossGroupTitle = `E2E Hierarchy Cross Group ${run}`;
    const [parent] = await api.seedTableIssues([
      { title: parentTitle, status: "todo", position: 1 },
    ]);
    if (!parent) throw new Error("Hierarchy parent fixture was not created");
    await api.seedTableIssues([
      {
        title: sameGroupTitle,
        status: "todo",
        parentIssueId: parent.id,
        position: 1,
      },
      {
        title: crossGroupTitle,
        status: "done",
        parentIssueId: parent.id,
        position: 2,
      },
    ]);
    await reloadAppPage(page);
    await switchToTable(page);

    const todoRootPromise = page.waitForResponse((response) => {
      if (apiPath(response.request()) !== "/api/issues/table/rows") return false;
      const body = tableBody(response.request());
      return (
        body.group_key === "status:todo" &&
        body.parent_id === null &&
        response.status() === 200
      );
    });
    const todoChildrenPromise = page.waitForResponse((response) => {
      if (apiPath(response.request()) !== "/api/issues/table/rows") return false;
      const body = tableBody(response.request());
      // The ungrouped table fetches the parent's children too, just before
      // grouping applies; the assertion below is about the todo group's branch.
      return (
        body.parent_id === parent.id &&
        body.group_key === "status:todo" &&
        response.status() === 200
      );
    });
    const doneRootPromise = page.waitForResponse((response) => {
      if (apiPath(response.request()) !== "/api/issues/table/rows") return false;
      const body = tableBody(response.request());
      return (
        body.group_key === "status:done" &&
        body.parent_id === null &&
        response.status() === 200
      );
    });

    // Grouped row pages carry no query-wide total: the rows API computes one
    // only for the ungrouped root head, and takes group totals from /groups
    // (issue_table_rows.go). The total of 3 seeded issues is therefore read
    // from the groups response, which is where the contract puts it.
    const groupsPromise = page.waitForResponse(
      (response) =>
        apiPath(response.request()) === "/api/issues/table/groups" &&
        response.status() === 200,
    );
    await groupByStatus(page);
    const [groupsResponse, todoRootResponse, todoChildrenResponse, doneRootResponse] =
      await Promise.all([
        groupsPromise,
        todoRootPromise,
        todoChildrenPromise,
        doneRootPromise,
      ]);
    const groups = (await groupsResponse.json()) as TableGroupsResponse;
    const todoRoot = (await todoRootResponse.json()) as TableRowsResponse;
    const todoChildren =
      (await todoChildrenResponse.json()) as TableRowsResponse;
    const doneRoot = (await doneRootResponse.json()) as TableRowsResponse;

    expect(groups.total).toBe(3);
    expect(
      Object.fromEntries(groups.groups.map((group) => [group.key, group.count])),
    ).toEqual({ "status:todo": 2, "status:done": 1 });
    expect(todoRoot.rows).toEqual([
      expect.objectContaining({
        issue: expect.objectContaining({ id: parent.id, title: parentTitle }),
        direct_child_count: 1,
      }),
    ]);
    expect(todoChildren.rows.map((row) => row.issue.title)).toEqual([
      sameGroupTitle,
    ]);
    expect(doneRoot.rows.map((row) => row.issue.title)).toEqual([
      crossGroupTitle,
    ]);

    const sameGroupRow = page
      .getByRole("row")
      .filter({ hasText: sameGroupTitle });
    const crossGroupRow = page
      .getByRole("row")
      .filter({ hasText: crossGroupTitle });
    await expect(sameGroupRow).toBeVisible();
    await expect(crossGroupRow).toBeVisible();
    await expect(
      sameGroupRow.getByRole("button", { name: sameGroupTitle }).locator(".."),
    ).toHaveCSS("padding-left", "18px");
    await expect(
      crossGroupRow.getByRole("button", { name: crossGroupTitle }).locator(".."),
    ).toHaveCSS("padding-left", "0px");
  });

  test("drops stale branch cursors after a realtime sort-boundary update", async ({
    page,
  }) => {
    const run = Date.now();
    const seeds = await api.seedTableIssues(
      Array.from({ length: 60 }, (_, index) => ({
        title: `E2E Table Cursor ${run} ${index.toString().padStart(2, "0")}`,
        status: "todo" as const,
        position: index + 1,
      })),
    );
    // Manual order exists only under status grouping, so this ungrouped table
    // sorts by Updated date, newest first. The issue touched below is the one
    // in the very last tail row, so touching it moves it to the head, across
    // the page boundary.
    if (seeds.length !== 60) throw new Error("Cursor fixture was not created");
    await reloadAppPage(page);

    const firstHeadPromise = page.waitForResponse((response) => {
      if (apiPath(response.request()) !== "/api/issues/table/rows") return false;
      const body = tableBody(response.request());
      return (
        body.group?.kind === "none" &&
        body.query?.sort?.field === "updated_at" &&
        (body.page?.cursor === null || body.page?.cursor === undefined) &&
        response.status() === 200
      );
    });
    await switchToTable(page);
    await page.getByRole("button", { name: "Display", exact: true }).click();
    await page.getByRole("combobox", { name: "Ordering" }).click();
    await page.getByRole("option", { name: "Updated date" }).click();
    await page.keyboard.press("Escape");
    const firstHead = (await (await firstHeadPromise).json()) as TableRowsResponse;
    const staleCursor = firstHead.next_cursor;
    expect(staleCursor).toBeTruthy();

    const firstTailPromise = page.waitForResponse((response) => {
      if (apiPath(response.request()) !== "/api/issues/table/rows") return false;
      const body = tableBody(response.request());
      return body.page?.cursor === staleCursor && response.status() === 200;
    });
    const tableScroller = page.locator("table").locator("..");
    await tableScroller.evaluate((element) => {
      element.scrollTop = element.scrollHeight;
    });
    const firstTail = (await (await firstTailPromise).json()) as TableRowsResponse;
    // "Loaded 60 of 60": the head and the tail together return all 60 rows and
    // nothing is left to load; the last row is rendered. The table shows no
    // such text any more.
    expect(firstHead.rows.length + firstTail.rows.length).toBe(60);
    const movedId = firstTail.rows.at(-1)?.issue.id;
    const moved = seeds.find((seed) => seed.id === movedId);
    if (!moved) throw new Error("The last tail row is not a seeded issue");
    expect(firstTail.next_cursor).toBeNull();
    const lastTitle = firstTail.rows.at(-1)?.issue.title;
    expect(lastTitle).toBeTruthy();
    await expect(page.getByRole("row").filter({ hasText: lastTitle! })).toBeAttached();

    const postUpdateResponses: Array<{
      body: TableRequestBody;
      payload: TableRowsResponse;
    }> = [];
    const collectResponse = async (response: Response) => {
      if (
        apiPath(response.request()) !== "/api/issues/table/rows" ||
        response.status() !== 200
      ) {
        return;
      }
      postUpdateResponses.push({
        body: tableBody(response.request()),
        payload: (await response.json()) as TableRowsResponse,
      });
    };
    // The refreshed head: a head page that already has the touched issue first.
    // A head page that was in flight when the update landed does not count.
    const isFreshHead = ({ body, payload }: { body: TableRequestBody; payload: TableRowsResponse }) =>
      (body.page?.cursor === null || body.page?.cursor === undefined) &&
      payload.rows[0]?.issue.id === moved.id;
    page.on("response", collectResponse);
    await api.updateIssue(moved.id, { priority: "high" });

    await expect
      .poll(
        () =>
          postUpdateResponses.find(isFreshHead)?.payload.next_cursor,
        { timeout: 10000 },
      )
      .not.toBeUndefined();
    const freshHead = postUpdateResponses.find(isFreshHead)?.payload;
    const freshCursor = freshHead?.next_cursor;
    expect(freshCursor).toBeTruthy();
    expect(freshCursor).not.toBe(staleCursor);

    await tableScroller.evaluate((element) => {
      element.scrollTop = element.scrollHeight;
    });
    await expect
      .poll(
        () =>
          postUpdateResponses.find(
            ({ body }) => body.page?.cursor === freshCursor,
          )?.payload,
        { timeout: 10000 },
      )
      .not.toBeUndefined();
    const freshTail = postUpdateResponses.find(
      ({ body }) => body.page?.cursor === freshCursor,
    )?.payload;
    const refreshedIds = [
      ...(freshHead?.rows ?? []),
      ...(freshTail?.rows ?? []),
    ].map((row) => row.issue.id);
    expect(new Set(refreshedIds).size).toBe(60);
    expect(refreshedIds).toContain(moved.id);
    // Loaded 60 of 60 again after the refresh: the refreshed head and tail hold
    // every row once, and the last refreshed row is on screen.
    expect(freshTail?.next_cursor).toBeNull();
    const refreshedLast = freshTail?.rows.at(-1)?.issue.title;
    expect(refreshedLast).toBeTruthy();
    await expect(page.getByRole("row").filter({ hasText: refreshedLast! })).toBeAttached();
    page.off("response", collectResponse);
  });

  test("shows a branch-level retry and never falls back to client grouping", async ({
    page,
  }) => {
    const title = `E2E Table Branch Retry ${Date.now()}`;
    await api.seedTableIssues([{ title, status: "todo", position: 1 }]);
    await reloadAppPage(page);

    let allowRows = false;
    let forcedFailures = 0;
    await page.route("**/api/issues/table/rows", async (route) => {
      const body = tableBody(route.request());
      if (
        body.group?.kind === "status" &&
        body.group_key === "status:todo" &&
        !allowRows
      ) {
        forcedFailures += 1;
        await route.fulfill({
          status: 500,
          contentType: "application/json",
          body: JSON.stringify({
            error: "forced_e2e_failure",
            message: "Forced Table branch failure",
          }),
        });
        return;
      }
      await route.continue();
    });

    await switchToTable(page);
    await groupByStatus(page);
    const retry = page.getByRole("button", {
      name: "Loading more failed — Retry",
      exact: true,
    });
    await expect(retry).toBeVisible({ timeout: 20000 });
    await expect(page.getByText(title)).toHaveCount(0);
    expect(forcedFailures).toBeGreaterThan(0);

    allowRows = true;
    await retry.click();
    await expect(page.getByText(title)).toBeVisible({ timeout: 10000 });
    await expect(retry).toHaveCount(0);
  });
});
