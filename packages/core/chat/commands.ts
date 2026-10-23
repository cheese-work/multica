export type ChatCommandId =
  | "statistic"
  | "archive"
  | "pin"
  | "unarchive"
  | "compact"
  | "rename"
  | "fork";

export type ChatCommandKind = "client" | "prompt";

export type ChatCommandArgsPolicy = "none" | "optional_text" | "required_text";

export type ChatCommandHintKey = "focus_hint" | "title_hint" | "message_hint";

export interface ChatCommandContext {
  archived?: boolean;
  surface?: "composer" | "menu";
  hasStatistic?: boolean;
  hasArchive?: boolean;
  hasPin?: boolean;
  hasUnarchive?: boolean;
  hasCompact?: boolean;
  hasRename?: boolean;
  hasFork?: boolean;
}

export interface ChatCommandDefinition {
  id: ChatCommandId;
  i18nKey: ChatCommandId;
  descriptionKey: ChatCommandId;
  kind: ChatCommandKind;
  args: ChatCommandArgsPolicy;
  argumentHint: string | null;
  argumentHintKey: ChatCommandHintKey | null;
  searchTerms: readonly string[];
  isAvailable: (ctx?: ChatCommandContext) => boolean;
}

export type ParsedChatCommandResult =
  | {
      status: "ok";
      command: ChatCommandDefinition;
      args: string;
    }
  | {
      status: "invalid";
      command: ChatCommandDefinition;
      reason: "no_arguments" | "missing_arguments" | "no_attachments";
      rawArgs: string;
    };

export type ChatSessionMenuItemId = "statistic" | "pin" | "rename" | "archive" | "unarchive" | "delete";

export interface ChatSessionMenuItemDescriptor {
  id: ChatSessionMenuItemId;
  section: "main" | "trailing";
  commandId: ChatCommandId | null;
  destructive?: boolean;
  requiresConfirmation?: boolean;
}

export const CHAT_COMMANDS: readonly ChatCommandDefinition[] = [
  {
    id: "statistic",
    i18nKey: "statistic",
    descriptionKey: "statistic",
    kind: "client",
    args: "none",
    argumentHint: null,
    argumentHintKey: null,
    searchTerms: ["statistic", "stats", "statistics", "usage"],
    isAvailable: (ctx = {}) =>
      (ctx.surface === "menu" ? true : !ctx.archived) && (ctx.hasStatistic ?? true),
  },
  {
    id: "archive",
    i18nKey: "archive",
    descriptionKey: "archive",
    kind: "client",
    args: "none",
    argumentHint: null,
    argumentHintKey: null,
    searchTerms: ["archive"],
    isAvailable: (ctx = {}) => !ctx.archived && (ctx.hasArchive ?? true),
  },
  {
    id: "pin",
    i18nKey: "pin",
    descriptionKey: "pin",
    kind: "client",
    args: "none",
    argumentHint: null,
    argumentHintKey: null,
    searchTerms: ["pin"],
    isAvailable: (ctx = {}) => !ctx.archived && (ctx.hasPin ?? true),
  },
  {
    id: "unarchive",
    i18nKey: "unarchive",
    descriptionKey: "unarchive",
    kind: "client",
    args: "none",
    argumentHint: null,
    argumentHintKey: null,
    searchTerms: ["unarchive"],
    isAvailable: (ctx = {}) => Boolean(ctx.archived) && (ctx.hasUnarchive ?? true),
  },
  {
    id: "compact",
    i18nKey: "compact",
    descriptionKey: "compact",
    kind: "prompt",
    args: "optional_text",
    argumentHint: "[focus]",
    argumentHintKey: "focus_hint",
    searchTerms: ["compact"],
    isAvailable: (ctx = {}) => !ctx.archived && (ctx.hasCompact ?? true),
  },
  {
    id: "rename",
    i18nKey: "rename",
    descriptionKey: "rename",
    kind: "client",
    args: "required_text",
    argumentHint: "<title>",
    argumentHintKey: "title_hint",
    searchTerms: ["rename"],
    isAvailable: (ctx = {}) =>
      (ctx.surface === "menu" ? true : !ctx.archived) && (ctx.hasRename ?? true),
  },
  {
    id: "fork",
    i18nKey: "fork",
    descriptionKey: "fork",
    kind: "client",
    args: "optional_text",
    argumentHint: "[message]",
    argumentHintKey: "message_hint",
    searchTerms: ["fork"],
    isAvailable: (ctx = {}) => !ctx.archived && (ctx.hasFork ?? true),
  },
];

