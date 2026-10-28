import { createElement } from "react";
import { expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import type { WakeupCondition } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import type { SupportedLocale } from "@multica/core/i18n";
import { RESOURCES } from "../../test/i18n";
import { formatWakeupTime, useWakeupText } from "./wakeup-presentation";

vi.mock("./wakeup-condition-names", () => ({
  useConditionNames: () => ({ status: (key: string) => key, label: () => undefined, property: () => undefined, actor: (_type: string, id: string) => id }),
}));
vi.mock("../../common/use-viewing-timezone", () => ({ useViewingTimezone: () => "UTC" }));

it("uses the configured timezone to distinguish today from the next day", () => {
  const now = new Date("2026-09-16T15:00:00Z");
  expect(formatWakeupTime("2026-09-16T15:30:00Z", "en-US", "Asia/Shanghai", now)).not.toContain("Sep");
  expect(formatWakeupTime("2026-09-16T16:30:00Z", "en-US", "Asia/Shanghai", now)).toContain("Sep 17");
});

it("includes the year across the configured timezone's new year", () => {
  const now = new Date("2026-12-31T15:00:00Z");
  expect(formatWakeupTime("2026-12-31T16:30:00Z", "en-US", "Asia/Shanghai", now)).toContain("2027");
});

it("reads a github_run condition as a sentence in every locale", () => {
  const sha = "59819ba721b4d542791db013ec2bb43ed9d66503";
  const byId: WakeupCondition = { type: "github_run", repo: "cheese-work/multica", run_id: 40343069164, state: "waiting" };
  const bySha: WakeupCondition = { type: "github_run", repo: "cheese-work/multica", workflow: "cd-qualification.yml", head_sha: sha, state: "completed" };
  const read = (locale: SupportedLocale) =>
    renderHook(() => useWakeupText(), {
      wrapper: ({ children }) => createElement(I18nProvider, { locale, resources: RESOURCES, children }),
    }).result.current;
  const en = read("en");
  expect(en.condition(byId)).toBe("When run 40343069164 of cheese-work/multica waits at an approval gate");
  expect(en.conditionWait(byId)).toBe("Waiting for run 40343069164 of cheese-work/multica to reach an approval gate");
  expect(en.condition(bySha)).toBe("When cd-qualification.yml at 59819ba of cheese-work/multica finishes");
  expect(en.conditionWait(bySha)).toBe("Waiting for cd-qualification.yml at 59819ba of cheese-work/multica to finish");
  for (const locale of ["fr", "ja", "ko", "zh-Hans"] as const) {
    const text = read(locale);
    expect(text.condition(byId)).toContain("40343069164");
    expect(text.conditionWait(bySha)).toContain("59819ba");
    expect(text.condition(byId)).not.toBe(en.condition(byId));
  }
});
