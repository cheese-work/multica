// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@react-native-async-storage/async-storage", async () =>
  (await import("./test-support/disk")).asyncStorage(),
);
vi.mock("react-native", () => import("./test-support/react-native-stub"));
vi.mock("expo-router", async () => (await import("./test-support/form-screen-mocks")).expoRouter());
vi.mock("@/components/ui/text", async () => (await import("./test-support/form-screen-mocks")).text());
vi.mock("@/components/ui/autosize-textarea", async () =>
  (await import("./test-support/form-screen-mocks")).autosizeTextArea(),
);
vi.mock("@/components/ui/input-tokens", () => ({
  MIN_BODY_INPUT_HEIGHT_PX: 80,
  MOBILE_PLACEHOLDER_COLOR: "#999",
}));
vi.mock("@/components/ui/project-status-icon", () => ({ ProjectStatusIcon: () => null }));
vi.mock("@/components/ui/project-priority-icon", () => ({ ProjectPriorityIcon: () => null }));
vi.mock("@/lib/project-status", () => ({
  projectStatusLabel: (s: string) => `status:${s}`,
  projectPriorityLabel: (p: string) => `priority:${p}`,
}));
vi.mock("@/lib/i18n", async () => (await import("./test-support/form-screen-mocks")).i18n());
vi.mock("@/data/workspace-store", () => ({
  useWorkspaceStore: (sel: (s: { currentWorkspaceSlug: string }) => unknown) =>
    sel({ currentWorkspaceSlug: "acme" }),
}));
const create = vi.hoisted(() => ({ mutate: vi.fn(), isPending: false }));
vi.mock("@/data/mutations/projects", () => ({ useCreateProject: () => create }));

import { Alert } from "react-native";
import { disk } from "./test-support/disk";
import { router } from "./test-support/form-screen-mocks";
import { useFormDraftsStore } from "./form-drafts-store";
import { useNewProjectDraftStore } from "./new-project-draft-store";
import NewProject from "@/app/(app)/[workspace]/project/new";

const SCOPE = "u:ws1";
const settle = () => act(() => vi.advanceTimersByTimeAsync(1000));
const hydrate = () => act(() => useFormDraftsStore.getState().hydrate(SCOPE));
const coldStart = () => {
  useFormDraftsStore.getState().reset();
  useNewProjectDraftStore.getState().reset();
};
const titleInput = () => screen.getByPlaceholderText("form.title_placeholder") as HTMLInputElement;
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } });
const press = (name: string) => fireEvent.click(screen.getByText(name));

/** Types a draft, closes the screen with Cancel, then cold-starts the app. */
async function leaveDraftOnDisk() {
  render(<NewProject />);
  type(titleInput(), "Launch");
  await settle();
  cleanup();
  coldStart();
}

describe("new project screen", () => {
  beforeEach(async () => {
    disk.clear();
    vi.useFakeTimers();
    vi.clearAllMocks();
    coldStart();
    await hydrate();
  });
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it("shows the typed title again after the screen is closed and reopened, also after a cold start", async () => {
    render(<NewProject />);
    type(titleInput(), "Launch");
    await settle();
    cleanup();
    expect(useFormDraftsStore.getState().restored["form:new-project"]).toBe(false);

    render(<NewProject />);
    expect(titleInput().value).toBe("Launch");
    cleanup();

    coldStart();
    await hydrate();
    render(<NewProject />);
    expect(titleInput().value).toBe("Launch");
  });

  it("stays locked until the saved draft loads: no input, no pickers", async () => {
    await leaveDraftOnDisk();
    render(<NewProject />);

    expect(titleInput().readOnly).toBe(true);
    expect((screen.getByText("status:planned").closest("button") as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByText("common:actions.create").closest("button") as HTMLButtonElement).disabled).toBe(true);

    await hydrate();
    expect(titleInput().readOnly).toBe(false);
    expect(titleInput().value).toBe("Launch");
    expect((screen.getByText("status:planned").closest("button") as HTMLButtonElement).disabled).toBe(false);
  });

  it("keeps the saved draft when Cancel is pressed before it has loaded", async () => {
    await leaveDraftOnDisk();
    render(<NewProject />);
    press("common:actions.cancel");

    expect(router.back).toHaveBeenCalledTimes(1);
    expect(Alert.alert).not.toHaveBeenCalled();
    await settle();
    cleanup();

    await hydrate();
    render(<NewProject />);
    expect(titleInput().value).toBe("Launch");
  });

  it("asks before discarding a dirty form, and discards only after confirmation", async () => {
    render(<NewProject />);
    type(titleInput(), "Launch");
    press("common:actions.cancel");

    expect(router.back).not.toHaveBeenCalled();
    const buttons = (Alert.alert as ReturnType<typeof vi.fn>).mock.calls[0][2] as {
      style?: string;
      onPress?: () => void;
    }[];
    act(() => buttons.find((b) => b.style === "destructive")!.onPress!());
    expect(router.back).toHaveBeenCalledTimes(1);
    await settle();
    cleanup();

    coldStart();
    await hydrate();
    render(<NewProject />);
    expect(titleInput().value).toBe("");
  });

  it("clears the draft after a successful create and keeps it after a failed one", async () => {
    render(<NewProject />);
    type(titleInput(), "Launch");
    await settle();

    press("common:actions.create");
    const [, opts] = create.mutate.mock.calls[0] as [unknown, { onSuccess: (p: { id: string }) => void; onError: (e: Error) => void }];
    act(() => opts.onError(new Error("boom")));
    expect(Alert.alert).toHaveBeenCalled();
    await settle();
    cleanup();
    coldStart();
    await hydrate();
    render(<NewProject />);
    expect(titleInput().value).toBe("Launch");

    press("common:actions.create");
    const [, opts2] = create.mutate.mock.calls[1] as [unknown, { onSuccess: (p: { id: string }) => void }];
    act(() => opts2.onSuccess({ id: "p1" }));
    await settle();
    cleanup();
    coldStart();
    await hydrate();
    render(<NewProject />);
    expect(titleInput().value).toBe("");
  });
});
