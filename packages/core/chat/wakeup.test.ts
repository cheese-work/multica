// @vitest-environment node
import { describe, expect, it } from "vitest";
import { isWakeupHeaderContent, stripWakeupHeader } from "./wakeup";

const HEADER = "[Scheduled wakeup — you set this earlier in this DM; no person wrote this message.]";

describe("wakeup header", () => {
  it("recognises the model-facing header", () => {
    expect(isWakeupHeaderContent(`${HEADER}\nInstruction: report`)).toBe(true);
    expect(isWakeupHeaderContent("hello [Scheduled wakeup — x]")).toBe(false);
  });

  it("strips the header and the Instruction label, keeping the instruction verbatim", () => {
    expect(stripWakeupHeader(`${HEADER}\nInstruction: report on CHE-1\nline 2`)).toBe("report on CHE-1\nline 2");
  });

  it("leaves ordinary messages untouched", () => {
    expect(stripWakeupHeader("[Scheduled wakeup is a feature]")).toBe("[Scheduled wakeup is a feature]");
    expect(stripWakeupHeader("hi")).toBe("hi");
  });
});
