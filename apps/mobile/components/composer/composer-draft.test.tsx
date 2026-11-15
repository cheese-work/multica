// @vitest-environment node
import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ChatComposer } from "@/components/chat/chat-composer";
import { InlineCommentComposer } from "@/components/issue/inline-comment-composer";
import { clearAllDrafts, draftPersistence } from "@/data/draft-persistence";
import { useComposerDraftsStore } from "@/data/stores/composer-drafts-store";
import { useMentionDraftStore } from "@/data/stores/mention-draft-store";
import { useReplyTargetStore } from "@/data/stores/reply-target-store";

// Native modules are replaced by string host components: the tree is real
// React, only the native leaves are fake.
const mocks = vi.hoisted(() => ({
  disk: new Map<string, string>(),
  uploadFile: vi.fn(),
  pickImage: vi.fn(),
  createComment: vi.fn(),
}));
vi.mock("@react-native-async-storage/async-storage", () => ({
  default: {
    getItem: async (k: string) => mocks.disk.get(k) ?? null,
    setItem: async (k: string, v: string) => void mocks.disk.set(k, v),
    removeItem: async (k: string) => void mocks.disk.delete(k),
    getAllKeys: async () => [...mocks.disk.keys()],
    multiGet: async (ks: string[]) =>
      ks.map((k) => [k, mocks.disk.get(k) ?? null] as [string, string | null]),
    multiRemove: async (ks: string[]) => void ks.forEach((k) => mocks.disk.delete(k)),
  },
}));
vi.mock("react-native", () => ({
  Alert: { alert: vi.fn() },
  Keyboard: { dismiss: vi.fn() },
  Pressable: "Pressable",
  TextInput: "TextInput",
  View: "View",
}));
vi.mock("react-native-reanimated", () => ({
  default: { View: "View" },
  FadeIn: { duration: () => ({}) },
  FadeOut: { duration: () => ({}) },
}));
vi.mock("react-native-keyboard-controller", () => ({
  KeyboardStickyView: "KeyboardStickyView",
}));
vi.mock("react-native-safe-area-context", () => ({
  useSafeAreaInsets: () => ({ bottom: 0 }),
}));
vi.mock("@expo/vector-icons", () => ({ Ionicons: "Ionicons" }));
vi.mock("expo-router", () => ({ router: { push: vi.fn() } }));
vi.mock("expo-haptics", () => ({
  impactAsync: async () => {},
  selectionAsync: async () => {},
  ImpactFeedbackStyle: { Light: 0, Medium: 1 },
}));
vi.mock("expo-image-picker", () => ({
  launchImageLibraryAsync: mocks.pickImage,
  MediaTypeOptions: { Images: "Images" },
}));
vi.mock("expo-document-picker", () => ({ getDocumentAsync: vi.fn() }));
vi.mock("@/data/api", () => ({
  api: { uploadFile: mocks.uploadFile },
  MAX_FILE_SIZE: 1e9,
}));
vi.mock("@/data/mutations/issues", () => ({
  useCreateComment: () => ({ mutateAsync: mocks.createComment }),
}));
vi.mock("@/data/workspace-store", () => ({
  useWorkspaceStore: (select: (s: unknown) => unknown) =>
    select({ currentWorkspaceSlug: "acme" }),
}));
vi.mock("@/lib/use-color-scheme", () => ({
  useColorScheme: () => ({ colorScheme: "light" }),
}));
vi.mock("@/lib/theme", () => ({
  THEME: { light: { mutedForeground: "", primary: "", primaryForeground: "", background: "" } },
}));
vi.mock("@/lib/radius", () => ({ continuousCorners: {} }));
vi.mock("@/lib/i18n", () => ({ useT: () => ({ t: (key: string) => key }) }));
vi.mock("@/components/ui/text", () => ({ Text: "Text" }));
vi.mock("@/components/ui/icon-button", () => ({ IconButton: "IconButton" }));
vi.mock("@/components/issue/composer-attachment-row", () => ({
  ComposerAttachmentRow: "ComposerAttachmentRow",
}));

const SCOPE = "u1:ws1";
const reply = { commentId: "c1", actorName: "Ada", preview: "hello" };
const mention = { type: "agent" as const, id: "a1", name: "Bot" };

let tree: ReactTestRenderer | null = null;

function mount(element: React.ReactElement) {
  act(() => {
    tree = create(element);
  });
  return tree as unknown as ReactTestRenderer;
}
function unmount() {
  act(() => tree?.unmount());
  tree = null;
}
// Native leaves are string host components, so match by type name.
const hosts = (t: ReactTestRenderer, type: string) =>
  t.root.findAll((node) => node.type === type);
const pill = (t: ReactTestRenderer) => hosts(t, "Pressable")[0];
const input = (t: ReactTestRenderer) => hosts(t, "TextInput")[0];
const iconButton = (t: ReactTestRenderer, label: string) =>
  hosts(t, "IconButton").find((b) => b.props.accessibilityLabel === label)!;
const chips = (t: ReactTestRenderer) => hosts(t, "ComposerAttachmentRow")[0];
const flush = () => act(async () => {});

