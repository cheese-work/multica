// @vitest-environment jsdom
import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const disk = vi.hoisted(() => new Map<string, string>());
vi.mock("@react-native-async-storage/async-storage", () => ({
  default: {
    getItem: async (k: string) => disk.get(k) ?? null,
    setItem: async (k: string, v: string) => void disk.set(k, v),
    removeItem: async (k: string) => void disk.delete(k),
    getAllKeys: async () => [...disk.keys()],
    multiGet: async (ks: string[]) =>
      ks.map((k) => [k, disk.get(k) ?? null] as [string, string | null]),
    multiRemove: async (ks: string[]) => void ks.forEach((k) => disk.delete(k)),
  },
}));

import { clearAllDrafts } from "../draft-persistence";
import {
  NEW_ISSUE_KEY,
  readFormDraft,
  useFormDraftsStore,
} from "./form-drafts-store";
import {
  useIssueEditDraft,
  useNewIssueDraft,
  useNewProjectDraft,
  useProjectEditDraft,
} from "./use-form-drafts";
import { useNewIssueDraftStore } from "./new-issue-draft-store";
import { useNewProjectDraftStore } from "./new-project-draft-store";
import type { Project } from "@multica/core/types";

const SCOPE = "u:ws1";
const issueRecord = { title: "Server title", description: "server body" };
const projectRecord = { title: "Server project", description: "", icon: "📦" };

const settle = () => act(() => vi.advanceTimersByTimeAsync(1000));
// App cold start: in-memory state is gone, the disk stays.
const coldStart = () => act(async () => useFormDraftsStore.getState().reset());
const hydrate = () => act(() => useFormDraftsStore.getState().hydrate(SCOPE));

