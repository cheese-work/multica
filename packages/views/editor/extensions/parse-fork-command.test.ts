// @vitest-environment node

import { describe, expect, it } from "vitest";
import { parseForkCommand } from "./parse-fork-command";

describe("parseForkCommand", () => {
  it.each([
    ["/fork", ""],
    ["/fork ", ""],
    ["/fork some text", "some text"],
    ["/fork   padded  ", "padded"],
    ["\n\n/fork hello", "hello"],
    ["  \n  /fork hello", "hello"],
    ["/fork\nsecond line", "second line"],
    ["/fork first\nsecond", "first\nsecond"],
  ])("forks %j with message %j", (input, message) => {
    expect(parseForkCommand(input)).toEqual({ message });
  });

  it.each([
    "",
    "hello",
    "please /fork",
    "first line\n/fork",
    "/forked",
    "/fork-it",
    "/Fork",
    "/forks now",
    "fork",
  ])("does not fork %j", (input) => {
    expect(parseForkCommand(input)).toBeNull();
  });
});