function comment() {
  return <InlineCommentComposer issueId="i1" />;
}
function chat(sessionId: string) {
  return (
    <ChatComposer
      draftKey={sessionId}
      value=""
      onChangeText={() => {}}
      onSend={mocks.createComment}
      onStop={() => {}}
      sending={false}
    />
  );
}
function openAndType(t: ReactTestRenderer, text: string) {
  act(() => pill(t).props.onPress());
  act(() => input(t).props.onChangeText(text));
}
// Presses "upload image" and lets the picker settle; the upload itself may
// stay pending.
async function pickImage(t: ReactTestRenderer) {
  await act(async () => {
    void iconButton(t, "composer.upload_image").props.onPress();
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

describe("composers keep their draft across unmount and remount", () => {
  beforeEach(async () => {
    vi.clearAllMocks();
    // Also cancels writes still pending from the previous test.
    await clearAllDrafts();
    vi.stubGlobal("requestAnimationFrame", (cb: () => void) => setTimeout(cb, 0));
    vi.stubGlobal("cancelAnimationFrame", clearTimeout);
    vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
    useMentionDraftStore.setState({ mentions: [] });
    useReplyTargetStore.setState({ target: null });
    await useComposerDraftsStore.getState().hydrate(SCOPE);
    mocks.pickImage.mockResolvedValue({
      canceled: false,
      assets: [{ uri: "file:///a.png", fileName: "a.png", mimeType: "image/png" }],
    });
    mocks.uploadFile.mockResolvedValue({
      id: "att1",
      url: "mc://file/att1",
      download_url: "/api/attachments/att1/download",
    });
  });
  afterEach(() => {
    if (tree) unmount();
    vi.unstubAllGlobals();
  });

  it("comment composer: typed text", () => {
    openAndType(mount(comment()), "half-typed");
    unmount();

    expect(input(mount(comment())).props.value).toBe("half-typed");
  });

  it("comment composer: the reply target", () => {
    mount(comment());
    act(() => useReplyTargetStore.getState().setTarget(reply));
    unmount();
    expect(useReplyTargetStore.getState().target).toBeNull();

    mount(comment());
    expect(useReplyTargetStore.getState().target?.commentId).toBe("c1");
  });

  it("comment composer: @mention chips", () => {
    mount(comment());
    act(() => useMentionDraftStore.getState().toggle(mention));
    unmount();
    expect(useMentionDraftStore.getState().mentions).toEqual([]);

    const t = mount(comment());
    expect(useMentionDraftStore.getState().mentions).toEqual([mention]);
    expect(chips(t).props.mentions).toEqual([mention]);
  });

  it("comment composer: an uploaded attachment", async () => {
    const t = mount(comment());
    act(() => pill(t).props.onPress());
    await pickImage(t);
    unmount();

    const again = mount(comment());
    expect(chips(again).props.attachments).toMatchObject([
      { filename: "a.png", status: "completed", id: "att1" },
    ]);
  });

  it("comment composer: an upload that finishes after the composer unmounted", async () => {
    let finish!: (v: unknown) => void;
    mocks.uploadFile.mockReturnValue(new Promise((resolve) => (finish = resolve)));
    const t = mount(comment());
    act(() => pill(t).props.onPress());
    await pickImage(t);
    unmount();
    await act(async () => finish({ id: "att1", url: "mc://file/att1", download_url: "/d" }));

    const again = mount(comment());
    expect(chips(again).props.attachments).toMatchObject([
      { status: "completed", id: "att1" },
    ]);
  });

  it("chat composer: mention chips and attachments, per session", async () => {
    const t = mount(chat("s1"));
    act(() => pill(t).props.onPress());
    act(() => useMentionDraftStore.getState().toggle(mention));
    await pickImage(t);
    unmount();

    const again = mount(chat("s1"));
    expect(chips(again).props.mentions).toEqual([mention]);
    expect(chips(again).props.attachments).toHaveLength(1);
    unmount();

    expect(hosts(mount(chat("s2")), "ComposerAttachmentRow")).toHaveLength(0);
  });

  it("a failed send keeps the draft and a successful send clears it", async () => {
    mocks.createComment.mockRejectedValueOnce(new Error("refused"));
    const t = mount(comment());
    openAndType(t, "try me");
    await act(async () => {
      await iconButton(t, "composer.send").props.onPress();
    });
    unmount();
    expect(input(mount(comment())).props.value).toBe("try me");

    mocks.createComment.mockResolvedValueOnce(undefined);
    await act(async () => {
      await iconButton(tree!, "composer.send").props.onPress();
    });
    await flush();
    unmount();
    expect(hosts(mount(comment()), "TextInput")).toHaveLength(0);
  });

  it("comment composer: typing waits until the saved draft has loaded", async () => {
    const store = useComposerDraftsStore.getState();
    store.patch("comment:i1", { text: "hello" });
    await draftPersistence.flush();
    store.reset();

    const loading = useComposerDraftsStore.getState().hydrate(SCOPE);
    const t = mount(comment());
    act(() => pill(t).props.onPress());
    expect(input(t).props.editable).toBe(false);

    await act(async () => loading);
    expect(input(t).props.editable).toBe(true);
    expect(input(t).props.value).toBe("hello");
  });

  it("comment composer: a reply and a mention chosen while the draft loads survive", async () => {
    const store = useComposerDraftsStore.getState();
    store.patch("comment:i1", { text: "hello", reply, mentions: [mention] });
    await draftPersistence.flush();
    store.reset();

    const loading = useComposerDraftsStore.getState().hydrate(SCOPE);
    mount(comment());
    const newReply = { commentId: "c2", actorName: "Bo", preview: "hi" };
    const newMention = { type: "member" as const, id: "m1", name: "Mo" };
    act(() => useReplyTargetStore.getState().setTarget(newReply));
    act(() => useMentionDraftStore.getState().toggle(newMention));
    await act(async () => loading);

    expect(useReplyTargetStore.getState().target?.commentId).toBe("c2");
    expect(useMentionDraftStore.getState().mentions).toEqual([mention, newMention]);
    expect(useComposerDraftsStore.getState().drafts["comment:i1"]?.text).toBe("hello");
  });
});
