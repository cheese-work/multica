import { act, render } from "@testing-library/react";
import { createRef, type ReactNode } from "react";
import { beforeAll, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import { workspaceKeys } from "@multica/core/workspace/queries";
import type { Agent, MemberWithUser } from "@multica/core/types";
import type { QueryClient } from "@tanstack/react-query";
import { Editor } from "@tiptap/core";
import StarterKit from "@tiptap/starter-kit";
import { Markdown } from "@tiptap/markdown";
import { SlashCommandExtension } from "./slash-command-extension";
import enEditor from "../../locales/en/editor.json";
import frEditor from "../../locales/fr/editor.json";
import jaEditor from "../../locales/ja/editor.json";
import koEditor from "../../locales/ko/editor.json";
import zhEditor from "../../locales/zh-Hans/editor.json";

const TEST_RESOURCES = {
  en: { editor: enEditor },
};

function I18nWrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

vi.mock("@multica/core/platform", () => ({
  getCurrentWsId: () => "ws-1",
}));

const authState = { user: { id: "u1" } as { id: string } | null };
vi.mock("@multica/core/auth", () => ({
  useAuthStore: { getState: () => authState },
}));

const chatState = { selectedAgentId: "agent-1" as string | null };
vi.mock("@multica/core/chat", () => ({
  useChatStore: { getState: () => chatState },
}));

import {
  SlashCommandList,
  type SlashCommandListRef,
  createSlashCommandSuggestion,
  type SlashCommandItem,
  buildBuiltinCommandItems,
  BUILTIN_COMMANDS,
  createBuiltinCommandSuggestion,
  QUICK_ACTION_ITEM_PREFIX,
} from "./slash-command-suggestion";

function agent(overrides: Partial<Agent>): Agent {
  return {
    id: "agent-1",
    workspace_id: "ws-1",
    runtime_id: "runtime-1",
    name: "Agent",
    description: "",
    instructions: "",
    avatar_url: null,
    runtime_mode: "local",
    runtime_config: {},
    custom_args: [],
    visibility: "workspace",
    permission_mode: "public_to",
    invocation_targets: [{ target_type: "workspace", target_id: null }],
    status: "idle",
    max_concurrent_tasks: 1,
    model: "",
    owner_id: null,
    skills: [],
    created_at: "",
    updated_at: "",
    archived_at: null,
    archived_by: null,
    ...overrides,
  };
}

function fakeQc(data: {
  members?: Array<Pick<MemberWithUser, "user_id" | "name" | "role">>;
  agents?: Agent[];
}): QueryClient {
  const map = new Map<string, unknown>();
  map.set(JSON.stringify(workspaceKeys.members("ws-1")), data.members ?? []);
  map.set(JSON.stringify(workspaceKeys.agents("ws-1")), data.agents ?? []);
  return {
    getQueryData: (key: readonly unknown[]) => map.get(JSON.stringify(key)),
  } as unknown as QueryClient;
}

function items(qc: QueryClient, query = "", before = "", canFork = false): SlashCommandItem[] {
  const config = createSlashCommandSuggestion(qc, () => true, () => canFork);
  const editor = new Editor({
    extensions: [StarterKit],
    content: {
      type: "doc",
      content: `${before}/${query}`.split("\n").map((text) => ({
        type: "paragraph",
        content: text ? [{ type: "text", text }] : [],
      })),
    },
  });
  editor.commands.setTextSelection(editor.state.doc.content.size - 1);
  try {
    return config.items!({
      query,
      editor,
      signal: new AbortController().signal,
    }) as SlashCommandItem[];
  } finally {
    editor.destroy();
  }
}

describe("slash command suggestion items", () => {
  it("returns all active agent skills when query is empty", () => {
    chatState.selectedAgentId = "agent-1";
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [
        agent({
          id: "agent-1",
          skills: [
            { id: "s1", name: "deploy", description: "Ship changes" },
            { id: "s2", name: "review", description: "Review code" },
          ],
        }),
      ],
    });

    expect(items(qc).map((i) => i.label)).toEqual(["compact", "rename", "deploy", "review"]);
  });

  it("filters skills by name case-insensitively", () => {
    chatState.selectedAgentId = "agent-1";
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [
        agent({
          id: "agent-1",
          skills: [
            { id: "s1", name: "Deploy", description: "" },
            { id: "s2", name: "Review", description: "" },
          ],
        }),
      ],
    });

    expect(items(qc, "dep").map((i) => i.id)).toEqual(["s1"]);
  });

  it("filters skills by description", () => {
    chatState.selectedAgentId = "agent-1";
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [
        agent({
          id: "agent-1",
          skills: [
            { id: "s1", name: "deploy", description: "Ship changes" },
            { id: "s2", name: "review", description: "Read a pull request" },
          ],
        }),
      ],
    });

    expect(items(qc, "pull").map((i) => i.id)).toEqual(["s2"]);
  });

  it("ranks name prefix matches above description-only matches", () => {
    chatState.selectedAgentId = "agent-1";
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [
        agent({
          id: "agent-1",
          skills: [
            { id: "s1", name: "grilling", description: "Grill the user about a plan" },
            { id: "s2", name: "prototype", description: "Build a throwaway prototype" },
            { id: "s3", name: "wayfinder", description: "Plan a huge chunk of work" },
          ],
        }),
      ],
    });

    expect(items(qc, "wa").map((i) => i.id)).toEqual(["s3", "s2"]);
  });

  it("ranks an exact name match ahead of a longer prefix match", () => {
    chatState.selectedAgentId = "agent-1";
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [
        agent({
          id: "agent-1",
          skills: [
            { id: "s1", name: "reviewer", description: "" },
            { id: "s2", name: "review", description: "" },
          ],
        }),
      ],
    });

    expect(items(qc, "review").map((i) => i.id)).toEqual(["s2", "s1"]);
  });

  it("ranks a name prefix above a mid-name match", () => {
    chatState.selectedAgentId = "agent-1";
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [
        agent({
          id: "agent-1",
          skills: [
            { id: "s1", name: "pr-review", description: "" },
            { id: "s2", name: "review", description: "" },
          ],
        }),
      ],
    });

    expect(items(qc, "rev").map((i) => i.id)).toEqual(["s2", "s1"]);
  });

  it("keeps the configured skill order within a match tier", () => {
    chatState.selectedAgentId = "agent-1";
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [
        agent({
          id: "agent-1",
          skills: [
            { id: "s1", name: "deploy-web", description: "" },
            { id: "s2", name: "deploy-api", description: "" },
          ],
        }),
      ],
    });

    expect(items(qc, "deploy").map((i) => i.id)).toEqual(["s1", "s2"]);
  });

  it("keeps a name match inside the 20-item cap when description hits fill it", () => {
    chatState.selectedAgentId = "agent-1";
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [
        agent({
          id: "agent-1",
          skills: [
            ...Array.from({ length: 25 }, (_, i) => ({
              id: `d${i}`,
              name: `skill-${i}`,
              description: "Build a throwaway prototype",
            })),
            { id: "s-named", name: "wayfinder", description: "" },
          ],
        }),
      ],
    });

    const result = items(qc, "wa");
    expect(result).toHaveLength(20);
    expect(result[0]?.id).toBe("s-named");
  });

  it("tolerates skills with missing descriptions from cached API data", () => {
    chatState.selectedAgentId = "agent-1";
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [
        agent({
          id: "agent-1",
          skills: [
            { id: "s1", name: "deploy" } as Agent["skills"][number],
          ],
        }),
      ],
    });

    expect(() => items(qc, "dep")).not.toThrow();
    expect(items(qc, "dep")).toEqual([
      { id: "s1", label: "deploy", description: "" },
    ]);
  });

  it("offers compact when the active agent has no skills", () => {
    chatState.selectedAgentId = "agent-1";
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [agent({ id: "agent-1", skills: [] })],
    });

    expect(items(qc).map((item) => item.label)).toEqual(["compact", "rename"]);
  });

  it("caps results at 20", () => {
    chatState.selectedAgentId = "agent-1";
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [
        agent({
          id: "agent-1",
          skills: Array.from({ length: 25 }, (_, i) => ({
            id: `s${i}`,
            name: `skill-${i}`,
            description: "",
          })),
        }),
      ],
    });

    expect(items(qc)).toHaveLength(20);
  });

  it("falls back to the first available agent when selectedAgentId is stale", () => {
    chatState.selectedAgentId = "missing";
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [
        agent({
          id: "agent-1",
          skills: [{ id: "s1", name: "deploy", description: "" }],
        }),
      ],
    });

    expect(items(qc).map((i) => i.label)).toEqual(["compact", "rename", "deploy"]);
  });

  it("offers only compact when no agents exist", () => {
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [],
    });

    expect(items(qc).map((item) => item.label)).toEqual(["compact", "rename"]);
  });

  it("excludes skills from private agents the user cannot access", () => {
    chatState.selectedAgentId = "private-agent";
    const qc = fakeQc({
      members: [
        { user_id: "u1", name: "Alice", role: "member" },
        { user_id: "u2", name: "Bob", role: "member" },
      ],
      agents: [
        agent({
          id: "private-agent",
          visibility: "private",
          permission_mode: "private",
          invocation_targets: [],
          owner_id: "u2",
          skills: [{ id: "private-skill", name: "secret", description: "" }],
        }),
      ],
    });

    expect(items(qc).map((item) => item.label)).toEqual(["compact", "rename"]);
  });
});

