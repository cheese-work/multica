// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import type { Squad } from "@multica/core/types";
import {
  AddMemberDialog,
  RoleEditor,
  SquadDescriptionEditor,
  SquadInstructionsSection,
  SquadNameEditor,
} from "./squad-detail-page";

// Mirrors the real ContentEditor contract: `value` is read at mount, `onUpdate`
// is debounced, and a pending update is dropped on unmount unless the host opts
// in with `flushPendingOnUnmount` (editor/content-editor.tsx).
vi.mock("../../editor/content-editor", async () => {
  const { useEffect, useRef } = await import("react");
  return {
    ContentEditor: ({
      value,
      onUpdate,
      debounceMs = 0,
      flushPendingOnUnmount = false,
    }: {
      value: string;
      onUpdate: (v: string) => void;
      debounceMs?: number;
      flushPendingOnUnmount?: boolean;
    }) => {
      const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
      const pending = useRef<string | null>(null);
      const onUpdateRef = useRef(onUpdate);
      onUpdateRef.current = onUpdate;
      const flushRef = useRef(flushPendingOnUnmount);
      flushRef.current = flushPendingOnUnmount;
      useEffect(
        () => () => {
          clearTimeout(timer.current);
          if (flushRef.current && pending.current !== null) onUpdateRef.current(pending.current);
        },
        [],
      );
      return (
        <textarea
          aria-label="editor"
          defaultValue={value}
          onChange={(e) => {
            pending.current = e.target.value;
            clearTimeout(timer.current);
            timer.current = setTimeout(() => {
              const v = pending.current;
              pending.current = null;
              if (v !== null) onUpdateRef.current(v);
            }, debounceMs);
          }}
        />
      );
    },
  };
});
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => null }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

// The section shows saved text read-only until Edit; an unsent draft reopens it.
function SquadInstructionsTab(props: React.ComponentProps<typeof SquadInstructionsSection>) {
  return <SquadInstructionsSection {...props} />;
}
const openEditor = () => {
  if (!screen.queryByLabelText("editor")) fireEvent.click(screen.getByRole("button", { name: "Edit" }));
};

const renderTab = (ui: React.ReactElement) => {
  const r = renderWithI18n(ui);
  openEditor();
  return r;
};

const squad = { id: "squad-1", instructions: "saved" } as Squad;

beforeEach(() => resetAllRegisteredDrafts());
afterEach(() => vi.useRealTimers());

const type = (label: string, value: string) =>
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
const flushDebounce = () => act(() => vi.advanceTimersByTimeAsync(200));