export const CHAT_COMMANDS_BY_ID: Record<ChatCommandId, ChatCommandDefinition> = {
  statistic: CHAT_COMMANDS[0]!,
  archive: CHAT_COMMANDS[1]!,
  pin: CHAT_COMMANDS[2]!,
  unarchive: CHAT_COMMANDS[3]!,
  compact: CHAT_COMMANDS[4]!,
  rename: CHAT_COMMANDS[5]!,
  fork: CHAT_COMMANDS[6]!,
};

export const CHAT_SESSION_MENU_ITEMS: readonly ChatSessionMenuItemDescriptor[] = [
  { id: "statistic", section: "main", commandId: "statistic" },
  { id: "pin", section: "main", commandId: "pin" },
  { id: "rename", section: "main", commandId: "rename" },
  { id: "archive", section: "trailing", commandId: "archive" },
  { id: "unarchive", section: "trailing", commandId: "unarchive" },
  {
    id: "delete",
    section: "trailing",
    commandId: null,
    destructive: true,
    requiresConfirmation: true,
  },
];

export function getChatCommand(id: string): ChatCommandDefinition | undefined {
  return (CHAT_COMMANDS_BY_ID as Record<string, ChatCommandDefinition | undefined>)[id];
}

export function getAvailableChatCommands(ctx: ChatCommandContext = {}): ChatCommandDefinition[] {
  return CHAT_COMMANDS.filter((command) => command.isAvailable(ctx));
}

export function matchesChatCommandQuery(command: ChatCommandDefinition, query: string): boolean {
  const normalized = query.trim().toLowerCase();
  if (!normalized) return true;
  return (
    command.id.startsWith(normalized) ||
    command.searchTerms.some((term) => term.toLowerCase().startsWith(normalized))
  );
}

export function filterChatCommands(
  query: string,
  ctx: ChatCommandContext = {},
): ChatCommandDefinition[] {
  return getAvailableChatCommands(ctx).filter((command) => matchesChatCommandQuery(command, query));
}

export function parseChatCommandInput(
  text: string,
  options?: { hasAttachments?: boolean },
): ParsedChatCommandResult | null {
  const trimmed = text.trim();
  if (!trimmed.startsWith("/")) return null;

  const noArgMatch = /^\/(statistic|archive|pin|unarchive)(?:\s+([\s\S]+))?$/i.exec(trimmed);
  if (noArgMatch) {
    const commandId = noArgMatch[1]!.toLowerCase() as ChatCommandId;
    const command = CHAT_COMMANDS_BY_ID[commandId];
    const extraArgs = (noArgMatch[2] ?? "").trim();
    if (extraArgs) {
      return { status: "invalid", command, reason: "no_arguments", rawArgs: extraArgs };
    }
    if (options?.hasAttachments) {
      return { status: "invalid", command, reason: "no_attachments", rawArgs: "" };
    }
    return { status: "ok", command, args: "" };
  }

  const otherMatch = /^\/(compact|rename|fork)(?:\s+([\s\S]*))?$/.exec(trimmed);
  if (!otherMatch) return null;

  const commandId = otherMatch[1]! as ChatCommandId;
  const command = CHAT_COMMANDS_BY_ID[commandId];
  const rawArgs = (otherMatch[2] ?? "").trim();

  if (command.kind === "client" && options?.hasAttachments) {
    return { status: "invalid", command, reason: "no_attachments", rawArgs };
  }

  if (command.id === "rename" && !rawArgs) {
    return { status: "invalid", command, reason: "missing_arguments", rawArgs: "" };
  }

  return { status: "ok", command, args: rawArgs };
}

export function getChatSessionMenuItems(
  ctx: { archived?: boolean } = {},
): ChatSessionMenuItemDescriptor[] {
  const archived = Boolean(ctx.archived);
  return CHAT_SESSION_MENU_ITEMS.filter((item) => {
    if (item.commandId === null) {
      return archived;
    }
    return CHAT_COMMANDS_BY_ID[item.commandId].isAvailable({
      archived,
      surface: "menu",
    });
  });
}