describe("DM rename command", () => {
  const rename: SlashCommandItem = { id: "rename", label: "rename", descriptionKey: "rename" };

  it("offers rename next to compact with a localized description and client styling", () => {
    expect(items(fakeQc({})).slice(0, 2).map((item) => item.id)).toEqual(["compact", "rename"]);
    const { getByRole } = render(
      <I18nWrapper><SlashCommandList items={[rename]} query="" command={vi.fn()} /></I18nWrapper>,
    );
    const row = getByRole("button", { name: /\/rename \[title\]/ });
    expect(row).toHaveTextContent(enEditor.slash_command.client_command);
    expect(row).toHaveTextContent(enEditor.slash_command.commands.rename);
  });

  it.each(["re", "RENAME"])("matches the label prefix %j", (query) => {
    expect(items(fakeQc({}), query)).toEqual([rename]);
  });

  it.each(["Hello ", "Hello\n"])("omits rename after message content %j", (before) => {
    expect(items(fakeQc({}), "re", before)).toEqual([]);
  });

  it("inserts plain rename text without a skill node or the display hint", () => {
    const element = document.body.appendChild(document.createElement("div"));
    const editor = new Editor({ element, extensions: [StarterKit, Markdown, SlashCommandExtension], content: "<p>/re</p>" });
    editor.commands.setTextSelection(4);
    window.getSelection()?.selectAllChildren(editor.view.dom);
    try {
      createSlashCommandSuggestion(fakeQc({})).command!({ editor, range: { from: 1, to: 4 }, props: rename });
      expect(editor.getMarkdown()).toBe("/rename ");
      expect(editor.getJSON().content?.[0]?.content?.[0]?.type).toBe("text");
    } finally {
      editor.destroy();
      element.remove();
    }
  });

  it.each([enEditor, frEditor, jaEditor, koEditor, zhEditor])("localizes rename in every locale", (locale) => {
    expect(locale.slash_command.commands).toHaveProperty("rename", expect.any(String));
    expect(locale.slash_command).toHaveProperty("title_hint", expect.any(String));
  });
});