describe("form screens keep unsent input across unmount and remount", () => {
  beforeEach(async () => {
    disk.clear();
    vi.useFakeTimers();
    useFormDraftsStore.getState().reset();
    useNewIssueDraftStore.getState().reset();
    useNewProjectDraftStore.getState().reset();
    await hydrate();
  });
  afterEach(() => vi.useRealTimers());

  it("issue edit: typed title and description come back, also after a cold start", async () => {
    const first = renderHook(() => useIssueEditDraft("i1", issueRecord));
    expect(first.result.current.restored).toBe(true);
    act(() => first.result.current.setTitle("My edit"));
    act(() => first.result.current.description.setText("half-typed"));
    await settle();
    first.unmount();

    const second = renderHook(() => useIssueEditDraft("i1", issueRecord));
    expect(second.result.current.title).toBe("My edit");
    expect(second.result.current.currentDescription).toBe("half-typed");
    expect(second.result.current.dirty).toBe(true);
    second.unmount();

    await coldStart();
    await hydrate();
    const third = renderHook(() => useIssueEditDraft("i1", issueRecord));
    expect(third.result.current.title).toBe("My edit");
    expect(third.result.current.currentDescription).toBe("half-typed");
  });

  it("issue edit: a successful save clears the draft, a failed save keeps it", async () => {
    const first = renderHook(() => useIssueEditDraft("i1", issueRecord));
    act(() => first.result.current.setTitle("My edit"));
    await settle();
    first.unmount(); // failed save: the screen only unmounts

    const second = renderHook(() => useIssueEditDraft("i1", issueRecord));
    expect(second.result.current.title).toBe("My edit");
    act(() => second.result.current.settle()); // successful save
    await settle();
    second.unmount();

    await coldStart();
    await hydrate();
    const third = renderHook(() => useIssueEditDraft("i1", issueRecord));
    expect(third.result.current.title).toBe("Server title");
    expect(third.result.current.dirty).toBe(false);
  });

  it("project edit: title, description and icon come back", async () => {
    const first = renderHook(() => useProjectEditDraft("p1", projectRecord));
    act(() => {
      first.result.current.setTitle("Renamed");
      first.result.current.setDescription("notes");
      first.result.current.setIcon("🚀");
    });
    await settle();
    first.unmount();

    await coldStart();
    await hydrate();
    const second = renderHook(() => useProjectEditDraft("p1", projectRecord));
    expect(second.result.current.title).toBe("Renamed");
    expect(second.result.current.description).toBe("notes");
    expect(second.result.current.icon).toBe("🚀");
    act(() => second.result.current.settle());
    await settle();
    second.unmount();
    const third = renderHook(() => useProjectEditDraft("p1", projectRecord));
    expect(third.result.current.title).toBe("Server project");
  });

  it("new issue: title, description and chips come back", async () => {
    const first = renderHook(() => useNewIssueDraft());
    act(() => {
      first.result.current.setTitle("Bug");
      first.result.current.description.setText("steps");
      useNewIssueDraftStore.getState().setPriority("high");
    });
    await settle();
    first.unmount();
    useNewIssueDraftStore.getState().reset(); // chips are in-memory state

    await coldStart();
    await hydrate();
    const second = renderHook(() => useNewIssueDraft());
    expect(second.result.current.title).toBe("Bug");
    expect(second.result.current.description.text).toBe("steps");
    expect(useNewIssueDraftStore.getState().priority).toBe("high");
    act(() => second.result.current.settle());
    await settle();
    second.unmount();
    const third = renderHook(() => useNewIssueDraft());
    expect(third.result.current.title).toBe("");
  });

  it("new project: title, icon, description and chips come back", async () => {
    const first = renderHook(() => useNewProjectDraft());
    act(() => {
      first.result.current.setTitle("Launch");
      first.result.current.setIcon("🚀");
      first.result.current.setDescription("plan");
      useNewProjectDraftStore.getState().setStatus("in_progress");
    });
    await settle();
    first.unmount();
    useNewProjectDraftStore.getState().reset();

    await coldStart();
    await hydrate();
    const second = renderHook(() => useNewProjectDraft());
    expect(second.result.current.title).toBe("Launch");
    expect(second.result.current.icon).toBe("🚀");
    expect(second.result.current.description).toBe("plan");
    expect(useNewProjectDraftStore.getState().status).toBe("in_progress");
  });

  it("create forms stay locked and write nothing until the saved draft is restored", async () => {
    const seed = renderHook(() => useNewProjectDraft());
    act(() => seed.result.current.setTitle("Saved"));
    await settle();
    seed.unmount();
    await coldStart(); // disk not read yet

    const screen = renderHook(() => useNewProjectDraft());
    expect(screen.result.current.restored).toBe(false);
    await settle();
    expect(screen.result.current.restored).toBe(false);
    await hydrate();
    expect(screen.result.current.restored).toBe(true);
    expect(screen.result.current.title).toBe("Saved");
  });

  const projectOfA = {
    id: "pA",
    title: "A private project",
    icon: "🔒",
  } as unknown as Project;

  it("logout clears the picker stores, so the next account never sees them", async () => {
    // Account A picks a private project and a status, closes the form, logs out.
    useNewIssueDraftStore.getState().setProject(projectOfA);
    useNewIssueDraftStore.getState().setStatus("in_progress");
    useNewProjectDraftStore.getState().setPriority("urgent");
    await clearAllDrafts();

    expect(useNewIssueDraftStore.getState().project).toBeNull();
    expect(useNewIssueDraftStore.getState().status).toBe("todo");
    expect(useNewProjectDraftStore.getState().priority).toBe("none");
  });

  it("a new-issue form with no saved draft opens with clean pickers", async () => {
    // Stale picker value from another session; nothing saved in this scope.
    useNewIssueDraftStore.getState().setProject(projectOfA);
    useNewIssueDraftStore.getState().setPriority("high");
    const screen = renderHook(() => useNewIssueDraft());
    expect(screen.result.current.restored).toBe(true);
    expect(useNewIssueDraftStore.getState().project).toBeNull();
    expect(useNewIssueDraftStore.getState().priority).toBe("none");
    await settle();
    expect(readFormDraft(NEW_ISSUE_KEY)).toBeUndefined();
  });
});
