// @vitest-environment node
import { describe, expect, it } from "vitest";
import { orientChart, pickOrientation } from "./mermaid-orientation";

describe("orientChart", () => {
  it.each([
    ["flowchart TD\n  A-->B", "horizontal", "flowchart LR\n  A-->B"],
    ["flowchart TB\n  A-->B", "horizontal", "flowchart LR\n  A-->B"],
    ["flowchart BT\n  A-->B", "horizontal", "flowchart RL\n  A-->B"],
    ["flowchart LR\n  A-->B", "horizontal", "flowchart LR\n  A-->B"],
    ["flowchart LR\n  A-->B", "vertical", "flowchart TD\n  A-->B"],
    ["flowchart RL\n  A-->B", "vertical", "flowchart BT\n  A-->B"],
    ["flowchart TB\n  A-->B", "vertical", "flowchart TB\n  A-->B"],
    ["graph\n  A-->B", "horizontal", "graph LR\n  A-->B"],
    ["graph\n  A-->B", "vertical", "graph\n  A-->B"],
  ] as const)("%j → %s", (source, orientation, expected) => {
    expect(orientChart(source, orientation)).toBe(expected);
  });

  it("finds the header past frontmatter and comments", () => {
    const source = "---\ntitle: T\n---\n%% note\nflowchart LR\n  A-->B";
    expect(orientChart(source, "vertical")).toBe(
      "---\ntitle: T\n---\n%% note\nflowchart TD\n  A-->B",
    );
  });

  it("leaves a fixed-direction chart and other diagram types alone", () => {
    const fixed = "%% multica:fixed-direction\nflowchart LR\n  A-->B";
    expect(orientChart(fixed, "vertical")).toBe(fixed);
    const seq = "sequenceDiagram\n  A->>B: hi";
    expect(orientChart(seq, "vertical")).toBe(seq);
  });

  it("rewrites only a top-level state direction", () => {
    expect(orientChart("stateDiagram-v2\n  direction LR\n  [*] --> A", "vertical")).toBe(
      "stateDiagram-v2\n  direction TB\n  [*] --> A",
    );
    const nested = "stateDiagram-v2\n  state X {\n    direction LR\n    a --> b\n  }";
    expect(orientChart(nested, "vertical")).toBe(nested);
  });

  it("inserts a state direction for horizontal when none exists", () => {
    expect(orientChart("stateDiagram-v2\n  [*] --> A", "horizontal")).toBe(
      "stateDiagram-v2\n  direction LR\n  [*] --> A",
    );
  });

  it("replaces only the direction token on a header with statements", () => {
    expect(orientChart("graph LR; A-->B", "vertical")).toBe("graph TD; A-->B");
  });

  it("treats flowchart-elk like flowchart and keeps subgraph direction", () => {
    expect(orientChart("flowchart-elk LR\n  A-->B", "vertical")).toBe("flowchart-elk TD\n  A-->B");
    const sub = "flowchart LR\n  subgraph S\n    direction TB\n    A-->B\n  end";
    expect(orientChart(sub, "vertical")).toBe(
      "flowchart TD\n  subgraph S\n    direction TB\n    A-->B\n  end",
    );
  });
});

describe("pickOrientation", () => {
  it("is vertical below 600 and horizontal above 680", () => {
    expect(pickOrientation(599)).toBe("vertical");
    expect(pickOrientation(681)).toBe("horizontal");
    expect(pickOrientation(599, "horizontal")).toBe("vertical");
    expect(pickOrientation(681, "vertical")).toBe("horizontal");
  });

  it("keeps the previous orientation inside the band", () => {
    expect(pickOrientation(640, "vertical")).toBe("vertical");
    expect(pickOrientation(640, "horizontal")).toBe("horizontal");
  });

  it("splits at 640 with no previous", () => {
    expect(pickOrientation(639)).toBe("vertical");
    expect(pickOrientation(640)).toBe("horizontal");
  });
});

describe("orientChart state-diagram comments", () => {
  it("rewrites a direction that carries an inline comment", () => {
    expect(
      orientChart("stateDiagram-v2\n  direction LR %% author direction\n  [*] --> A", "vertical"),
    ).toBe("stateDiagram-v2\n  direction TB %% author direction\n  [*] --> A");
  });

  it("ignores braces inside comments when tracking block depth", () => {
    expect(
      orientChart("stateDiagram-v2\n  %% { note\n  direction TB\n  [*] --> A", "horizontal"),
    ).toBe("stateDiagram-v2\n  %% { note\n  direction LR\n  [*] --> A");
  });

  it("ignores a closing brace in a trailing comment", () => {
    const source = "stateDiagram-v2\n  state X { %% }\n    direction LR\n  }\n  direction LR\n  [*] --> A";
    expect(orientChart(source, "vertical")).toBe(
      "stateDiagram-v2\n  state X { %% }\n    direction LR\n  }\n  direction TB\n  [*] --> A",
    );
  });

  it("keeps %% inside a quoted state label", () => {
    const source =
      'stateDiagram-v2\n  state "Auth %% complete" as Auth {\n    direction LR\n    A --> B\n  }\n  direction LR';
    expect(orientChart(source, "vertical")).toBe(source.replace("  }\n  direction LR", "  }\n  direction TB"));
  });
});
