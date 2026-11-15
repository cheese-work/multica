// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { setCurrentWorkspace } from "@multica/core/platform";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSkills from "../../locales/en/skills.json";

const createSkill = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/api", () => ({ api: { createSkill } }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { CreateSkillDialog } from "./create-skill-dialog";

function mount() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <I18nProvider locale="en" resources={{ en: { common: enCommon, skills: enSkills } }}>
      <QueryClientProvider client={qc}>
        <CreateSkillDialog onClose={vi.fn()} onCreated={vi.fn()} />
      </QueryClientProvider>
    </I18nProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: /Create manually/ }));
  return view;
}

beforeEach(async () => {
  localStorage.clear();
  resetAllRegisteredDrafts();
  setCurrentWorkspace("acme", "ws-1");
  await new Promise((r) => queueMicrotask(() => r(null)));
  createSkill.mockReset();
});
afterEach(cleanup);

describe("CreateSkillDialog manual form draft", () => {
  it("keeps the typed name and description across a dialog close and reopen", () => {
    const first = mount();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "review-helper" } });
    fireEvent.change(screen.getByLabelText(/Description/), { target: { value: "Reviews diffs" } });
    first.unmount();

    mount();
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("review-helper");
    expect((screen.getByLabelText(/Description/) as HTMLTextAreaElement).value).toBe("Reviews diffs");
  });

  it("keeps the draft when the create fails and clears it when the create succeeds", async () => {
    createSkill.mockRejectedValueOnce(new Error("boom"));
    const first = mount();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "review-helper" } });
    fireEvent.keyDown(screen.getByLabelText("Name"), { key: "Enter" });
    await waitFor(() => expect(screen.getByRole("alert")).toBeTruthy());
    first.unmount();

    createSkill.mockResolvedValueOnce({ id: "s1", name: "review-helper", description: "", content: "", files: [] });
    const second = mount();
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("review-helper");
    fireEvent.keyDown(screen.getByLabelText("Name"), { key: "Enter" });
    await waitFor(() => expect(createSkill).toHaveBeenCalledTimes(2));
    second.unmount();

    mount();
    expect((screen.getByLabelText("Name") as HTMLInputElement).value).toBe("");
  });
});
