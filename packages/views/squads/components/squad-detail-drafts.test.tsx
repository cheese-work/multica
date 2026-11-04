// @vitest-environment jsdom

import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import type { Squad } from "@multica/core/types";
import {
  AddMemberDialog,
  RoleEditor,
  SquadDescriptionEditor,
  SquadInstructionsTab,
  SquadNameEditor,
} from "./squad-detail-page";

// ContentEditor reads `value` at mount only, like the real one.
vi.mock("../../editor/content-editor", () => ({
  ContentEditor: ({ value, onUpdate }: { value: string; onUpdate: (v: string) => void }) => (
    <textarea aria-label="editor" defaultValue={value} onChange={(e) => onUpdate(e.target.value)} />
  ),
}));
vi.mock("../../common/actor-avatar", () => ({ ActorAvatar: () => null }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const squad = { id: "squad-1", instructions: "saved" } as Squad;

beforeEach(() => resetAllRegisteredDrafts());

describe("squad detail drafts survive unmount and remount", () => {
  it("instructions tab", () => {
    const props = { squad, canManage: true, onSave: vi.fn() };
    const first = renderWithI18n(<SquadInstructionsTab {...props} />);
    fireEvent.change(screen.getByLabelText("editor"), { target: { value: "unsent instructions" } });
    first.unmount();

    renderWithI18n(<SquadInstructionsTab {...props} />);
    expect(screen.getByLabelText("editor")).toHaveValue("unsent instructions");
  });

  it("instructions tab keeps the draft when save fails and clears it on success", async () => {
    const onSave = vi.fn().mockRejectedValueOnce(new Error("boom")).mockResolvedValueOnce(undefined);
    const props = { squad, canManage: true, onSave };
    const first = renderWithI18n(<SquadInstructionsTab {...props} />);
    fireEvent.change(screen.getByLabelText("editor"), { target: { value: "new" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await vi.waitFor(() => expect(onSave).toHaveBeenCalledTimes(1));
    first.unmount();
    const second = renderWithI18n(<SquadInstructionsTab {...props} />);
    expect(screen.getByLabelText("editor")).toHaveValue("new");

    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await vi.waitFor(() => expect(onSave).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(screen.queryByText("Unsaved changes")).toBeNull());
    second.unmount();
    renderWithI18n(<SquadInstructionsTab {...props} />);
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
    fireEvent.click(screen.getByText("Dev"));
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