describe("DM fork command", () => {
  const fork: SlashCommandItem = { id: "fork", label: "fork", descriptionKey: "fork" };

  it("offers fork after compact and rename only when the fork capability is on", () => {
    expect(items(fakeQc({}), "", "", true).map((item) => item.id)).toEqual(["compact", "rename", "fork"]);
    expect(items(fakeQc({}), "", "", false).map((item) => item.id)).toEqual(["compact", "rename"]);
    expect(items(fakeQc({}), "fork", "", false)).toEqual([]);
  });

  it.each(["f", "fo", "FORK"])("matches the label prefix %j", (query) => {
    expect(items(fakeQc({}), query, "", true)).toEqual([fork]);
  });

  it.each(["Hello ", "Hello\n"])("omits fork after message content %j", (before) => {
    expect(items(fakeQc({}), "fo", before, true)).toEqual([]);
  });

  it("renders the Multica badge, [message] hint and localized description", () => {
    const { getByRole, getByText } = render(
      <I18nWrapper><SlashCommandList items={[fork]} query="" command={vi.fn()} /></I18nWrapper>,
    );
    const row = getByRole("button", { name: /\/fork \[message\]/ });
    expect(row).toHaveTextContent(enEditor.slash_command.commands.fork);
    expect(getByText("Multica")).toBeInTheDocument();
    expect(getByText("Copy this DM into a new one; optional message is sent there")).toBeInTheDocument();
  });

  it("inserts plain fork text without a skill node or the display hint", () => {
    const element = document.body.appendChild(document.createElement("div"));
    const editor = new Editor({ element, extensions: [StarterKit, Markdown, SlashCommandExtension], content: "<p>/fo</p>" });
    editor.commands.setTextSelection(4);
    window.getSelection()?.selectAllChildren(editor.view.dom);
    try {
      createSlashCommandSuggestion(fakeQc({})).command!({ editor, range: { from: 1, to: 4 }, props: fork });
      expect(editor.getMarkdown()).toBe("/fork ");
      expect(editor.getJSON().content?.[0]?.content?.[0]?.type).toBe("text");
    } finally {
      editor.destroy();
      element.remove();
    }
  });

  it.each([enEditor, frEditor, jaEditor, koEditor, zhEditor])("localizes fork in every locale", (locale) => {
    expect(locale.slash_command.commands).toHaveProperty("fork", expect.any(String));
    expect(locale.slash_command).toHaveProperty("message_hint", expect.any(String));
  });
});

