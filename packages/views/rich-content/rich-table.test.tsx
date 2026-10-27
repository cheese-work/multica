import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const copyText = vi.hoisted(() => vi.fn(async () => true));
vi.mock("@multica/ui/lib/clipboard", () => ({ copyText }));

vi.mock("../issues/hooks", () => ({
  useResolveIssueIdentifier: () => null,
}));

vi.mock("../i18n", async () => {
  const editor: Record<string, Record<string, unknown>> = (await import("../locales/en/editor.json")).default;
  // Records the selected key path so plural keys resolve the way i18next does.
  const t = (select: (bundle: unknown) => unknown, values: Record<string, unknown> = {}) => {
    const path: string[] = [];
    const recorder: unknown = new Proxy({}, { get: (_, key: string) => (path.push(key), recorder) });
    select(recorder);
    const [section = "", key = ""] = path;
    const strings = editor[section] ?? {};
    const plural = values.count === 1 ? `${key}_one` : `${key}_other`;
    const template = String(strings[key] ?? strings[plural] ?? "");
    return template.replace(/\{\{(\w+)\}\}/g, (_, name: string) => String(values[name] ?? ""));
  };
  return { useT: () => ({ t }), useTimeAgo: () => "just now" };
});

vi.mock("@multica/core/api", () => ({
  api: { getAttachmentTextContent: vi.fn() },
  PreviewTooLargeError: class extends Error {},
  PreviewUnsupportedError: class extends Error {},
}));

vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ issueDetail: (id: string) => `/acme/issues/${id}` }),
  useWorkspaceSlug: () => "acme",
}));

vi.mock("../editor/link-hover-card", () => ({
  useLinkHover: () => ({}),
  LinkHoverCard: () => null,
}));

import { RichContent } from "./rich-content";

const TABLE = [
  "Intro paragraph.",
  "",
  "| GitHub | Relation | Count |",
  "| --- | --- | --- |",
  "| #7738 harness switch | MUL-12 tracks it | 10 |",
  "| #4753 agent registry | see ==hot== | 9 |",
  "| #9250 PATH key | none | 100 |",
].join("\n");

function renderTable() {
  const view = render(<RichContent content={TABLE} />);
  const table = view.container.querySelector("table");
  if (!table) throw new Error("table not rendered");
  return { ...view, table };
}

afterEach(() => {
  window.localStorage.clear();
  copyText.mockClear();
});

