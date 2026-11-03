import { afterEach, describe, expect, it } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import type { TaskInitContext } from "@multica/core/types";
import { renderWithI18n } from "../../test/i18n";
import enAgents from "../../locales/en/agents.json";
import { RunInitContextDetails, initContextOf } from "./run-init-context-details";

const part = (name: TaskInitContext["parts"][number]["part"], tokens: number, budget = 0, notes?: string[]) => ({
  part: name,
  bytes: tokens * 4,
  tokens,
  budget,
  ...(notes ? { notes } : {}),
});

const report = (): TaskInitContext => ({
  parts: [
    part("instructions", 1200, 1000, ["over budget by 200 est. tokens"]),
    part("tools", 300, 500),
    part("skills", 90),
    part("memory", 0),
    part("project", 40),
    part("task", 12),
    part("seed", 0),
  ],
});

const t = enAgents.transcript;

describe("RunInitContextDetails", () => {
  afterEach(cleanup);

  it("lists one row per part with its estimated tokens", () => {
    renderWithI18n(<RunInitContextDetails report={report()} />);
    expect(screen.getByText(t.details_init_context)).toBeInTheDocument();
    for (const name of ["instructions", "tools", "skills", "memory", "project", "task", "seed"]) {
      expect(screen.getByText(name)).toBeInTheDocument();
    }
    expect(screen.getByText(/1,?200/)).toBeInTheDocument();
  });

  it("marks only the part that is over its budget and shows its notes", () => {
    renderWithI18n(<RunInitContextDetails report={report()} />);
    expect(screen.getAllByText(t.details_init_context_over_budget)).toHaveLength(1);
    expect(screen.getByText("over budget by 200 est. tokens")).toBeInTheDocument();
  });

  it("renders nothing when the run has no report", () => {
    const { container } = renderWithI18n(<RunInitContextDetails report={undefined} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing for a malformed report", () => {
    const { container } = renderWithI18n(<RunInitContextDetails report={{} as TaskInitContext} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("reads the report from a run result and tolerates results without one", () => {
    expect(initContextOf({ init_context: report() })).toEqual(report());
    for (const result of [null, undefined, "done", {}, { init_context: null }]) {
      expect(initContextOf(result)).toBeUndefined();
    }
  });
});
