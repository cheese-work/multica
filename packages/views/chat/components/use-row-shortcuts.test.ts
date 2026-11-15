// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import type { KeyboardEvent } from "react";
import { handleRowShortcut } from "./use-row-shortcuts";

function keyEvent(
  key: string,
  init: Partial<{
    target: unknown;
    ctrlKey: boolean;
    metaKey: boolean;
    altKey: boolean;
    defaultPrevented: boolean;
  }> = {},
) {
  const preventDefault = vi.fn();
  const event = {
    key,
    ctrlKey: false,
    metaKey: false,
    altKey: false,
    defaultPrevented: false,
    target: { tagName: "DIV", isContentEditable: false },
    preventDefault,
    ...init,
  } as unknown as KeyboardEvent<HTMLElement>;
  return { event, preventDefault };
}

const items = () => ({
  u: vi.fn(),
  p: vi.fn(),
  e: vi.fn(),
  r: vi.fn(),
  x: vi.fn(),
});

function toItems(fns: ReturnType<typeof items>) {
  return [
    { key: "unseen", shortcut: "U", onSelect: fns.u },
    { key: "pin", shortcut: "P", onSelect: fns.p },
    { key: "archive", shortcut: "E", onSelect: fns.e },
    { key: "rename", shortcut: "R", onSelect: fns.r },
    { key: "select", shortcut: "X", onSelect: fns.x },
  ];
}

describe("handleRowShortcut", () => {
  it.each([
    ["u", "u"],
    ["p", "p"],
    ["e", "e"],
    ["r", "r"],
    ["x", "x"],
    ["U", "u"],
  ] as const)("key %s runs the %s action once", (key, name) => {
    const fns = items();
    const { event, preventDefault } = keyEvent(key);

    handleRowShortcut(event, toItems(fns));

    expect(fns[name]).toHaveBeenCalledTimes(1);
    for (const other of Object.keys(fns) as (keyof typeof fns)[]) {
      if (other !== name) expect(fns[other]).not.toHaveBeenCalled();
    }
    expect(preventDefault).toHaveBeenCalled();
  });

  it("ignores a key typed into an input", () => {
    const fns = items();
    const { event } = keyEvent("p", { target: { tagName: "INPUT", isContentEditable: false } });

    handleRowShortcut(event, toItems(fns));

    expect(fns.p).not.toHaveBeenCalled();
  });

  it("ignores a key typed into a contenteditable", () => {
    const fns = items();
    const { event } = keyEvent("p", { target: { tagName: "DIV", isContentEditable: true } });

    handleRowShortcut(event, toItems(fns));

    expect(fns.p).not.toHaveBeenCalled();
  });

  it.each(["ctrlKey", "metaKey", "altKey"] as const)("ignores the key with %s held", (mod) => {
    const fns = items();
    const { event } = keyEvent("e", { [mod]: true });

    handleRowShortcut(event, toItems(fns));

    expect(fns.e).not.toHaveBeenCalled();
  });

  it("ignores a key that is not a shortcut, and an already-handled event", () => {
    const fns = items();
    handleRowShortcut(keyEvent("q").event, toItems(fns));
    handleRowShortcut(keyEvent("p", { defaultPrevented: true }).event, toItems(fns));

    expect(Object.values(fns).every((fn) => fn.mock.calls.length === 0)).toBe(true);
  });
});