describe("DM compact command", () => {
  const compact = {
    id: "compact",
    label: "compact",
    descriptionKey: "compact",
  } as SlashCommandItem;

  it.each(["", "co", "COMPACT"])("matches the label prefix %j ahead of skills", (query) => {
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [agent({ skills: [{ id: "s1", name: "compact-skill", description: "" }] })],
    });
    expect(items(qc, query).map((item) => item.id)).toEqual(query ? ["compact", "s1"] : ["compact", "rename", "s1"]);
  });

  it.each(["omp", "focus", "summarize", "compactx"])("does not match %j", (query) => {
    expect(items(fakeQc({}), query)).toEqual([]);
  });

  it.each(["Hello ", "Hello\n", "\nHello\n"])("omits compact after message content %j without hiding skills", (before) => {
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [agent({ skills: [{ id: "s1", name: "compile", description: "" }] })],
    });
    expect(items(qc, "co", before).map((item) => item.id)).toEqual(["s1"]);
  });

  it.each([" ", "\n", "  \n  "])("allows leading whitespace %j", (before) => {
    expect(items(fakeQc({}), "co", before).map((item) => item.id)).toEqual(["compact"]);
  });

  it.each([
    ["<p><br>/co</p>", true],
    ["<blockquote><p>/co</p></blockquote>", false],
    ["<ul><li><p>/co</p></li></ul>", false],
    ["<pre><code>/co</code></pre>", false],
    ['<p><span data-type="slash-command" data-id="s1" data-label="deploy"></span> /co</p>', false],
  ])("matches the server's first-word position in %j", (content, offered) => {
    const editor = new Editor({ extensions: [StarterKit, SlashCommandExtension], content });
    editor.state.doc.descendants((node, position) => {
      if (node.isText) editor.commands.setTextSelection(position + node.nodeSize);
    });
    try {
      const result = createSlashCommandSuggestion(fakeQc({})).items!({
        editor, query: "co", signal: new AbortController().signal,
      }) as SlashCommandItem[];
      expect(result.some((item) => item.id === "compact")).toBe(offered);
    } finally {
      editor.destroy();
    }
  });

  it("keeps compact first within the 20-item cap", () => {
    const qc = fakeQc({
      members: [{ user_id: "u1", name: "Alice", role: "member" }],
      agents: [agent({ skills: Array.from({ length: 25 }, (_, index) => ({
        id: `s${index}`, name: `skill-${index}`, description: "",
      })) })],
    });
    const result = items(qc);
    expect(result).toHaveLength(20);
    expect(result[0]?.id).toBe("compact");
  });

  it.each(["/co", "/co remaining focus"])("inserts plain compact text into %j, never the display hint or a skill node", (text) => {
    const element = document.body.appendChild(document.createElement("div"));
    const editor = new Editor({
      element,
      extensions: [StarterKit, Markdown, SlashCommandExtension],
      content: `<p>${text}</p>`,
    });
    editor.commands.setTextSelection(4);
    window.getSelection()?.selectAllChildren(editor.view.dom);
    try {
      createSlashCommandSuggestion(fakeQc({})).command!({
        editor, range: { from: 1, to: 4 }, props: compact,
      });
      const expected = text.replace(/^\/co ?/, "/compact ");
      expect(editor.getJSON()).toEqual({
        type: "doc",
        content: [{ type: "paragraph", content: [{ type: "text", text: expected }] }],
      });
      expect(editor.getMarkdown()).toBe(expected);
    } finally {
      editor.destroy();
      element.remove();
    }
  });

  it("keeps skill selections as rich slashCommand nodes", () => {
    const element = document.body.appendChild(document.createElement("div"));
    const editor = new Editor({
      element,
      extensions: [StarterKit, SlashCommandExtension],
      content: "<p>/de</p>",
    });
    editor.commands.setTextSelection(4);
    window.getSelection()?.selectAllChildren(editor.view.dom);
    try {
      createSlashCommandSuggestion(fakeQc({})).command!({
        editor, range: { from: 1, to: 4 }, props: { id: "s1", label: "deploy" },
      });
      expect(editor.getJSON().content?.[0]?.content?.[0]).toMatchObject({
        type: "slashCommand", attrs: { id: "s1", label: "deploy" },
      });
    } finally {
      editor.destroy();
      element.remove();
    }
  });

  it("renders the display-only focus hint, localized description, and Multica badge", () => {
    const { getByRole, getByText } = render(
      <I18nWrapper>
        <SlashCommandList items={[compact]} query="" command={vi.fn()} />
      </I18nWrapper>,
    );
    expect(getByRole("button", { name: /\/compact \[focus\]/ })).toBeInTheDocument();
    expect(getByText("Multica")).toBeInTheDocument();
    expect(getByText("Summarize this DM and continue in a new one")).toBeInTheDocument();
  });

  it.each([enEditor, frEditor, jaEditor, koEditor, zhEditor])("has a compact description in every locale", (locale) => {
    expect(locale.slash_command.commands).toHaveProperty("compact", expect.any(String));
  });
});

