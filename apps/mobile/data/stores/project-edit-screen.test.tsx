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
vi.mock("@/lib/i18n", async () => (await import("./test-support/form-screen-mocks")).i18n());
vi.mock("@/components/ui/input-tokens", () => ({
  MIN_BODY_INPUT_HEIGHT_PX: 80,
  MOBILE_PLACEHOLDER_COLOR: "#999",
}));
vi.mock("@/lib/radius", () => ({ continuousCorners: {} }));
vi.mock("@/components/issue/mention-suggestion-bar", () => ({ MentionSuggestionBar: () => null }));
vi.mock("@/data/workspace-store", () => ({
  useWorkspaceStore: (sel: (s: { currentWorkspaceId: string }) => unknown) =>
    sel({ currentWorkspaceId: "ws1" }),
}));
const server = vi.hoisted(() => ({ record: { id: "rec1", title: "Server project", description: "", icon: "📦" } }));
vi.mock("@tanstack/react-query", () => ({ useQuery: () => ({ data: server.record }) }));
vi.mock("@/data/queries/projects", () => ({ projectDetailOptions: () => ({}) }));
const update = vi.hoisted(() => ({ mutate: vi.fn(), isPending: false }));
vi.mock("@/data/mutations/projects", () => ({ useUpdateProject: () => update }));

import { Alert } from "react-native";
import { disk } from "./test-support/disk";
import { router } from "./test-support/form-screen-mocks";
import { useFormDraftsStore } from "./form-drafts-store";
import EditProject from "@/app/(app)/[workspace]/project/[id]/edit";

const SCOPE = "u:ws1";
const settle = () => act(() => vi.advanceTimersByTimeAsync(1000));
const hydrate = () => act(() => useFormDraftsStore.getState().hydrate(SCOPE));
const coldStart = () => useFormDraftsStore.getState().reset();
const titleInput = () => screen.getByPlaceholderText("form.title_placeholder") as HTMLInputElement;
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } });
const press = (name: string) => fireEvent.click(screen.getByText(name));
const saveLabel = "common:actions.save";

describe("project-edit screen", () => {
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

  it("shows the unsent edit again after the screen is closed and reopened, also after a cold start", async () => {
    render(<EditProject />);
    expect(titleInput().value).toBe(server.record.title);
    type(titleInput(), "My edit");
    await settle();
    cleanup();

    render(<EditProject />);
    expect(titleInput().value).toBe("My edit");
    cleanup();

    coldStart();
    await hydrate();
    render(<EditProject />);
    expect(titleInput().value).toBe("My edit");
  });

  it("shows the loading state, not the server values, until the saved draft loads", async () => {
    render(<EditProject />);
    type(titleInput(), "My edit");
    await settle();
    cleanup();
    coldStart();

    render(<EditProject />);
    expect(screen.queryByPlaceholderText("form.title_placeholder")).toBeNull();
    expect((screen.getByText(saveLabel).closest("button") as HTMLButtonElement).disabled).toBe(true);
    await hydrate();
    expect(titleInput().value).toBe("My edit");
  });

  it("keeps the draft after a failed save and clears it after a successful one", async () => {
    render(<EditProject />);
    type(titleInput(), "My edit");
    await settle();

    press(saveLabel);
    const [, failed] = update.mutate.mock.calls[0] as [unknown, { onError: (e: Error) => void }];
    act(() => failed.onError(new Error("boom")));
    expect(Alert.alert).toHaveBeenCalled();
    expect(router.back).not.toHaveBeenCalled();
    await settle();
    cleanup();
    coldStart();
    await hydrate();
    render(<EditProject />);
    expect(titleInput().value).toBe("My edit");

    press(saveLabel);
    const [, ok] = update.mutate.mock.calls[1] as [unknown, { onSuccess: () => void }];
    act(() => ok.onSuccess());
    expect(router.back).toHaveBeenCalledTimes(1);
    await settle();
    cleanup();
    coldStart();
    await hydrate();
    render(<EditProject />);
    expect(titleInput().value).toBe(server.record.title);
  });
});
