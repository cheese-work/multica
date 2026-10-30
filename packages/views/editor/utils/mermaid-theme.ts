/**
 * Mermaid theme built from the app's tokens (CHE-2525): a full variable set,
 * a small themeCSS layer, and the rule that flips a too-wide diagram top-down.
 * Pure, so every part is unit-tested without a DOM.
 */

/** Label size Mermaid draws at; the unit the minimum below is measured against. */
export const MERMAID_FONT_PX = 15;

/** Smallest label size a diagram may be scaled down to before it is stacked. */
export const MERMAID_MIN_TEXT_PX = 12;

export interface MermaidThemeColors {
  background: string;
  card: string;
  muted: string;
  border: string;
  primary: string;
  foreground: string;
  mutedForeground: string;
}

/**
 * True when fitting a diagram of `naturalWidth` into `availableWidth` would
 * scale its labels below MERMAID_MIN_TEXT_PX. An unmeasured column (0) never
 * stacks. Widths in the same unit; only their ratio matters.
 */
export function shouldStackVertically(naturalWidth: number, availableWidth: number): boolean {
  if (availableWidth <= 0 || naturalWidth <= 0) return false;
  return (MERMAID_FONT_PX * availableWidth) / naturalWidth < MERMAID_MIN_TEXT_PX;
}

export function buildMermaidThemeVariables(c: MermaidThemeColors, dark: boolean) {
  return {
    darkMode: dark,
    fontFamily: "inherit",
    fontSize: `${MERMAID_FONT_PX}px`,
    background: c.background,
    primaryColor: c.card,
    primaryBorderColor: c.border,
    primaryTextColor: c.foreground,
    secondaryColor: c.muted,
    tertiaryColor: c.muted,
    textColor: c.foreground,
    lineColor: c.mutedForeground,
    mainBkg: c.card,
    nodeBorder: c.border,
    nodeTextColor: c.foreground,
    clusterBkg: c.muted,
    clusterBorder: c.border,
    titleColor: c.foreground,
    edgeLabelBackground: c.card,
    // sequence
    actorBkg: c.card,
    actorBorder: c.border,
    actorTextColor: c.foreground,
    actorLineColor: c.mutedForeground,
    signalColor: c.mutedForeground,
    signalTextColor: c.foreground,
    labelBoxBkgColor: c.card,
    labelBoxBorderColor: c.border,
    labelTextColor: c.foreground,
    loopTextColor: c.foreground,
    noteBkgColor: c.muted,
    noteBorderColor: c.border,
    noteTextColor: c.foreground,
    activationBkgColor: c.muted,
    activationBorderColor: c.border,
    // state and class
    stateBkg: c.card,
    labelBackgroundColor: c.card,
    compositeBackground: c.muted,
    transitionColor: c.mutedForeground,
    transitionLabelColor: c.foreground,
    stateLabelColor: c.foreground,
    classText: c.foreground,
  };
}

/**
 * Drawn inside the diagram's sandboxed document, where the host's tokens are
 * re-declared (getSandboxCssVariables), so `var(--success)` resolves. An
 * author's own `classDef` outranks these; their `themeCSS` replaces them.
 */
/**
 * Tighter than Mermaid's defaults: a 4-actor sequence is 916px wide by default
 * and shrinks to ~9px text in a column; this keeps it near 614px.
 */
export const MERMAID_SEQUENCE_CONFIG = { width: 110, actorMargin: 24, boxMargin: 8, messageMargin: 28, wrap: true };

export const MERMAID_THEME_CSS = `
.node rect, .cluster rect, .actor, .labelBox, .note { rx: 10px; ry: 10px; }
.node rect, .node polygon, .node circle, .node path, .actor, .note { filter: drop-shadow(0 1px 2px rgb(0 0 0 / 0.18)); }
.flowchart-link, .edgePath .path, .messageLine0, .messageLine1, .transition, .relation { stroke-width: 1.6px; }
.nodeLabel, .edgeLabel, .label, .actor text, .noteText { font-weight: 500; }
.messageText { font-size: 15px !important; font-weight: 500 !important; }
.node.done rect, .node.done polygon, .node.done circle, .node.done path { fill: color-mix(in srgb, var(--success) 18%, transparent); stroke: var(--success); }
.node.warn rect, .node.warn polygon, .node.warn circle, .node.warn path { fill: color-mix(in srgb, var(--warning) 18%, transparent); stroke: var(--warning); }
`.trim();
