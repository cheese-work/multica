// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { Skill } from "@multica/core/types";
import { resetAllRegisteredDrafts } from "@multica/core/drafts";
import { useSkillEditDraftStore } from "@multica/core/skills/draft-stores";
import { setCurrentWorkspace } from "@multica/core/platform";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSkills from "../../locales/en/skills.json";
import { NavigationProvider, type NavigationAdapter } from "../../navigation";

const skill: Skill = {
  id: "skill-1",
  workspace_id: "ws-1",
  name: "animations",
  description: "Animate things",
  config: {},
  created_by: "user-1",
  created_at: "2026-07-28T18:11:37Z",
  updated_at: "2026-07-28T18:14:40Z",
  content: "# Animations\n",
  files: [],
};

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "ws-1" }));
vi.mock("@multica/core/workspace/queries", () => ({
  skillDetailOptions: (wsId: string, id: string) => ({
    queryKey: ["skill", wsId, id],
    queryFn: () => Promise.resolve(skill),
  }),
  agentListOptions: (wsId: string) => ({ queryKey: ["agents", wsId], queryFn: () => Promise.resolve([]) }),
  memberListOptions: (wsId: string) => ({
    queryKey: ["members", wsId],
    queryFn: () => Promise.resolve([{ user_id: "user-1", role: "member", name: "Ada", email: "ada@example.com", avatar_url: null }]),
  }),
  selectSkillAssignments: () => new Map(),
  skillVersionsOptions: (wsId: string, id: string) => ({
    queryKey: ["skill-versions", wsId, id],
    queryFn: () => Promise.resolve([]),
  }),
  workspaceKeys: { skills: (wsId: string) => ["skills", wsId], agents: (wsId: string) => ["agents", wsId] },
}));
vi.mock("@multica/core/runtimes", () => ({
  runtimeListOptions: (wsId: string) => ({ queryKey: ["runtimes", wsId], queryFn: () => Promise.resolve([]) }),
  runtimeDisplayLabel: () => "runtime",
}));
vi.mock("@multica/core/auth", () => {
  const state = () => ({ user: { id: "user-1" } });
  return { useAuthStore: Object.assign((s?: (x: ReturnType<typeof state>) => unknown) => (s ? s(state()) : state()), { getState: state }) };
});
vi.mock("@multica/core/paths", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@multica/core/paths")>()),
  useWorkspacePaths: () => ({ skills: () => "/acme/skills" }),
}));
vi.mock("@multica/core/permissions", () => ({
  useSkillPermissions: () => ({ canEdit: { allowed: true, reason: null } }),
}));
vi.mock("@multica/core/workspace/avatar-url", () => ({ resolvePublicFileUrl: (v: string | null) => v }));
vi.mock("@multica/core/api", () => ({ api: { updateSkill: vi.fn(), deleteSkill: vi.fn() } }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("../hooks/use-can-edit-skill", () => ({ useCanEditSkill: () => true }));
vi.mock("../../rich-content", () => ({ RichContent: ({ content }: { content: string }) => <div>{content}</div> }));
vi.mock("../../labels/resource-label-picker", () => ({ ResourceLabelPicker: () => <div /> }));
vi.mock("./skill-list-actions", () => ({ AddToAgentDialog: () => null }));
vi.mock("@multica/ui/components/common/capability-banner", () => ({ CapabilityBanner: () => <div /> }));
vi.mock("@multica/ui/components/common/actor-avatar", () => ({ ActorAvatar: () => <div /> }));

import { SkillDetailPage } from "./skill-detail-page";

function mount() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const navigation: NavigationAdapter = {
    push: vi.fn(),
    replace: vi.fn(),
    back: vi.fn(),
    pathname: "/acme/skills/skill-1",
    searchParams: new URLSearchParams(),
    hash: "",
    getShareableUrl: (path) => path,
  };
  return render(
    <I18nProvider locale="en" resources={{ en: { common: enCommon, skills: enSkills } }}>
      <NavigationProvider value={navigation}>
        <QueryClientProvider client={qc}>
          <SkillDetailPage skillId="skill-1" />
        </QueryClientProvider>
      </NavigationProvider>
    </I18nProvider>,
  );
}

beforeEach(async () => {
  localStorage.clear();
  resetAllRegisteredDrafts();
  setCurrentWorkspace("acme", "ws-1");
  await new Promise((r) => queueMicrotask(() => r(null)));
});
afterEach(cleanup);

describe("SkillDetailPage unsaved edits", () => {
  it("keeps edited name and description across a remount, and Discard drops them", async () => {
    const first = mount();
    const name = (await screen.findByDisplayValue("animations")) as HTMLInputElement;
    fireEvent.change(name, { target: { value: "animations-v2" } });
    // 1.0 layout: the description edits inside a popover opened from its value.
    fireEvent.click(screen.getByRole("button", { name: "Animate things" }));
    fireEvent.change(await screen.findByRole("textbox", { name: "Description" }), { target: { value: "Edited" } });
    first.unmount();

    const second = mount();
    expect(await screen.findByDisplayValue("animations-v2")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Edited" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /Discard/ }));
    second.unmount();

    mount();
    expect(await screen.findByDisplayValue("animations")).toBeTruthy();
  });

  it("keeps the original base version of a restored stale draft, so the conflict survives a later visit", async () => {
    const staleBase = "2026-07-28T18:00:00Z";
    useSkillEditDraftStore.getState().setDraft("skill-1", {
      name: "animations-mine",
      description: "Animate things",
      content: "# Animations\n",
      files: [],
      baseUpdatedAt: staleBase,
    });

    const first = mount();
    expect(await screen.findByDisplayValue("animations-mine")).toBeTruthy();
    expect(await screen.findByText("Someone else updated this skill")).toBeTruthy();
    expect(useSkillEditDraftStore.getState().getDraft("skill-1")?.baseUpdatedAt).toBe(staleBase);
    first.unmount();

    mount();
    expect(await screen.findByText("Someone else updated this skill")).toBeTruthy();
  });
});
