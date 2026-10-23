import { describe, expect, it } from "vitest";
import {
  CHAT_COMMANDS,
  CHAT_COMMANDS_BY_ID,
  filterChatCommands,
  getAvailableChatCommands,
  getChatCommand,
  getChatSessionMenuItems,
  matchesChatCommandQuery,
  parseChatCommandInput,
} from "./commands";

describe("chat command registry", () => {
  it("registers all seven built-in DM commands in composer order with complete metadata", () => {
    expect(CHAT_COMMANDS.map((cmd) => cmd.id)).toEqual([
      "statistic",
      "archive",
      "pin",
      "unarchive",
      "compact",
      "rename",
      "fork",
    ]);

    for (const cmd of CHAT_COMMANDS) {
      expect(CHAT_COMMANDS_BY_ID[cmd.id]).toBe(cmd);
      expect(getChatCommand(cmd.id)).toBe(cmd);
      expect(cmd.i18nKey).toBe(cmd.id);
      expect(cmd.descriptionKey).toBe(cmd.id);
    }
  });

  it("classifies client vs prompt commands and argument policies (DX-3, DES-3, DES-5)", () => {
    expect(CHAT_COMMANDS_BY_ID.statistic).toMatchObject({
      kind: "client",
      args: "none",
      argumentHint: null,
      argumentHintKey: null,
      searchTerms: ["statistic", "stats", "statistics", "usage"],
    });
    expect(CHAT_COMMANDS_BY_ID.archive).toMatchObject({
      kind: "client",
      args: "none",
      argumentHint: null,
      argumentHintKey: null,
    });
    expect(CHAT_COMMANDS_BY_ID.pin).toMatchObject({
      kind: "client",
      args: "none",
      argumentHint: null,
      argumentHintKey: null,
    });
    expect(CHAT_COMMANDS_BY_ID.unarchive).toMatchObject({
      kind: "client",
      args: "none",
      argumentHint: null,
      argumentHintKey: null,
    });
    expect(CHAT_COMMANDS_BY_ID.compact).toMatchObject({
      kind: "prompt",
      args: "optional_text",
      argumentHint: "[focus]",
      argumentHintKey: "focus_hint",
    });
    expect(CHAT_COMMANDS_BY_ID.rename).toMatchObject({
      kind: "client",
      args: "required_text",
      argumentHint: "<title>",
      argumentHintKey: "title_hint",
    });
    expect(CHAT_COMMANDS_BY_ID.fork).toMatchObject({
      kind: "client",
      args: "optional_text",
      argumentHint: "[message]",
      argumentHintKey: "message_hint",
    });
  });

  it("enforces active vs archived command availability (A2, ENG-10)", () => {
    const activeComposer = getAvailableChatCommands({ archived: false, surface: "composer" });
    expect(activeComposer.map((cmd) => cmd.id)).toEqual([
      "statistic",
      "archive",
      "pin",
      "compact",
      "rename",
      "fork",
    ]);

    const archivedComposer = getAvailableChatCommands({ archived: true, surface: "composer" });
    expect(archivedComposer.map((cmd) => cmd.id)).toEqual(["unarchive"]);

    // In menu context, Statistics and Rename stay available on archived DMs while Pin and Archive hide.
    expect(CHAT_COMMANDS_BY_ID.statistic.isAvailable({ archived: true, surface: "menu" })).toBe(true);
    expect(CHAT_COMMANDS_BY_ID.statistic.isAvailable({ archived: true, surface: "composer" })).toBe(false);
    expect(CHAT_COMMANDS_BY_ID.pin.isAvailable({ archived: true, surface: "menu" })).toBe(false);
    expect(CHAT_COMMANDS_BY_ID.archive.isAvailable({ archived: true, surface: "menu" })).toBe(false);
    expect(CHAT_COMMANDS_BY_ID.unarchive.isAvailable({ archived: true, surface: "menu" })).toBe(true);
    expect(CHAT_COMMANDS_BY_ID.unarchive.isAvailable({ archived: false, surface: "menu" })).toBe(false);
  });

  it("matches /statistic by search terms stats, statistics, and usage, and lists all available commands on empty query (DES-5)", () => {
    for (const q of ["s", "stat", "stats", "statistics", "usage", "USAGE"]) {
      expect(matchesChatCommandQuery(CHAT_COMMANDS_BY_ID.statistic, q)).toBe(true);
      expect(filterChatCommands(q, { archived: false }).map((c) => c.id)).toContain("statistic");
    }

    expect(filterChatCommands("", { archived: false }).map((c) => c.id)).toEqual([
      "statistic",
      "archive",
      "pin",
      "compact",
      "rename",
      "fork",
    ]);
    expect(filterChatCommands("", { archived: true }).map((c) => c.id)).toEqual(["unarchive"]);
  });

  it("parses standalone case-insensitive no-arg commands and rejects extra text or attachments on client commands", () => {
    for (const input of ["/archive", "  /ARCHIVE  ", "/UnArchive", "/Statistic", "/PIN"]) {
      const parsed = parseChatCommandInput(input);
      expect(parsed).toMatchObject({ status: "ok", args: "" });
    }

    for (const input of ["/archive extra", "/unarchive now", "/statistic today", "/pin top"]) {
      const parsed = parseChatCommandInput(input);
      expect(parsed).toMatchObject({
        status: "invalid",
        reason: "no_arguments",
      });
    }

    for (const input of ["/archive", "/unarchive", "/statistic", "/pin", "/rename New title", "/fork", "/fork continue with plan B"]) {
      const parsed = parseChatCommandInput(input, { hasAttachments: true });
      expect(parsed).toMatchObject({
        status: "invalid",
        reason: "no_attachments",
      });
    }

    // /compact is a prompt command and allows attachments
    expect(parseChatCommandInput("/compact focus", { hasAttachments: true })).toMatchObject({
      status: "ok",
      args: "focus",
    });
  });

  it("preserves L2 session menu order and A2 archived visibility", () => {
    expect(getChatSessionMenuItems({ archived: false }).map((item) => item.id)).toEqual([
      "statistic",
      "pin",
      "rename",
      "archive",
    ]);
    expect(getChatSessionMenuItems({ archived: true }).map((item) => item.id)).toEqual([
      "statistic",
      "rename",
      "unarchive",
      "delete",
    ]);
    const deleteItem = getChatSessionMenuItems({ archived: true }).find((item) => item.id === "delete");
    expect(deleteItem).toMatchObject({
      id: "delete",
      section: "trailing",
      destructive: true,
      requiresConfirmation: true,
    });
  });
});
