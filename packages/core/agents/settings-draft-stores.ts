"use client";

import { createKeyedDraftStore } from "../drafts/create-keyed-draft-store";
import type { AgentConversationStarter } from "../types";

/**
 * Unsent edits on the agent settings surfaces, one entry per agent (the MCP
 * dialog: per agent and server). `null` means "no edit"; a surface writes a
 * field only while it differs from the saved value, so an entry that matches
 * the server drops out on its own.
 *
 * Secret material stays out: env values, MCP env/header values and the raw MCP
 * JSON are never written here (see {@link useAgentEnvDraftStore}).
 */

export interface AgentInstructionsDraft {
  value: string | null;
}
export const EMPTY_AGENT_INSTRUCTIONS_DRAFT: AgentInstructionsDraft = { value: null };
export const useAgentInstructionsDraftStore = createKeyedDraftStore<AgentInstructionsDraft>({
  storageKey: "multica:agents:instructions-draft",
  emptyData: EMPTY_AGENT_INSTRUCTIONS_DRAFT,
  hasMeaningful: (d) => d.value !== null,
});

export interface AgentProfileDraft {
  name: string | null;
  description: string | null;
  starters: AgentConversationStarter[] | null;
}
export const EMPTY_AGENT_PROFILE_DRAFT: AgentProfileDraft = {
  name: null,
  description: null,
  starters: null,
};
export const useAgentProfileDraftStore = createKeyedDraftStore<AgentProfileDraft>({
  storageKey: "multica:agents:profile-draft",
  emptyData: EMPTY_AGENT_PROFILE_DRAFT,
  hasMeaningful: (d) => d.name !== null || d.description !== null || d.starters !== null,
});

export interface AgentCustomArgsDraft {
  args: string[] | null;
  /** The open add/edit field, by position: entry ids do not survive a remount. */
  editor: { index: number | null; value: string } | null;
}
export const EMPTY_AGENT_CUSTOM_ARGS_DRAFT: AgentCustomArgsDraft = { args: null, editor: null };
export const useAgentCustomArgsDraftStore = createKeyedDraftStore<AgentCustomArgsDraft>({
  storageKey: "multica:agents:custom-args-draft",
  emptyData: EMPTY_AGENT_CUSTOM_ARGS_DRAFT,
  hasMeaningful: (d) => d.args !== null || d.editor !== null,
});

/**
 * Env values are secrets: the server withholds them until an audited reveal,
 * so they must not land in localStorage in clear text. Only the key NAMES the
 * user added are kept; the value is re-entered after a reload.
 */
export interface AgentEnvDraft {
  keys: string[];
}
export const EMPTY_AGENT_ENV_DRAFT: AgentEnvDraft = { keys: [] };
export const useAgentEnvDraftStore = createKeyedDraftStore<AgentEnvDraft>({
  storageKey: "multica:agents:env-draft",
  emptyData: EMPTY_AGENT_ENV_DRAFT,
  hasMeaningful: (d) => d.keys.length > 0,
});

/**
 * MCP server dialog. Name, transport, command, args, URL and the JSON text
 * with credential-named keys removed. Env and header values never
 * reach storage.
 */
export interface McpDialogDraft {
  name: string | null;
  transport: "stdio" | "http" | null;
  command: string | null;
  args: string[] | null;
  url: string | null;
  /** JSON editor text, credential-named keys removed (`mcp-draft.ts`). Set means JSON mode. */
  json: string | null;
  /** Set when the edit moved the destination or removed a credential: saved credentials stay out on reopen. */
  dropCredentials: true | null;
}
export const EMPTY_MCP_DIALOG_DRAFT: McpDialogDraft = {
  name: null,
  transport: null,
  command: null,
  args: null,
  url: null,
  json: null,
  dropCredentials: null,
};
export const useMcpDialogDraftStore = createKeyedDraftStore<McpDialogDraft>({
  storageKey: "multica:agents:mcp-dialog-draft",
  emptyData: EMPTY_MCP_DIALOG_DRAFT,
  hasMeaningful: (d) => Object.values(d).some((v) => v !== null),
});
