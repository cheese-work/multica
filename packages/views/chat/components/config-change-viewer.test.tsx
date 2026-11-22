import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import type { DecisionConfigChange } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import { ConfigChangeViewer } from "./config-change-viewer";

const change: DecisionConfigChange = {
  field: "workflow.instructions",
  target_id: "wf-1",
  keys: [],
  diff_kind: "text",
  diff: [{ old_start: 1, new_start: 1, lines: [" keep", "-old line", "+new line", "+added line"] }],
  after: "keep\nnew line\nadded line",
  before_sha256: "b".repeat(64),
  after_sha256: "c".repeat(64),
  meta: { workflow_name: "Production flow", status_key: "in_review" },
  sha256: "a".repeat(64),
};

const open = (value = change) => renderWithI18n(<ConfigChangeViewer change={value} />);

describe("ConfigChangeViewer", () => {
  it("names the field, the target and the size of the change", () => {
    open();
    const viewer = screen.getByTestId("config-change-viewer");
    expect(viewer).toHaveTextContent("Workflow step instructions");
    expect(viewer).toHaveTextContent("wf-1");
    expect(viewer).toHaveTextContent("+2 −1");
  });

  it("shows only the changed rows on open: old value struck through, new value in a chip, no context line", () => {
    open();
    const rows = screen.getByRole("list", { name: "Changes to Workflow step instructions" });
    expect(rows).toHaveTextContent("old line");
    expect(rows).toHaveTextContent("new line");
    expect(rows).toHaveTextContent("added line");
    expect(rows).not.toHaveTextContent("keep");
    expect(rows.querySelector("del")).toHaveTextContent("old line");
    expect(rows.querySelectorAll("ins")).toHaveLength(2);
    expect(rows.textContent).not.toMatch(/[+−]/);
  });

  it("names each change in words, so status is never colour alone", () => {
    open();
    const rows = within(screen.getByRole("list", { name: "Changes to Workflow step instructions" })).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("Changed");
    expect(rows[1]).toHaveTextContent("Added");
  });

  it("shows a removed line with no replacement as Removed", () => {
    open({ ...change, diff: [{ old_start: 1, new_start: 1, lines: ["-gone"] }], after: "" });
    const row = within(screen.getByRole("list", { name: "Changes to Workflow step instructions" })).getByRole("listitem");
    expect(row).toHaveTextContent("Removed");
    expect(row.querySelector("del")).toHaveTextContent("gone");
    expect(row.querySelector("ins")).toBeNull();
  });

  it("does not show the result until Show full is pressed", () => {
    open();
    expect(screen.getByRole("button", { name: "Show full" })).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("region", { name: "Result after approval" })).toBeNull();
  });

  it("opens the full value as written, with no − or + marks, and closes it again", async () => {
    const user = userEvent.setup();
    open();
    await user.click(screen.getByRole("button", { name: "Show full" }));
    const region = screen.getByRole("region", { name: "Result after approval" });
    expect(region).toHaveAttribute("tabindex", "0");
    expect(region.textContent).toBe("keep\nnew line\nadded line");
    expect(screen.getByRole("button", { name: "Hide full" })).toHaveAttribute("aria-expanded", "true");
    await user.click(screen.getByRole("button", { name: "Hide full" }));
    expect(screen.queryByRole("region", { name: "Result after approval" })).toBeNull();
  });

  it.each([
    ["lines", ["a", "b"], "a\nb"],
    ["json", { a: 1 }, '{\n  "a": 1\n}'],
  ] as const)("renders a %s result in Show full", async (diff_kind, after, text) => {
    const user = userEvent.setup();
    open({ ...change, diff_kind, after });
    await user.click(screen.getByRole("button", { name: "Show full" }));
    expect(screen.getByRole("region", { name: "Result after approval" }).textContent).toBe(text);
  });

  it("renders values as text, never as markup", async () => {
    const user = userEvent.setup();
    const evil = '<img src=x onerror="alert(1)"> **bold**';
    const { container } = open({ ...change, diff: [{ old_start: 1, new_start: 1, lines: ["-a", `+${evil}`] }], after: evil });
    await user.click(screen.getByRole("button", { name: "Show full" }));
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByRole("region", { name: "Result after approval" }).textContent).toBe(evil);
  });

  it("says so when no line changes, and when only whitespace changes", () => {
    const view = open({ ...change, diff: [] });
    expect(screen.getByText("No line changes.")).toBeInTheDocument();
    view.unmount();
    open({ ...change, diff: [], meta: { whitespace_only: true } });
    expect(screen.getByText("Only whitespace or line endings change.")).toBeInTheDocument();
  });

  it("shows a hidden-lines row between distant hunks", () => {
    open({ ...change, diff: [{ old_start: 1, new_start: 1, lines: ["-a", "+b"] }, { old_start: 20, new_start: 20, lines: ["-c", "+d"] }] });
    expect(screen.getByText(/lines? hidden/)).toBeInTheDocument();
  });

  it("shows exactly the added and removed skills of a list diff, one row each", () => {
    const skills = [
      "Review (11111111-1111-1111-1111-111111111111) [enabled]",
      "Deploy (22222222-2222-2222-2222-222222222222) [enabled]",
      "Triage (33333333-3333-3333-3333-333333333333) [enabled]",
    ];
    open({
      ...change,
      field: "agent.skills",
      diff_kind: "lines",
      diff: [{ old_start: 2, new_start: 2, lines: [` ${skills[0]}`, `-${skills[1]}`, `+${skills[2]}`] }],
      after: [skills[0], skills[2]],
      meta: undefined,
    });
    expect(screen.getByTestId("config-change-viewer")).toHaveTextContent("+1 −1");
    const rows = screen.getByRole("list", { name: "Changes to Agent skills" });
    expect(rows.querySelector("del")).toHaveTextContent(skills[1]!);
    expect(rows.querySelector("ins")).toHaveTextContent(skills[2]!);
    expect(rows).not.toHaveTextContent(skills[0]!);
  });

  it("on a long config shows only the changed rows on open, not the whole value", () => {
    const lines = Array.from({ length: 500 }, (_, i) => `setting_${i}: ${i}`);
    open({
      ...change,
      diff: [{ old_start: 250, new_start: 250, lines: [" setting_249: 249", "-setting_250: 250", "+setting_250: 999", " setting_251: 251"] }],
      after: lines.join("\n"),
    });
    const viewer = screen.getByTestId("config-change-viewer");
    expect(viewer.textContent).toContain("setting_250: 999");
    expect(viewer.textContent).not.toContain("setting_100");
    expect(viewer.textContent).not.toContain("setting_249");
    expect(viewer.textContent!.length).toBeLessThan(600);
  });

  it("keeps a long unbroken line wrapping inside a scrolling list and Show full", async () => {
    const user = userEvent.setup();
    const long = "x".repeat(10_000);
    open({ ...change, diff: [{ old_start: 1, new_start: 1, lines: [`-${long}`, `+${long}y`] }], after: `${long}y` });
    expect(screen.getByRole("list", { name: "Changes to Workflow step instructions" })).toHaveClass("overflow-auto", "break-words");
    await user.click(screen.getByRole("button", { name: "Show full" }));
    expect(screen.getByRole("region", { name: "Result after approval" })).toHaveClass("overflow-auto", "break-words");
  });

  it("names the server-resolved workflow and status key of a workflow step change", () => {
    open();
    expect(screen.getByTestId("config-change-workflow")).toHaveTextContent("Workflow: Production flow");
    expect(screen.getByTestId("config-change-status")).toHaveTextContent("Step: in_review");
  });

  it("names no workflow for another field", () => {
    open({ ...change, field: "agent.instructions", meta: undefined });
    expect(screen.queryByTestId("config-change-workflow")).toBeNull();
    expect(screen.queryByTestId("config-change-status")).toBeNull();
  });

  it("lists the settings keys of a settings change", () => {
    open({ ...change, field: "workspace.settings", keys: ["compact_prompt", "auto_compact"] });
    expect(screen.getByTestId("config-change-viewer")).toHaveTextContent("Workspace settings");
    expect(screen.getByTestId("config-change-keys")).toHaveTextContent("compact_prompt, auto_compact");
  });

  it.each([
    ["workspace.routing_agent", "Routing agent"],
    ["workspace.delegated_writes", "Delegated governed writes"],
    ["agent.skills", "Agent skills"],
  ])("names the field %s", (field, label) => {
    open({ ...change, field, meta: undefined });
    expect(screen.getByTestId("config-change-viewer")).toHaveTextContent(label);
  });

  it("shows the server's character counts against the cap, in the server's unit", () => {
    open({ ...change, field: "workspace.context", meta: { unit: "code_points", chars_before: 9990, chars_after: 10000, cap: 10000 } });
    expect(screen.getByTestId("config-change-counts")).toHaveTextContent("Characters: 9,990 → 10,000 of 10,000 allowed");
  });

  it("shows counts without a cap when the server sends none", () => {
    open({ ...change, field: "agent.instructions", meta: { unit: "code_points", chars_before: 120, chars_after: 95 } });
    expect(screen.getByTestId("config-change-counts")).toHaveTextContent("Characters: 120 → 95");
  });

  it.each([
    ["no meta", undefined],
    ["no counts", { unit: "code_points" }],
    ["non-numeric counts", { unit: "code_points", chars_before: "9", chars_after: "10" }],
    ["negative counts", { unit: "code_points", chars_before: -1, chars_after: 3 }],
    ["an unknown unit", { unit: "bytes", chars_before: 1, chars_after: 2 }],
  ])("shows no counts for %s", (_label, meta) => {
    open({ ...change, meta });
    expect(screen.queryByTestId("config-change-counts")).toBeNull();
  });

  it("ignores a non-numeric cap", () => {
    open({ ...change, meta: { unit: "code_points", chars_before: 1, chars_after: 2, cap: "ten" } });
    expect(screen.getByTestId("config-change-counts")).toHaveTextContent("Characters: 1 → 2");
    expect(screen.getByTestId("config-change-counts")).not.toHaveTextContent("allowed");
  });
});
