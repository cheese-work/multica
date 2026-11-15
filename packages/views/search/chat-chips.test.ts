// @vitest-environment node
import { describe, expect, it } from "vitest";
import type { ChatSession } from "@multica/core/types";
import { chipForTab, matchesChips, toggleChip, type ChatChip, type ChipCandidate } from "./chat-chips";

const scope = (view: Extract<ChatChip, { type: "scope" }>["view"]): ChatChip => ({ type: "scope", view });
const agent = (id: string, name = id): ChatChip => ({ type: "agent", id, name });

const session = (over: Partial<ChatSession> & { id: string }) =>
  ({ agent_id: "tofu", status: "active", has_unread: false, ...over }) as ChatSession;

const rows = [
  session({ id: "main-tofu" }),
  session({ id: "waiting-tofu", stale_level: "red" }),
  session({ id: "waiting-luna", agent_id: "luna", stale_level: "red" }),
  session({ id: "unseen-luna", agent_id: "luna", has_unread: true }),
  session({ id: "archived-tofu", status: "archived" }),
];
const pick = (chips: ChatChip[]) => rows.filter((s) => matchesChips(chips, s, new Set())).map((s) => s.id);

describe("chip combination rules", () => {
  it("different types AND: a scope chip and an agent chip both have to match", () => {
    expect(pick([scope("waiting"), agent("tofu")])).toEqual(["waiting-tofu"]);
  });

  it("same type OR: two scope chips return the union, two agent chips return the union", () => {
    expect(pick([scope("waiting"), scope("unseen")])).toEqual(["waiting-tofu", "waiting-luna", "unseen-luna"]);
    expect(pick([agent("tofu"), agent("luna")])).toHaveLength(5);
  });

  it("no chips, or Tất cả, filters nothing; Lưu trữ reaches archived rows", () => {
    expect(pick([])).toHaveLength(5);
    expect(pick([scope("all")])).toHaveLength(5);
    expect(pick([scope("archived")])).toEqual(["archived-tofu"]);
  });

  it("Quyết định uses the open-decision session ids", () => {
    const hit = matchesChips([scope("decisions")], rows[0]!, new Set(["main-tofu"]));
    expect(hit).toBe(true);
    expect(matchesChips([scope("decisions")], rows[1]!, new Set(["main-tofu"]))).toBe(false);
  });
});

describe("toggleChip", () => {
  it("Tất cả is exclusive: picking it clears the others, picking another clears it", () => {
    const withAll = toggleChip([scope("waiting"), agent("tofu")], scope("all"));
    expect(withAll).toEqual([scope("all")]);
    expect(toggleChip(withAll, scope("pinned"))).toEqual([scope("pinned")]);
  });

  it("toggles a chip off again and keeps activation order", () => {
    const two = toggleChip(toggleChip([], scope("waiting")), agent("tofu"));
    expect(two).toEqual([scope("waiting"), agent("tofu")]);
    expect(toggleChip(two, scope("waiting"))).toEqual([agent("tofu")]);
  });
});

describe("chipForTab", () => {
  const candidates: ChipCandidate[] = [
    { chip: scope("all"), label: "All" },
    { chip: scope("main"), label: "Main" },
    { chip: scope("waiting"), label: "Waiting on you" },
    { chip: agent("tofu", "Tofu"), label: "Tofu" },
  ];

  it("empty text adds the first inactive chip after All", () => {
    expect(chipForTab("", candidates, [])).toEqual(scope("main"));
    expect(chipForTab("", candidates, [scope("main")])).toEqual(scope("waiting"));
  });

  it("typed text picks the first inactive chip whose label starts with it", () => {
    expect(chipForTab("tof", candidates, [])).toEqual(agent("tofu", "Tofu"));
    expect(chipForTab("wait", candidates, [])).toEqual(scope("waiting"));
  });

  it("returns null when nothing matches or everything is on", () => {
    expect(chipForTab("zzz", candidates, [])).toBeNull();
    expect(chipForTab("tof", candidates, [agent("tofu")])).toBeNull();
  });
});