describe("SlashCommandList keyboard handling", () => {
  it("lets Enter and arrow keys fall through when there are no selectable items", () => {
    const ref = createRef<SlashCommandListRef>();

    render(
      <I18nWrapper>
        <SlashCommandList ref={ref} items={[]} query="" command={vi.fn()} />
      </I18nWrapper>,
    );

    expect(
      ref.current?.onKeyDown({
        event: new KeyboardEvent("keydown", { key: "Enter" }),
      }),
    ).toBe(false);
    expect(
      ref.current?.onKeyDown({
        event: new KeyboardEvent("keydown", { key: "Enter", metaKey: true }),
      }),
    ).toBe(false);
    expect(
      ref.current?.onKeyDown({
        event: new KeyboardEvent("keydown", { key: "ArrowUp" }),
      }),
    ).toBe(false);
    expect(
      ref.current?.onKeyDown({
        event: new KeyboardEvent("keydown", { key: "ArrowDown" }),
      }),
    ).toBe(false);
  });

  it("handles Enter and arrow keys when selectable items exist", () => {
    const ref = createRef<SlashCommandListRef>();
    const command = vi.fn();
    const selectableItems: SlashCommandItem[] = [
      { id: "s1", label: "deploy", description: "Ship changes" },
      { id: "s2", label: "review", description: "Review code" },
    ];

    render(
      <I18nWrapper>
        <SlashCommandList
          ref={ref}
          items={selectableItems}
          query=""
          command={command}
        />
      </I18nWrapper>,
    );

    expect(
      ref.current?.onKeyDown({
        event: new KeyboardEvent("keydown", { key: "ArrowUp" }),
      }),
    ).toBe(true);
    expect(
      ref.current?.onKeyDown({
        event: new KeyboardEvent("keydown", { key: "ArrowDown" }),
      }),
    ).toBe(true);
    expect(
      ref.current?.onKeyDown({
        event: new KeyboardEvent("keydown", { key: "Enter" }),
      }),
    ).toBe(true);
    expect(command).toHaveBeenCalledWith(selectableItems[0]);
  });

  // MUL-5495: same Ctrl aliases the command bar (cmdk) accepts, so the slash
  // picker navigates like every other list in the product.
  it("navigates with Ctrl+N/J and Ctrl+P/K, and leaves the bare letters alone", () => {
    const ref = createRef<SlashCommandListRef>();
    const command = vi.fn();
    const selectableItems: SlashCommandItem[] = [
      { id: "s1", label: "deploy", description: "Ship changes" },
      { id: "s2", label: "review", description: "Review code" },
      { id: "s3", label: "note", description: "Leave a note" },
    ];

    render(
      <I18nWrapper>
        <SlashCommandList
          ref={ref}
          items={selectableItems}
          query=""
          command={command}
        />
      </I18nWrapper>,
    );

    const highlightedLabel = () => {
      const buttons = Array.from(document.querySelectorAll<HTMLButtonElement>("button"));
      return buttons.find((b) => b.classList.contains("bg-accent"))?.textContent ?? "";
    };
    let handled: boolean | undefined;
    const press = (init: KeyboardEventInit) =>
      act(() => {
        handled = ref.current?.onKeyDown({ event: new KeyboardEvent("keydown", init) });
      });

    press({ key: "n", ctrlKey: true });
    expect(handled).toBe(true);
    expect(highlightedLabel()).toContain("/review");

    press({ key: "j", ctrlKey: true });
    expect(highlightedLabel()).toContain("/note");

    press({ key: "p", ctrlKey: true });
    expect(highlightedLabel()).toContain("/review");

    press({ key: "k", ctrlKey: true });
    expect(highlightedLabel()).toContain("/deploy");

    // Bare letters stay query characters — "/note" must remain typeable.
    press({ key: "n" });
    expect(handled).toBe(false);
    expect(highlightedLabel()).toContain("/deploy");

    press({ key: "Enter" });
    expect(command).toHaveBeenCalledWith(selectableItems[0]);
  });

  // MUL-3685: plain Tab accepts the highlighted item like Enter; Shift+Tab and
  // modifier+Tab fall through so reverse focus / OS switching are preserved.
  it("accepts the highlighted item on plain Tab, ignoring Shift/modifier+Tab", () => {
    const ref = createRef<SlashCommandListRef>();
    const command = vi.fn();
    const selectableItems: SlashCommandItem[] = [
      { id: "s1", label: "deploy", description: "Ship changes" },
      { id: "s2", label: "review", description: "Review code" },
    ];

    render(
      <I18nWrapper>
        <SlashCommandList
          ref={ref}
          items={selectableItems}
          query=""
          command={command}
        />
      </I18nWrapper>,
    );

    const press = (init: KeyboardEventInit) =>
      ref.current?.onKeyDown({ event: new KeyboardEvent("keydown", init) });

    expect(press({ key: "Tab", shiftKey: true })).toBe(false);
    expect(press({ key: "Tab", metaKey: true })).toBe(false);
    expect(command).not.toHaveBeenCalled();

    expect(press({ key: "Tab" })).toBe(true);
    expect(command).toHaveBeenCalledWith(selectableItems[0]);
  });

  it("lets Tab fall through when there are no selectable items, like Enter", () => {
    const ref = createRef<SlashCommandListRef>();

    render(
      <I18nWrapper>
        <SlashCommandList ref={ref} items={[]} query="" command={vi.fn()} />
      </I18nWrapper>,
    );

    expect(
      ref.current?.onKeyDown({
        event: new KeyboardEvent("keydown", { key: "Tab" }),
      }),
    ).toBe(false);
  });
});

