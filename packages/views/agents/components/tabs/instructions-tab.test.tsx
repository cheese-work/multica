// @vitest-environment jsdom

import { describe, expect, it, vi, afterEach } from "vitest";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { cleanup, act, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Agent } from "@multica/core/types";
import { renderWithI18n } from "../../../test/i18n";
import { InstructionsTab } from "./instructions-tab";

const baseAgent: Agent = {
  id: "agent-1",
  workspace_id: "ws-1",
  runtime_id: "runtime-1",
  name: "Reviewer",
  description: "",
  instructions: "Review carefully.",
  conversation_starters: [{ label: "Review a PR", prompt: "Review it." }],
  avatar_url: null,
  runtime_mode: "local",
  runtime_config: {},
  custom_args: [],
  visibility: "workspace",
  permission_mode: "public_to",
  invocation_targets: [{ target_type: "workspace", target_id: null }],
  status: "idle",
  max_concurrent_tasks: 1,
  model: "",
  owner_id: "user-1",
  skills: [],
  created_at: "2026-08-24T00:00:00Z",
  updated_at: "2026-08-24T00:00:00Z",
  archived_at: null,
  archived_by: null,
};

function tab(agent: Agent, onSave = vi.fn().mockResolvedValue(undefined)) {
  return <InstructionsTab agent={agent} onSave={onSave} />;
}

async function typeInstructions(text: string) {
  const user = userEvent.setup();
  const field = screen.getByLabelText("System prompt");
  await user.clear(field);
  await user.type(field, text);
  return user;
}

// Unmount first: a mounted draft hook would write its draft back after the reset.
afterEach(() => {
  cleanup();
  resetAllRegisteredDrafts();
});

describe("InstructionsTab", () => {
  it("saves only the instructions; starters live in General settings", async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    renderWithI18n(tab(baseAgent, onSave));

    expect(screen.queryByText("Conversation starters")).toBeNull();
    const user = await typeInstructions("Updated instructions.");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(onSave).toHaveBeenCalledWith({
      instructions: "Updated instructions.",
    });
  });

  it("discards unsaved edits back to the saved instructions", async () => {
    renderWithI18n(tab(baseAgent));

    const user = await typeInstructions("Scratch that.");
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Discard" }));

    expect(screen.getByLabelText("System prompt")).toHaveValue(
      "Review carefully.",
    );
    expect(screen.queryByText("Unsaved changes")).toBeNull();
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("counts characters and previews an empty prompt without an editor", async () => {
    renderWithI18n(tab({ ...baseAgent, instructions: "" }));

    expect(screen.getByText("0 characters")).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole("tab", { name: "Preview" }));

    expect(screen.queryByLabelText("System prompt")).toBeNull();
    expect(screen.getByText("Nothing to preview yet.")).toBeInTheDocument();
  });

  it("preserves an unsaved edit when the server copy changes", async () => {
    const { rerender } = renderWithI18n(tab(baseAgent));
    await typeInstructions("Local draft.");

    rerender(tab({ ...baseAgent, instructions: "Server-side change." }));

    expect(screen.getByLabelText("System prompt")).toHaveValue("Local draft.");
  });

  it("adopts a server change while the editor is clean", () => {
    const { rerender } = renderWithI18n(tab(baseAgent));

    rerender(tab({ ...baseAgent, instructions: "Server-side change." }));

    expect(screen.getByLabelText("System prompt")).toHaveValue(
      "Server-side change.",
    );
  });

  it("keeps submitted edits when an optimistic update rolls back", async () => {
    let rejectSave!: (reason?: unknown) => void;
    const onSave = vi.fn(
      () =>
        new Promise<void>((_, reject) => {
          rejectSave = reject;
        }),
    );
    const { rerender } = renderWithI18n(tab(baseAgent, onSave));
    const user = await typeInstructions("Inspect the patch.");
    await user.click(screen.getByRole("button", { name: "Save" }));

    rerender(tab({ ...baseAgent, instructions: "Inspect the patch." }, onSave));
    rerender(tab(baseAgent, onSave));
    await act(async () => rejectSave(new Error("Update failed")));

    expect(screen.getByLabelText("System prompt")).toHaveValue(
      "Inspect the patch.",
    );
  });

  it("keeps an unsent edit across unmount and remount, and clears it after a successful save", async () => {
    const agent = { ...baseAgent, id: "agent-remount" };
    const first = renderWithI18n(tab(agent));
    await typeInstructions("Half-written prompt");
    first.unmount();

    const onSave = vi.fn().mockResolvedValue(undefined);
    const second = renderWithI18n(tab(agent, onSave));
    const user = userEvent.setup();
    expect(screen.getByLabelText("System prompt")).toHaveValue("Half-written prompt");

    await user.click(screen.getByRole("button", { name: "Save" }));
    second.unmount();
    renderWithI18n(tab(agent));
    expect(screen.getByLabelText("System prompt")).toHaveValue("Review carefully.");
  });

  it("keeps the edit when the save fails, and drops it on discard", async () => {
    const agent = { ...baseAgent, id: "agent-fail" };
    const failing = vi.fn().mockRejectedValue(new Error("nope"));
    const first = renderWithI18n(tab(agent, failing));
    const user = await typeInstructions("Will not save");
    await user.click(screen.getByRole("button", { name: "Save" }));
    first.unmount();

    const second = renderWithI18n(tab(agent));
    expect(screen.getByLabelText("System prompt")).toHaveValue("Will not save");
    await user.click(screen.getByRole("button", { name: "Discard" }));
    second.unmount();
    renderWithI18n(tab(agent));
    expect(screen.getByLabelText("System prompt")).toHaveValue("Review carefully.");
  });

  it("keeps an edit typed while an earlier save is still in flight", async () => {
    const agent = { ...baseAgent, id: "agent-inflight" };
    let resolveSave!: () => void;
    const onSave = vi.fn(() => new Promise<void>((resolve) => { resolveSave = resolve; }));
    const first = renderWithI18n(tab(agent, onSave));
    const user = await typeInstructions("First version");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await user.type(screen.getByLabelText("System prompt"), " plus newer");
    await act(async () => resolveSave());
    first.unmount();

    renderWithI18n(tab(agent));
    expect(screen.getByLabelText("System prompt")).toHaveValue("First version plus newer");
  });
});
