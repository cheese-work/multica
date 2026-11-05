// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@react-native-async-storage/async-storage", async () =>
  (await import("./test-support/disk")).asyncStorage(),
);
vi.mock("react-native", () => import("./test-support/react-native-stub"));
vi.mock("expo-router", async () => (await import("./test-support/form-screen-mocks")).expoRouter());
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
vi.mock("@/components/issue/submit-issue-button", () => ({
  SubmitIssueButton: ({ disabled, onPress }: { disabled: boolean; onPress: () => void }) => (
    <button disabled={disabled} onClick={onPress}>
      submit
    </button>
  ),
}));
// Attribute row: the real component runs; only its leaf visuals are stubbed.
vi.mock("@/components/issue/attribute-chip", () => ({
  AttributeChip: ({ label, onPress }: { label: string; onPress: () => void }) => (
    <button onClick={onPress}>{label}</button>
  ),
}));
vi.mock("@expo/vector-icons", () => ({ Ionicons: () => null }));
vi.mock("@/components/ui/actor-avatar", () => ({ ActorAvatar: () => null }));
vi.mock("@/components/ui/priority-icon", () => ({ PriorityIcon: () => null }));
vi.mock("@/components/ui/project-icon", () => ({ ProjectIcon: () => null }));
vi.mock("@/components/ui/status-icon", () => ({ StatusIcon: () => null }));
vi.mock("@/data/use-actor-name", () => ({ useActorLookup: () => ({ getName: () => "Ann" }) }));
vi.mock("@/lib/use-issue-statuses", () => ({
  useIssueStatuses: () => ({
    categoryOf: () => "todo",
    colorOf: () => "",
    iconOf: () => "",
    labelOf: (s: string) => `status:${s}`,
  }),
}));
vi.mock("@/lib/issue-status", () => ({ PRIORITY_LABEL: { high: "priority.high" } }));
vi.mock("@/data/workspace-store", () => ({
  useWorkspaceStore: (sel: (s: { currentWorkspaceSlug: string }) => unknown) =>
    sel({ currentWorkspaceSlug: "acme" }),
}));
const create = vi.hoisted(() => ({ mutateAsync: vi.fn(), isPending: false }));
vi.mock("@/data/mutations/issues", () => ({ useCreateIssue: () => create }));

import { disk } from "./test-support/disk";
import { router } from "./test-support/form-screen-mocks";
import { useFormDraftsStore } from "./form-drafts-store";
import { useNewIssueDraftStore } from "./new-issue-draft-store";
import NewIssueModal from "@/app/(app)/[workspace]/new-issue";

const SCOPE = "u:ws1";
const settle = () => act(() => vi.advanceTimersByTimeAsync(1000));
const hydrate = () => act(() => useFormDraftsStore.getState().hydrate(SCOPE));
const coldStart = () => {
  useFormDraftsStore.getState().reset();
  useNewIssueDraftStore.getState().reset();
};
const titleInput = () => screen.getByPlaceholderText("new.title_placeholder") as HTMLInputElement;
const type = (el: HTMLElement, value: string) => fireEvent.change(el, { target: { value } });
const button = (name: string) => screen.getByText(name).closest("button") as HTMLButtonElement;

describe("new issue screen", () => {
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

  it("shows title, description and chips again after the modal is closed and reopened, also after a cold start", async () => {
    render(<NewIssueModal />);
    type(titleInput(), "Bug");
    type(screen.getByPlaceholderText("new.description_placeholder"), "steps");
    act(() => useNewIssueDraftStore.getState().setPriority("high"));
    await settle();
    cleanup();
    coldStart();
    await hydrate();

    render(<NewIssueModal />);
    expect(titleInput().value).toBe("Bug");
    expect((screen.getByPlaceholderText("new.description_placeholder") as HTMLTextAreaElement).value).toBe("steps");
    expect(screen.getByText("priority.high")).toBeTruthy();
  });

  it("locks input, submit and every chip until the saved draft loads, and keeps the chips picked after", async () => {
    render(<NewIssueModal />);
    type(titleInput(), "Bug");
    act(() => useNewIssueDraftStore.getState().setPriority("high"));
    await settle();
    cleanup();
    coldStart();

    render(<NewIssueModal />);
    expect(titleInput().readOnly).toBe(true);
    expect(button("submit").disabled).toBe(true);
    fireEvent.click(button("status:todo"));
    expect(router.push).not.toHaveBeenCalled();

    await hydrate();
    expect(titleInput().value).toBe("Bug");
    expect(useNewIssueDraftStore.getState().priority).toBe("high");
    fireEvent.click(button("status:todo"));
    expect(router.push).toHaveBeenCalledTimes(1);
  });

  it("clears the draft after a successful create and keeps it after a failed one", async () => {
    render(<NewIssueModal />);
    type(titleInput(), "Bug");
    await settle();

    create.mutateAsync.mockRejectedValueOnce(new Error("boom"));
    await act(async () => fireEvent.click(button("submit")));
    expect(router.back).not.toHaveBeenCalled();
    await settle();
    cleanup();
    coldStart();
    await hydrate();
    render(<NewIssueModal />);
    expect(titleInput().value).toBe("Bug");

    create.mutateAsync.mockResolvedValueOnce({});
    await act(async () => fireEvent.click(button("submit")));
    expect(router.back).toHaveBeenCalledTimes(1);
    await settle();
    cleanup();
    coldStart();
    await hydrate();
    render(<NewIssueModal />);
    expect(titleInput().value).toBe("");
  });
});