describe("RichTable", () => {
  it("keeps the browser's layout until a column is resized", () => {
    const { table } = renderTable();
    expect(table.closest(".tableWrapper")).not.toBeNull();
    expect(table).not.toHaveAttribute("data-column-widths");
    expect(table.querySelectorAll("colgroup col")).toHaveLength(0);
    expect(screen.queryByRole("button", { name: "Reset widths" })).toBeNull();
  });

  it("gives an overflowing table capped content widths without saving them", () => {
    const natural = { "max-content": [500, 260, 90], "min-content": [200, 330, 64] } as const;
    const rect = vi
      .spyOn(HTMLTableCellElement.prototype, "getBoundingClientRect")
      .mockImplementation(function (this: HTMLTableCellElement) {
        const widths = natural[this.closest("table")?.style.width as keyof typeof natural];
        return { width: widths?.[this.cellIndex] ?? 0 } as DOMRect;
      });
    const clientWidth = vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(560);
    try {
      const { table } = renderTable();
      const cols = Array.from(table.querySelectorAll<HTMLElement>("colgroup col"), (col) => col.style.width);
      // Content width up to 320px, never below a column's minimum.
      expect(cols).toEqual(["320px", "330px", "90px"]);
      expect(table).toHaveAttribute("data-column-widths");
      expect(table.closest(".rich-table")).not.toHaveAttribute("data-resized");
      expect(screen.queryByRole("button", { name: "Reset widths" })).toBeNull();
      expect(window.localStorage.getItem("multica:rich-table-widths")).toBeNull();
    } finally {
      rect.mockRestore();
      clientWidth.mockRestore();
    }
  });

  describe("auto-fit cost", () => {
    // Counts the max-content pass of measureColumns, which lifts the table's
    // sizing and forces a layout: one header-cell read per column.
    function probeMeasurePasses() {
      const natural = { "max-content": [500, 260, 90], "min-content": [200, 330, 64] } as const;
      let passes = 0;
      const rect = vi
        .spyOn(HTMLTableCellElement.prototype, "getBoundingClientRect")
        .mockImplementation(function (this: HTMLTableCellElement) {
          const mode = this.closest("table")?.style.width as keyof typeof natural;
          if (mode === "max-content" && this.cellIndex === 0) passes += 1;
          return { width: natural[mode]?.[this.cellIndex] ?? 0, left: 0, right: 0, top: 0, height: 0 } as DOMRect;
        });
      const clientWidth = vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(560);
      return {
        passes: () => passes,
        restore: () => {
          rect.mockRestore();
          clientWidth.mockRestore();
        },
      };
    }

    it("reuses a table's measured widths when it remounts", () => {
      const probe = probeMeasurePasses();
      try {
        const content = TABLE.replace("harness switch", "remount cache");
        const first = render(<RichContent content={content} />);
        expect(probe.passes()).toBe(1);
        const cols = () =>
          Array.from(document.querySelectorAll<HTMLElement>("colgroup col"), (col) => col.style.width);
        const fitted = cols();
        expect(fitted).toEqual(["320px", "330px", "90px"]);

        first.unmount();
        render(<RichContent content={content} />);
        expect(probe.passes()).toBe(1);
        expect(cols()).toEqual(fitted);
      } finally {
        probe.restore();
      }
    });

    function stubFonts(status: "loaded" | "loading") {
      let settle = () => {};
      const ready = status === "loaded" ? Promise.resolve() : new Promise<void>((resolve) => (settle = resolve));
      Object.defineProperty(document, "fonts", { configurable: true, value: { status, ready } });
      return {
        loaded: async () => {
          settle();
          await act(async () => {});
        },
        restore: () => Reflect.deleteProperty(document, "fonts"),
      };
    }

    it("keeps the cached widths on remount once fonts have loaded", async () => {
      const probe = probeMeasurePasses();
      const fonts = stubFonts("loaded");
      try {
        const content = TABLE.replace("harness switch", "loaded fonts remount");
        const first = render(<RichContent content={content} />);
        await act(async () => {});
        expect(probe.passes()).toBe(1);

        first.unmount();
        render(<RichContent content={content} />);
        await act(async () => {});
        expect(probe.passes()).toBe(1);
      } finally {
        fonts.restore();
        probe.restore();
      }
    });

    it("measures again when a font finishes loading after the table mounted", async () => {
      const probe = probeMeasurePasses();
      const fonts = stubFonts("loading");
      try {
        render(<RichContent content={TABLE.replace("harness switch", "font swap")} />);
        expect(probe.passes()).toBe(1);
        await fonts.loaded();
        expect(probe.passes()).toBe(2);
      } finally {
        fonts.restore();
        probe.restore();
      }
    });

    it("measures again when the available width changes", () => {
      const probe = probeMeasurePasses();
      try {
        const content = TABLE.replace("harness switch", "width cache");
        const first = render(<RichContent content={content} />);
        first.unmount();
        vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(900);
        render(<RichContent content={content} />);
        expect(probe.passes()).toBe(2);
      } finally {
        probe.restore();
      }
    });

    it("skips auto-fit for a tall table", () => {
      const probe = probeMeasurePasses();
      try {
        const rows = Array.from({ length: 120 }, (_, i) => `| row ${i} | value | ${i} |`);
        const tall = ["| GitHub | Relation | Count |", "| --- | --- | --- |", ...rows].join("\n");
        const { container } = render(<RichContent content={tall} />);
        expect(probe.passes()).toBe(0);
        expect(container.querySelector("table")).not.toHaveAttribute("data-column-widths");
      } finally {
        probe.restore();
      }
    });

    it("observes size changes with a single ResizeObserver per table", () => {
      const Original = globalThis.ResizeObserver;
      let created = 0;
      globalThis.ResizeObserver = class {
        constructor() {
          created += 1;
        }
        observe() {}
        unobserve() {}
        disconnect() {}
      } as unknown as typeof ResizeObserver;
      try {
        renderTable();
        expect(created).toBe(1);
      } finally {
        globalThis.ResizeObserver = Original;
      }
    });
  });

  it("resizes a column from the keyboard, persists it, and resets", async () => {
    const { table, unmount } = renderTable();
    const handle = screen.getByRole("separator", { name: "Resize Relation column" });

    fireEvent.keyDown(handle, { key: "ArrowRight", shiftKey: true });

    expect(table).toHaveAttribute("data-column-widths");
    expect(table.querySelectorAll("colgroup col")).toHaveLength(3);
    expect(window.localStorage.getItem("multica:rich-table-widths")).toContain("widths");

    // Widths belong to the table's content, so a fresh mount restores them.
    unmount();
    const remounted = renderTable();
    expect(remounted.table).toHaveAttribute("data-column-widths");

    await userEvent.click(screen.getByRole("button", { name: "Reset widths" }));
    expect(remounted.table).not.toHaveAttribute("data-column-widths");
    expect(remounted.table.querySelectorAll("colgroup col")).toHaveLength(0);
    expect(window.localStorage.getItem("multica:rich-table-widths")).toBe("{}");
  });

  it("keeps a handle's height after a click, so a double-click lands", () => {
    renderTable();
    const handle = screen.getByRole("separator", { name: "Resize Relation column" });
    handle.style.height = "38px";
    fireEvent.pointerDown(handle, { clientX: 100, pointerId: 1 });
    fireEvent.pointerUp(window, { clientX: 100, pointerId: 1 });
    expect(handle.style.height).toBe("38px");
  });

  it("copies the table as the author wrote it", async () => {
    renderTable();
    await userEvent.click(screen.getByRole("button", { name: "Copy as Markdown" }));
    expect(copyText).toHaveBeenCalledWith(
      [
        "| GitHub | Relation | Count |",
        "| --- | --- | --- |",
        "| #7738 harness switch | MUL-12 tracks it | 10 |",
        "| #4753 agent registry | see ==hot== | 9 |",
        "| #9250 PATH key | none | 100 |",
      ].join("\n"),
    );
  });

  it("copies code, explicit links and quoted tables unchanged", async () => {
    const table = [
      "| Code | Link |",
      "| --- | --- |",
      "| `<mark>hello</mark>` and `[a](b)` | [https://x.com](https://x.com) |",
    ];
    render(<RichContent content={["Quoted:", "", ...table.map((line) => `> ${line}`)].join("\n")} />);
    await userEvent.click(screen.getByRole("button", { name: "Copy as Markdown" }));
    expect(copyText).toHaveBeenCalledWith(table.join("\n"));
  });

  it("expands into a sortable, filterable view of the same cells", async () => {
    renderTable();
    await userEvent.click(screen.getByRole("button", { name: "Expand table" }));

    const dialog = await screen.findByRole("dialog");
    const bodyRows = () =>
      within(dialog)
        .getAllByRole("row")
        .slice(1)
        .map((row) => within(row).getAllByRole("cell").map((cell) => cell.textContent));

    expect(bodyRows()).toEqual([
      ["1", "#7738 harness switch", "MUL-12 tracks it", "10"],
      ["2", "#4753 agent registry", "see hot", "9"],
      ["3", "#9250 PATH key", "none", "100"],
    ]);
    // Inline Markdown keeps its renderer inside the dialog.
    expect(within(dialog).getByText("hot").tagName).toBe("MARK");

    // Numbers sort as numbers; row numbers keep the source position.
    await userEvent.click(within(dialog).getByRole("button", { name: "Count" }));
    expect(bodyRows().map((row) => row[0])).toEqual(["2", "1", "3"]);

    await userEvent.type(within(dialog).getByRole("searchbox", { name: "Filter rows" }), "path");
    expect(bodyRows()).toEqual([["3", "#9250 PATH key", "none", "100"]]);

    await userEvent.click(within(dialog).getByRole("button", { name: "Copy as CSV" }));
    expect(copyText).toHaveBeenCalledWith(
      "GitHub,Relation,Count\n#7738 harness switch,MUL-12 tracks it,10\n#4753 agent registry,see hot,9\n#9250 PATH key,none,100",
    );

    await userEvent.click(within(dialog).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });
});
