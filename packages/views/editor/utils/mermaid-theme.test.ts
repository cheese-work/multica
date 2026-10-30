// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  MERMAID_FONT_PX,
  MERMAID_MIN_TEXT_PX,
  buildMermaidThemeCss,
  buildMermaidThemeVariables,
  shouldStackVertically,
} from "./mermaid-theme";

const COLORS = {
  background: "rgb(1, 1, 1)",
  card: "rgb(2, 2, 2)",
  muted: "rgb(3, 3, 3)",
  border: "rgb(4, 4, 4)",
  primary: "rgb(5, 5, 5)",
  foreground: "rgb(6, 6, 6)",
  mutedForeground: "rgb(7, 7, 7)",
  success: "rgb(8, 8, 8)",
  warning: "rgb(9, 9, 9)",
  destructive: "rgb(10, 10, 10)",
  chart: ["rgb(20, 0, 0)", "rgb(0, 40, 0)", "rgb(0, 0, 60)", "rgb(80, 80, 0)", "rgb(255, 255, 255)"],
};

describe("shouldStackVertically", () => {
  // The scaled label is MERMAID_FONT_PX * available / natural; stacking starts below MIN.
  const natural = 1000;
  const exactlyMin = (natural * MERMAID_MIN_TEXT_PX) / MERMAID_FONT_PX;

  it("keeps a diagram whose scaled text is exactly the minimum", () => {
    expect(shouldStackVertically(natural, exactlyMin)).toBe(false);
  });

  it("stacks one pixel of column width below the minimum", () => {
    expect(shouldStackVertically(natural, exactlyMin - 1)).toBe(true);
  });

  it("keeps a diagram that fits at natural size", () => {
    expect(shouldStackVertically(400, 800)).toBe(false);
  });

  it("ignores an unmeasured column", () => {
    expect(shouldStackVertically(1000, 0)).toBe(false);
  });
});

describe("buildMermaidThemeVariables", () => {
  const light = buildMermaidThemeVariables(COLORS, false);
  const dark = buildMermaidThemeVariables(COLORS, true);

  it("maps app tokens onto the node, cluster, edge and sequence slots", () => {
    expect(light).toMatchObject({
      mainBkg: COLORS.card,
      nodeBorder: COLORS.border,
      clusterBkg: COLORS.muted,
      edgeLabelBackground: COLORS.card,
      lineColor: COLORS.mutedForeground,
      actorBkg: COLORS.card,
      noteBkgColor: COLORS.muted,
      activationBkgColor: COLORS.muted,
      fontSize: `${MERMAID_FONT_PX}px`,
    });
  });

  it("tells Mermaid which scheme it draws for", () => {
    expect(light.darkMode).toBe(false);
    expect(dark.darkMode).toBe(true);
  });

  it("gives xychart-beta every color it would otherwise take from its light default", () => {
    // xychart-beta merges onto the light default theme, not the base theme's
    // derived values: any color left out stays near-white on a light grey plot.
    expect(dark.xyChart).toMatchObject({
      backgroundColor: COLORS.background,
      titleColor: COLORS.foreground,
      dataLabelColor: COLORS.foreground,
      xAxisTitleColor: COLORS.foreground,
      xAxisLabelColor: COLORS.foreground,
      xAxisTickColor: COLORS.foreground,
      xAxisLineColor: COLORS.foreground,
      yAxisTitleColor: COLORS.foreground,
      yAxisLabelColor: COLORS.foreground,
      yAxisTickColor: COLORS.foreground,
      yAxisLineColor: COLORS.foreground,
    });
  });

  it("writes the xychart palette as hex, because Mermaid splits it on commas", () => {
    expect(light.xyChart.plotColorPalette).toBe("#140000,#002800,#00003c,#505000,#ffffff");
  });

  it("colors timeline sections and pie slices from the chart tokens, with readable labels", () => {
    expect(light).toMatchObject({
      cScale0: COLORS.chart[0],
      cScale4: COLORS.chart[4],
      pie1: COLORS.chart[0],
      pie5: COLORS.chart[4],
      cScaleLabel0: "rgb(255, 255, 255)",
      cScaleLabel4: "rgb(17, 24, 39)",
    });
  });

  it("draws the radar grid in the muted foreground", () => {
    expect(light.radar.graticuleColor).toBe(COLORS.mutedForeground);
  });
});

describe("buildMermaidThemeCss", () => {
  const css = buildMermaidThemeCss(COLORS);

  it("rounds nodes, softens them with a shadow and thickens edges", () => {
    expect(css).toMatch(/\.node rect[^}]*rx:\s*10px/);
    expect(css).toContain("drop-shadow");
    expect(css).toContain("stroke-width: 1.6px");
  });

  it("outranks the inline weight Mermaid writes on sequence text", () => {
    expect(css).toMatch(/\.messageText[^}]*font-weight:\s*500\s*!important/);
  });

  it("styles the done and warn classes authors can assign", () => {
    expect(css).toContain(".done");
    expect(css).toContain(".warn");
  });

  // An exported SVG/PNG has no host page, so a var() would resolve to nothing.
  it("paints done and warn with concrete colors, never var()", () => {
    expect(css).not.toContain("var(");
    expect(css).toMatch(/\.node\.done[^}]*stroke:\s*rgb\(8, 8, 8\)/);
    expect(css).toMatch(/\.node\.warn[^}]*stroke:\s*rgb\(9, 9, 9\)/);
    expect(css).toMatch(/\.node\.done[^}]*fill:\s*rgba\(8, 8, 8, 0\.18\)/);
  });
});
