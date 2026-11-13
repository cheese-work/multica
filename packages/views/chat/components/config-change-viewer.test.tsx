import { screen } from "@testing-library/react";
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
const tab = (name: RegExp | string) => screen.getByRole("tab", { name });

describe("ConfigChangeViewer", () => {
  it("names the field, the target and the size of the change, collapsed by default", () => {
    open();
    const viewer = screen.getByTestId("config-change-viewer");
    expect(viewer).toHaveTextContent("Workflow step instructions");
    expect(viewer).toHaveTextContent("wf-1");
    expect(tab(/Changes/)).toHaveTextContent("+2 −1");
    expect(tab(/Changes/)).toHaveAttribute("aria-selected", "false");
    expect(screen.queryByRole("region")).toBeNull();
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

  it("shows the server diff when the Changes tab opens, with line prefixes as marks", async () => {
    const user = userEvent.setup();
    open();
    await user.click(tab(/Changes/));
    const region = screen.getByRole("region", { name: "Changes to Workflow step instructions" });
    expect(region).toHaveAttribute("tabindex", "0");
    expect(region).toHaveTextContent("old line");
    expect(region).toHaveTextContent("new line");
    expect(region).toHaveTextContent("added line");
    expect(tab(/Changes/)).toHaveAttribute("aria-selected", "true");
  });

  it("shows the value that will be written on the Result tab", async () => {
    const user = userEvent.setup();
    open();
    await user.click(tab("Result"));
    const region = screen.getByRole("region", { name: "Result after approval" });
    expect(region).toHaveAttribute("tabindex", "0");
    expect(region.textContent).toBe("keep\nnew line\nadded line");
  });

  it.each([
    ["lines", ["a", "b"], "a\nb"],
    ["json", { a: 1 }, '{\n  "a": 1\n}'],
  ] as const)("renders a %s result", async (diff_kind, after, text) => {
    const user = userEvent.setup();
    open({ ...change, diff_kind, after });
    await user.click(tab("Result"));
    expect(screen.getByRole("region", { name: "Result after approval" }).textContent).toBe(text);
  });

  it("renders the value as text, never as markup", async () => {
    const user = userEvent.setup();
    const { container } = open({ ...change, after: '<img src=x onerror="alert(1)"> **bold**' });
    await user.click(tab("Result"));
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByRole("region", { name: "Result after approval" }).textContent).toBe('<img src=x onerror="alert(1)"> **bold**');
  });

  it("links each tab to the panel and the panel back to the open tab", async () => {
    const user = userEvent.setup();
    open();
    const panel = screen.getByTestId("config-change-panel");
    expect(panel).toHaveAttribute("role", "tabpanel");
    for (const name of [/Changes/, "Result"]) expect(tab(name)).toHaveAttribute("aria-controls", panel.id);
    await user.click(tab("Result"));
    expect(panel).toHaveAttribute("aria-labelledby", tab("Result").id);
    expect(panel).not.toHaveAttribute("hidden");
    await user.click(tab(/Changes/));
    expect(panel).toHaveAttribute("aria-labelledby", tab(/Changes/).id);
  });

  it("puts only one tab in the tab order and moves focus with the arrow, Home and End keys", async () => {
    const user = userEvent.setup();
    open();
    expect(tab(/Changes/)).toHaveAttribute("tabindex", "0");
    expect(tab("Result")).toHaveAttribute("tabindex", "-1");
    tab(/Changes/).focus();
    await user.keyboard("{ArrowRight}");
    expect(tab("Result")).toHaveFocus();
    expect(tab("Result")).toHaveAttribute("tabindex", "0");
    expect(tab(/Changes/)).toHaveAttribute("tabindex", "-1");
    await user.keyboard("{ArrowRight}");
    expect(tab(/Changes/)).toHaveFocus();
    await user.keyboard("{ArrowLeft}");
    expect(tab("Result")).toHaveFocus();
    await user.keyboard("{Home}");
    expect(tab(/Changes/)).toHaveFocus();
    await user.keyboard("{End}");
    expect(tab("Result")).toHaveFocus();
  });

  it("closes the viewer when the open tab is selected again", async () => {
    const user = userEvent.setup();
    open();
    await user.click(tab(/Changes/));
    expect(screen.getByRole("region")).toBeInTheDocument();
    await user.click(tab(/Changes/));
    expect(screen.queryByRole("region")).toBeNull();
    expect(tab(/Changes/)).toHaveAttribute("aria-selected", "false");
  });

  it("lists the settings keys of a settings change", () => {
    open({ ...change, field: "workspace.settings", keys: ["compact_prompt", "auto_compact"] });
    expect(screen.getByTestId("config-change-viewer")).toHaveTextContent("Workspace settings");
    expect(screen.getByTestId("config-change-keys")).toHaveTextContent("compact_prompt, auto_compact");
  });

  it("says so when no line changes, and when only whitespace changes", async () => {
    const user = userEvent.setup();
    const view = open({ ...change, diff: [] });
    await user.click(tab(/Changes/));
    expect(screen.getByText("No line changes.")).toBeInTheDocument();
    view.unmount();
    open({ ...change, diff: [], meta: { whitespace_only: true } });
    await user.click(tab(/Changes/));
    expect(screen.getByText("Only whitespace or line endings change.")).toBeInTheDocument();
  });

  it("shows a hidden-lines row between distant hunks", async () => {
    const user = userEvent.setup();
    open({ ...change, diff: [{ old_start: 1, new_start: 1, lines: ["-a", "+b"] }, { old_start: 20, new_start: 20, lines: ["-c", "+d"] }] });
    await user.click(tab(/Changes/));
    expect(screen.getByText(/lines? hidden/)).toBeInTheDocument();
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

  it("shows exactly the added and removed skills of a list diff, one skill per line", async () => {
    const user = userEvent.setup();
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
    expect(tab(/Changes/)).toHaveTextContent("+1 −1");
    await user.click(tab(/Changes/));
    const region = screen.getByRole("region", { name: "Changes to Agent skills" });
    expect(region).toHaveTextContent(skills[1]!);
    expect(region).toHaveTextContent(skills[2]!);
    expect(region.querySelectorAll("code")).toHaveLength(3);
  });

  it("keeps a long unbroken line inside a scrolling, wrapping panel", async () => {
    const user = userEvent.setup();
    const long = "x".repeat(10_000);
    open({ ...change, diff: [{ old_start: 1, new_start: 1, lines: [`-${long}`, `+${long}y`] }], after: `${long}y` });
    await user.click(tab(/Changes/));
    const region = screen.getByRole("region", { name: "Changes to Workflow step instructions" });
    expect(region).toHaveClass("overflow-auto");
    expect(region.querySelector("code")?.parentElement).toHaveClass("break-words");
    expect(screen.getByTestId("config-change-panel")).toHaveClass("max-h-72");
    await user.click(tab("Result"));
    const result = screen.getByRole("region", { name: "Result after approval" });
    expect(result).toHaveClass("overflow-auto", "break-words");
  });
});
