import { act, render, screen } from "@testing-library/react";
import { createRef, type ReactNode } from "react";
import { beforeAll, describe, expect, it, vi } from "vitest";
import { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";
import { I18nProvider } from "@multica/core/i18n/react";
import enEditor from "../../locales/en/editor.json";
import type { QueryClient } from "@tanstack/react-query";
import {
  PRESET_ARG_PLUGIN_KEY,
  createPresetArgExtension,
  findPresetArgMatch,
  presetArgItems,
  presetNames,
} from "./preset-arg-suggestion";
import { SlashCommandList, type SlashCommandListRef } from "./slash-command-suggestion";

beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
  // ProseMirror measures the caret through Range rects, which jsdom lacks.
  Range.prototype.getClientRects = vi.fn(() => [] as unknown as DOMRectList);
  Range.prototype.getBoundingClientRect = vi.fn(() => new DOMRect());
});

vi.mock("@multica/core/platform", () => ({ getCurrentWsId: () => "ws-1" }));
const chatState = { selectedAgentId: "agent-1" as string | null };
vi.mock("@multica/core/chat", () => ({ useChatStore: { getState: () => chatState } }));

const NAMES = ["base", "reset", "fast", "deep"];

function editorWith(text: string) {
  const editor = new Editor({
    extensions: [
      StarterKit,
      createPresetArgExtension({ getNames: () => Promise.resolve(NAMES) }),
    ],
    // JSON, not HTML: HTML parsing trims the trailing space these cases hinge on.
    content: { type: "doc", content: [{ type: "paragraph", content: [{ type: "text", text }] }] },
  });
  editor.commands.focus("end");
  return editor;
}

const queryAt = async (text: string) => {
  const editor = editorWith(text);
  const state = PRESET_ARG_PLUGIN_KEY.getState(editor.state);
  // Let the async items settle before teardown, or the popup positions itself on a dead view.
  await act(() => new Promise((resolve) => setTimeout(resolve, 0)));
  editor.destroy();
  return state?.active ? state.query : null;
};

describe("/preset argument match", () => {
  it("opens after `/preset ` and follows the typed word", async () => {
    expect(await queryAt("/preset ")).toBe("");
    expect(await queryAt("/preset fa")).toBe("fa");
  });

  it("stays closed unless /preset is the first word of the message", async () => {
    expect(await queryAt("/preset")).toBeNull();
    expect(await queryAt("/presets x")).toBeNull();
    expect(await queryAt("hello /preset ")).toBeNull();
    expect(await queryAt("/rename ")).toBeNull();
  });

  it("closes once the name is accepted or more words follow", async () => {
    expect(await queryAt("/preset fast ")).toBeNull();
    expect(await queryAt("/preset fast now")).toBeNull();
  });

  it("matches only the word under the cursor", async () => {
    const editor = editorWith("/preset fa");
    const $pos = editor.state.doc.resolve(editor.state.selection.from);
    const match = findPresetArgMatch({ $position: $pos } as never);
    expect(match).toMatchObject({ query: "fa", text: "fa" });
    expect(match!.range.to - match!.range.from).toBe(2);
    await act(() => new Promise((resolve) => setTimeout(resolve, 0)));
    editor.destroy();
  });
});

describe("/preset argument items", () => {
  it("offers base, reset and the agent's presets, filtered as typed", () => {
    expect(presetArgItems(NAMES, "").map((i) => i.label)).toEqual(NAMES);
    expect(presetArgItems(NAMES, "f").map((i) => i.label)).toEqual(["fast"]);
    expect(presetArgItems(NAMES, "RES").map((i) => i.label)).toEqual(["reset"]);
    expect(presetArgItems(NAMES, "zzz")).toEqual([]);
  });
});

describe("/preset argument menu", () => {
  const wrap = (children: ReactNode) => (
    <I18nProvider locale="en" resources={{ en: { editor: enEditor } }}>
      {children}
    </I18nProvider>
  );

  it("lists bare names and Tab accepts the highlighted one", () => {
    const command = vi.fn();
    const ref = createRef<SlashCommandListRef>();
    render(
      wrap(
        <SlashCommandList ref={ref} items={presetArgItems(NAMES, "")} query="" command={command} hideOnEmpty />,
      ),
    );
    expect(screen.getByText("base")).toBeTruthy();
    expect(screen.getByText("reset")).toBeTruthy();
    expect(screen.queryByText("/base")).toBeNull();
    act(() => {
      ref.current!.onKeyDown({ event: new KeyboardEvent("keydown", { key: "ArrowDown" }) });
    });
    act(() => {
      ref.current!.onKeyDown({ event: new KeyboardEvent("keydown", { key: "Tab" }) });
    });
    expect(command).toHaveBeenCalledWith(expect.objectContaining({ label: "reset" }));
  });
});

describe("presetNames", () => {
  const qcWith = (fetchQuery: () => Promise<string[]>) => ({ fetchQuery }) as unknown as QueryClient;

  it("loads the selected agent's completions", async () => {
    const fetchQuery = vi.fn(() => Promise.resolve(NAMES));
    expect(await presetNames(qcWith(fetchQuery), () => true)()).toEqual(NAMES);
    expect(fetchQuery).toHaveBeenCalledWith(
      expect.objectContaining({ queryKey: ["workspaces", "ws-1", "agents", "agent-1", "presets", "completions"] }),
    );
  });

  it("offers nothing when the composer is not a saved DM, or no agent is selected", async () => {
    const fetchQuery = vi.fn(() => Promise.resolve(NAMES));
    expect(await presetNames(qcWith(fetchQuery), () => false)()).toEqual([]);
    chatState.selectedAgentId = null;
    expect(await presetNames(qcWith(fetchQuery), () => true)()).toEqual([]);
    chatState.selectedAgentId = "agent-1";
    expect(fetchQuery).not.toHaveBeenCalled();
  });

  it("offers nothing, not an error, when the fetch fails", async () => {
    expect(await presetNames(qcWith(() => Promise.reject(new Error("403"))), () => true)()).toEqual([]);
  });
});