describe("SlashCommandList empty states", () => {
  it("shows a configured-skills empty state before search text is entered", () => {
    const { getByText } = render(
      <I18nWrapper>
        <SlashCommandList items={[]} query="" command={vi.fn()} />
      </I18nWrapper>,
    );

    expect(getByText("No skills configured")).toBeInTheDocument();
  });

  it("shows a no-results empty state when search text has no matches", () => {
    const { getByText } = render(
      <I18nWrapper>
        <SlashCommandList items={[]} query="deploy" command={vi.fn()} />
      </I18nWrapper>,
    );

    expect(getByText("No matching skills")).toBeInTheDocument();
  });

  it("renders nothing on empty items when hideOnEmpty is set (command menu)", () => {
    const { container } = render(
      <I18nWrapper>
        <SlashCommandList items={[]} query="6" command={vi.fn()} hideOnEmpty />
      </I18nWrapper>,
    );

    // No popup box on a non-matching `/` (e.g. typing a date like 6/8).
    expect(container).toBeEmptyDOMElement();
  });
});

describe("buildBuiltinCommandItems", () => {
  it("returns the full built-in command set for an empty query", () => {
    expect(buildBuiltinCommandItems("")).toEqual(BUILTIN_COMMANDS);
  });

  it("includes /note while the query is a prefix of the label", () => {
    expect(buildBuiltinCommandItems("no").map((c) => c.id)).toEqual(["note"]);
    expect(buildBuiltinCommandItems("NOTE").map((c) => c.id)).toEqual(["note"]);
  });

  it("matches the label as a prefix only — not the description", () => {
    // "agent" appears in the description but is not a label prefix.
    expect(buildBuiltinCommandItems("agent")).toEqual([]);
    // A non-prefix substring of the label does not match either.
    expect(buildBuiltinCommandItems("ote")).toEqual([]);
  });

  it("returns nothing for a query that matches no command", () => {
    expect(buildBuiltinCommandItems("deploy")).toEqual([]);
  });
});