describe("squad detail drafts survive unmount and remount", () => {
  it("instructions tab", async () => {
    vi.useFakeTimers();
    const props = { squad, canManage: true, onSave: vi.fn() };
    const first = renderTab(<SquadInstructionsTab {...props} />);
    type("editor", "unsent instructions");
    await flushDebounce();
    first.unmount();

    renderTab(<SquadInstructionsTab {...props} />);
    expect(screen.getByLabelText("editor")).toHaveValue("unsent instructions");
  });

  it("instructions typed inside the debounce window survive an immediate unmount", () => {
    vi.useFakeTimers();
    const props = { squad, canManage: true, onSave: vi.fn() };
    const first = renderTab(<SquadInstructionsTab {...props} />);
    type("editor", "typed just before a tab change");
    first.unmount();

    renderTab(<SquadInstructionsTab {...props} />);
    expect(screen.getByLabelText("editor")).toHaveValue("typed just before a tab change");
  });

  it("cancel inside the debounce window discards the typed text", async () => {
    vi.useFakeTimers();
    const props = { squad, canManage: true, onSave: vi.fn() };
    renderTab(<SquadInstructionsTab {...props} />);
    type("editor", "typed then cancelled");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await flushDebounce();

    expect(screen.queryByLabelText("editor")).not.toBeInTheDocument();
    expect(screen.queryByText("Unsaved changes")).not.toBeInTheDocument();
    openEditor();
    expect(screen.getByLabelText("editor")).toHaveValue("saved");
  });

  it("instructions edited while a save is pending are not erased when it succeeds", async () => {
    vi.useFakeTimers();
    let resolveSave!: () => void;
    const onSave = vi.fn(() => new Promise<void>((r) => (resolveSave = r)));
    const props = { squad, canManage: true, onSave };
    const first = renderTab(<SquadInstructionsTab {...props} />);
    type("editor", "A");
    await flushDebounce();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    type("editor", "AB");
    await flushDebounce();
    await act(async () => resolveSave());

    expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
    first.unmount();
    renderTab(<SquadInstructionsTab {...props} />);
    expect(screen.getByLabelText("editor")).toHaveValue("AB");
  });

  it("instructions reverted to the old saved text during a pending save stay unsent", async () => {
    vi.useFakeTimers();
    let resolveSave!: () => void;
    const onSave = vi.fn(() => new Promise<void>((r) => (resolveSave = r)));
    const first = renderTab(<SquadInstructionsTab squad={squad} canManage onSave={onSave} />);
    type("editor", "new");
    await flushDebounce();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    type("editor", "saved");
    await flushDebounce();
    await act(async () => resolveSave());
    // The server now holds "new"; the reversal back to "saved" was never sent.
    const saved = { ...squad, instructions: "new" } as Squad;
    first.rerender(<SquadInstructionsTab squad={saved} canManage onSave={onSave} />);
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
    first.unmount();

    renderTab(<SquadInstructionsTab squad={saved} canManage onSave={onSave} />);
    expect(screen.getByLabelText("editor")).toHaveValue("saved");
  });

  it("a revert typed before the save finishes but flushed after it stays unsent while the saved prop is stale", async () => {
    vi.useFakeTimers();
    let resolveSave!: () => void;
    const onSave = vi.fn(() => new Promise<void>((r) => (resolveSave = r)));
    const first = renderTab(<SquadInstructionsTab squad={squad} canManage onSave={onSave} />);
    type("editor", "new");
    await flushDebounce();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    type("editor", "saved"); // debounce still pending
    await act(async () => resolveSave()); // save ends first; saved prop is still "saved"
    await flushDebounce(); // the editor update lands after the save, against the stale prop
    const saved = { ...squad, instructions: "new" } as Squad;
    first.rerender(<SquadInstructionsTab squad={saved} canManage onSave={onSave} />);
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
    first.unmount();

    renderTab(<SquadInstructionsTab squad={saved} canManage onSave={onSave} />);
    expect(screen.getByLabelText("editor")).toHaveValue("saved");
  });

  it("instructions tab keeps the draft when save fails and clears it on success", async () => {
    vi.useFakeTimers();
    const onSave = vi.fn().mockRejectedValueOnce(new Error("boom")).mockResolvedValueOnce(undefined);
    const props = { squad, canManage: true, onSave };
    const first = renderTab(<SquadInstructionsTab {...props} />);
    type("editor", "new");
    await flushDebounce();
    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Save" })));
    expect(onSave).toHaveBeenCalledTimes(1);
    first.unmount();
    const second = renderTab(<SquadInstructionsTab {...props} />);
    expect(screen.getByLabelText("editor")).toHaveValue("new");

    await act(async () => fireEvent.click(screen.getByRole("button", { name: "Save" })));
    expect(onSave).toHaveBeenCalledTimes(2);
    expect(screen.queryByText("Unsaved changes")).toBeNull();
    second.unmount();
    renderTab(<SquadInstructionsTab {...props} />);
    expect(screen.getByLabelText("editor")).toHaveValue("saved");
  });

  it("name popover", () => {
    const props = { squadId: "squad-1", value: "Alpha", onSave: vi.fn() };
    const first = renderWithI18n(<SquadNameEditor {...props} />);
    fireEvent.click(screen.getByText("Alpha"));
    fireEvent.change(screen.getByPlaceholderText("Squad name"), { target: { value: "Beta" } });
    first.unmount();

    renderWithI18n(<SquadNameEditor {...props} />);
    fireEvent.click(screen.getByText("Alpha"));
    expect(screen.getByPlaceholderText("Squad name")).toHaveValue("Beta");
  });

  it("description dialog", () => {
    const props = { squadId: "squad-1", value: "old", onSave: vi.fn() };
    const first = renderWithI18n(<SquadDescriptionEditor {...props} />);
    fireEvent.click(screen.getByText("old"));
    fireEvent.change(screen.getByPlaceholderText("What is this squad responsible for?"), {
      target: { value: "unsent description" },
    });
    first.unmount();

    renderWithI18n(<SquadDescriptionEditor {...props} />);
    fireEvent.click(screen.getByText("old"));
    expect(screen.getByPlaceholderText("What is this squad responsible for?")).toHaveValue(
      "unsent description",
    );
  });

  it("member role editor reopens in edit mode with the draft", () => {
    const props = { draftKey: "squad-1:role:agent:a1", value: "Dev", onSave: vi.fn() };
    const first = renderWithI18n(<RoleEditor {...props} />);
    fireEvent.click(screen.getByText("· Dev"));
    fireEvent.change(screen.getByPlaceholderText("Role (e.g. Reviewer)"), {
      target: { value: "Lead" },
    });
    first.unmount();

    renderWithI18n(<RoleEditor {...props} />);
    expect(screen.getByPlaceholderText("Role (e.g. Reviewer)")).toHaveValue("Lead");
  });

  it("add-member dialog role", () => {
    const props = {
      squadId: "squad-1",
      availableMembers: [],
      availableAgents: [],
      onClose: vi.fn(),
      onSubmit: vi.fn(),
    };
    const first = renderWithI18n(<AddMemberDialog {...props} />);
    fireEvent.change(screen.getByPlaceholderText("e.g. Reviewer, Frontend Lead"), {
      target: { value: "Reviewer" },
    });
    first.unmount();

    renderWithI18n(<AddMemberDialog {...props} />);
    expect(screen.getByPlaceholderText("e.g. Reviewer, Frontend Lead")).toHaveValue("Reviewer");
  });
});
