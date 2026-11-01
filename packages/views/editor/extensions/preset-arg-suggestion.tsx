"use client";

import { Extension } from "@tiptap/core";
import { PluginKey } from "@tiptap/pm/state";
import { Suggestion, type SuggestionMatch, type Trigger } from "@tiptap/suggestion";
import type { QueryClient } from "@tanstack/react-query";
import { agentPresetCompletionsOptions, filterPresetCompletions } from "@multica/core/agents";
import { useChatStore } from "@multica/core/chat";
import { getCurrentWsId } from "@multica/core/platform";
import {
  SlashCommandList,
  type SlashCommandItem,
  type SlashCommandListRef,
} from "./slash-command-suggestion";
import { createSuggestionPopupRender } from "./suggestion-popup";

export const PRESET_ARG_PLUGIN_KEY = new PluginKey("presetArgSuggestion");

// `/preset <name>` is a DM command the server answers; the menu opens only
// while the message so far is exactly `/preset` plus the word being typed.
const PRESET_ARG = /^\/preset\s+(\S*)$/;

export function findPresetArgMatch({ $position }: Pick<Trigger, "$position">): SuggestionMatch {
  const before = $position.parent.textBetween(0, $position.parentOffset, undefined, "￼");
  const query = PRESET_ARG.exec(before)?.[1];
  if (query === undefined) return null;
  return { range: { from: $position.pos - query.length, to: $position.pos }, query, text: query };
}

/** `names` already lead with `base` and `reset` (the completions endpoint). */
export const presetArgItems = (names: string[], query: string): SlashCommandItem[] =>
  filterPresetCompletions(names, query).map((name) => ({ id: name, label: name, plain: true }));

export function createPresetArgExtension({ getNames }: { getNames: () => Promise<string[]> }) {
  return Extension.create({
    name: "presetArgSuggestion",
    addProseMirrorPlugins() {
      return [
        Suggestion<SlashCommandItem>({
          editor: this.editor,
          pluginKey: PRESET_ARG_PLUGIN_KEY,
          char: "/",
          findSuggestionMatch: findPresetArgMatch,
          items: async ({ query }) => presetArgItems(await getNames(), query),
          // The trailing space ends the match, so Enter then sends the command.
          command: ({ editor, range, props }) => {
            editor.chain().focus().insertContentAt(range, `${props.label} `).run();
          },
          render: createSuggestionPopupRender<SlashCommandItem, SlashCommandItem, SlashCommandListRef, Parameters<typeof SlashCommandList>[0]>({
            pluginKey: PRESET_ARG_PLUGIN_KEY,
            component: SlashCommandList,
            getProps: (props) => ({
              items: props.items,
              query: props.query,
              command: props.command,
              hideOnEmpty: true,
            }),
            onKeyDown: (ref, props) => ref?.onKeyDown(props) ?? false,
          }),
        }),
      ];
    },
  });
}

/**
 * The selected agent's `/preset` completions. Tiptap calls items outside React,
 * so the store is read directly (as the `/` skill picker does). A failed fetch
 * offers nothing rather than an error in the composer.
 */
export const presetNames = (qc: QueryClient, enabled: () => boolean) => async (): Promise<string[]> => {
  const wsId = getCurrentWsId();
  const agentId = useChatStore.getState().selectedAgentId;
  if (!enabled() || !wsId || !agentId) return [];
  try {
    return await qc.fetchQuery({ ...agentPresetCompletionsOptions(wsId, agentId), staleTime: 10_000 });
  } catch {
    return [];
  }
};