describe("SlashCommandList built-in command rendering", () => {
  it("renders the localized description for a built-in command", () => {
    const { getByText } = render(
      <I18nWrapper>
        <SlashCommandList
          items={buildBuiltinCommandItems("")}
          query=""
          command={vi.fn()}
          hideOnEmpty
        />
      </I18nWrapper>,
    );

    expect(getByText("/note")).toBeInTheDocument();
    expect(
      getByText("Add a note — won't trigger any agents"),
    ).toBeInTheDocument();
  });
});


// Async quick-action rendering in the `/` menu (MUL-5465, review finding #4).
//
// The render request resolves after an arbitrary delay, during which the user
// keeps typing. Three behaviours have to hold, and each one was a real bug at
// some point in this PR:
//   - a rejection must not destroy what the user typed
//   - a success must replace the ORIGINAL command, not wherever the caret is
//   - a command edited mid-flight must be left alone, not overwritten
describe("builtin `/` menu — async quick action rendering", () => {
  // Minimal editor stand-in: enough ProseMirror surface for the command to
  // read the range text and issue its chain.
  function fakeEditor(text: string) {
    const calls: { from: number; to: number; content: string; contentType?: string }[] = [];
    let docText = text;
    const chain = {
      focus: () => chain,
      insertContentAt: (
        range: { from: number; to: number },
        content: string,
        opts?: { contentType?: string },
      ) => {
        calls.push({ from: range.from, to: range.to, content, contentType: opts?.contentType });
        return chain;
      },
      insertContent: (content: string) => {
        calls.push({ from: -1, to: -1, content });
        return chain;
      },
      deleteRange: () => chain,
      run: () => true,
    };
    return {
      calls,
      setText: (next: string) => {
        docText = next;
      },
      editor: {
        chain: () => chain,
        state: {
          doc: {
            get content() {
              return { size: docText.length + 1 };
            },
            textBetween: (from: number, to: number) => docText.slice(from, to),
          },
        },
        view: { state: { selection: { $to: { nodeAfter: null } } } },
      },
    };
  }

  const range = { from: 0, to: 7 };
  const item = { id: `${QUICK_ACTION_ITEM_PREFIX}qa-1`, label: "review" };

  it("leaves the typed command intact and reports the failure when render rejects", async () => {
    const { editor, calls } = fakeEditor("/review");
    const onRenderError = vi.fn();
    const suggestion = createBuiltinCommandSuggestion({
      renderQuickAction: () => Promise.reject(new Error("boom")),
      onRenderError,
    });

    suggestion.command!({ editor, range, props: item } as never);
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(calls).toHaveLength(0);
    expect(onRenderError).toHaveBeenCalledTimes(1);
  });

  it("replaces the original range once a delayed render resolves", async () => {
    const { editor, calls } = fakeEditor("/review");
    let resolve!: (v: string) => void;
    const suggestion = createBuiltinCommandSuggestion({
      renderQuickAction: () => new Promise<string>((r) => { resolve = r; }),
    });

    suggestion.command!({ editor, range, props: item } as never);
    expect(calls).toHaveLength(0); // nothing destroyed while in flight

    await act(async () => {
      resolve("rendered body");
      await Promise.resolve();
      await Promise.resolve();
    });

    // contentType must be "markdown": inserted as a plain string, the
    // server-rendered `[@Name](mention://…)` lands as literal text and
    // serialises back out with escaped brackets, so the mention never becomes
    // a node and renders as raw markup in the thread.
    expect(calls).toEqual([
      { from: 0, to: 7, content: "rendered body", contentType: "markdown" },
    ]);
  });

  it("abandons the insert when the command was edited while the request was open", async () => {
    const { editor, calls, setText } = fakeEditor("/review");
    let resolve!: (v: string) => void;
    const suggestion = createBuiltinCommandSuggestion({
      renderQuickAction: () => new Promise<string>((r) => { resolve = r; }),
    });

    suggestion.command!({ editor, range, props: item } as never);
    // The user rewrites the command; a prefix-only check would still see a
    // leading "/" here and clobber it.
    setText("/fixnow");

    await act(async () => {
      resolve("rendered body");
      await Promise.resolve();
      await Promise.resolve();
    });

    expect(calls).toHaveLength(0);
  });
});

