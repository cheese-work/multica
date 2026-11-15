import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { useModalStore } from "@multica/core/modals";

// A modal module's factory runs when the module is first imported.
const loaded = vi.hoisted(() => ({ names: [] as string[] }));

vi.mock("./create-issue-dialog", () => {
  loaded.names.push("create-issue-dialog");
  return { CreateIssueDialog: () => <div>create-issue-dialog</div> };
});
vi.mock("./create-project", () => {
  loaded.names.push("create-project");
  return { CreateProjectModal: () => <div>create-project</div> };
});
vi.mock("./create-squad", () => {
  loaded.names.push("create-squad");
  return { CreateSquadModal: () => <div>create-squad</div> };
});
vi.mock("./feedback", () => {
  loaded.names.push("feedback");
  return { FeedbackModal: () => <div>feedback</div> };
});
vi.mock("./issue-limit-upgrade-dialog", () => ({
  IssueLimitUpgradeDialog: () => null,
}));

import { ModalRegistry } from "./registry";

afterEach(() => {
  cleanup();
  useModalStore.getState().close();
});

describe("ModalRegistry", () => {
  it("does not load any modal code until a modal is opened", () => {
    render(<ModalRegistry />);

    expect(loaded.names).toEqual([]);
  });

  it("loads and shows a modal when it opens", async () => {
    render(<ModalRegistry />);

    act(() => useModalStore.getState().open("create-project"));

    expect(await screen.findByText("create-project")).toBeTruthy();
    expect(loaded.names).toEqual(["create-project"]);
  });
});