describe("DM statistic, archive, pin and unarchive commands", () => {
  type Name = "statistic" | "archive" | "pin" | "unarchive";
  const ACTIVE: Name[] = ["statistic", "archive", "pin"];

  function offered(enabled: Name[], query = "", canCompact = true): string[] {
    const config = createSlashCommandSuggestion(
      fakeQc({}),
      () => false,
      () => false,
      (command) => enabled.includes(command),
      () => canCompact,
    );
    const editor = new Editor({
      extensions: [StarterKit],
      content: { type: "doc", content: [{ type: "paragraph", content: [{ type: "text", text: `/${query}` }] }] },
    });
    editor.commands.setTextSelection(editor.state.doc.content.size - 1);
    try {
      return (config.items!({ query, editor, signal: new AbortController().signal }) as SlashCommandItem[]).map(
        (item) => item.id,
      );
    } finally {
      editor.destroy();
    }
  }

  it("offers nothing extra until a surface enables a command", () => {
    expect(offered([])).toEqual(["compact"]);
  });

  it("offers an active DM statistic, archive and pin ahead of compact", () => {
    expect(offered(ACTIVE)).toEqual(["statistic", "archive", "pin", "compact"]);
  });

  it("offers only unarchive on an archived DM, with compact turned off", () => {
    expect(offered(["unarchive"], "", false)).toEqual(["unarchive"]);
  });

  it("never offers unarchive together with archive or pin on the same DM", () => {
    expect(offered(ACTIVE)).not.toContain("unarchive");
    expect(offered(["unarchive"], "", false)).not.toContain("archive");
    expect(offered(["unarchive"], "", false)).not.toContain("pin");
  });

  it.each([
    ["s", "statistic"],
    ["ar", "archive"],
    ["P", "pin"],
  ])("matches the label prefix %j", (query, id) => {
    expect(offered(ACTIVE, query)).toEqual([id]);
  });

  it("matches unarchive by prefix on an archived DM", () => {
    expect(offered(["unarchive"], "un", false)).toEqual(["unarchive"]);
  });

  it.each(["statistic", "archive", "pin", "unarchive"] as const)(
    "renders /%s as a Multica command with a localized description and no argument hint",
    (name) => {
      const item: SlashCommandItem = { id: name, label: name, descriptionKey: name };
      const { getByRole, getByText } = render(
        <I18nWrapper><SlashCommandList items={[item]} query="" command={vi.fn()} /></I18nWrapper>,
      );
      const row = getByRole("button", { name: new RegExp(`^/${name}`) });
      expect(row).toHaveTextContent(enEditor.slash_command.commands[name]);
      expect(row).not.toHaveTextContent("[");
      expect(getByText("Multica")).toBeInTheDocument();
    },
  );

  it("inserts plain command text without a skill node", () => {
    const element = document.body.appendChild(document.createElement("div"));
    const editor = new Editor({ element, extensions: [StarterKit, Markdown, SlashCommandExtension], content: "<p>/ar</p>" });
    editor.commands.setTextSelection(4);
    window.getSelection()?.selectAllChildren(editor.view.dom);
    try {
      createSlashCommandSuggestion(fakeQc({})).command!({
        editor,
        range: { from: 1, to: 4 },
        props: { id: "archive", label: "archive", descriptionKey: "archive" },
      });
      expect(editor.getMarkdown()).toBe("/archive ");
      expect(editor.getJSON().content?.[0]?.content?.[0]?.type).toBe("text");
    } finally {
      editor.destroy();
      element.remove();
    }
  });

  it.each([enEditor, frEditor, jaEditor, koEditor, zhEditor])("localizes every command in every locale", (locale) => {
    for (const name of ["statistic", "archive", "pin", "unarchive"]) {
      expect(locale.slash_command.commands).toHaveProperty(name, expect.any(String));
    }
  });
});
