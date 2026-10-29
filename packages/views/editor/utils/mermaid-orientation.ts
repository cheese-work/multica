export type Orientation = "horizontal" | "vertical";

const VERTICAL_BELOW_PX = 600;
const HORIZONTAL_ABOVE_PX = 680;
const SPLIT_PX = 640;

const FIXED_DIRECTION_MARKER = "%% multica:fixed-direction";

const FLOWCHART_HEADER = /^(\s*)(flowchart-elk|flowchart|graph)(?:[ \t]+(TD|TB|BT|LR|RL))?(?=[\s;]|$)(.*)$/;
const STATE_HEADER = /^\s*stateDiagram(?:-v2)?\s*$/;
const STATE_DIRECTION = /^(\s*direction\s+)(TB|BT|LR|RL)(\s*(?:%%.*)?)$/;

/**
 * Wide below this width a diagram reads better left-to-right; narrow, top-down.
 * The band between the two bounds keeps whatever was picked before, so a
 * container resizing around one value does not re-render on every pixel.
 */
export function pickOrientation(width: number, previous?: Orientation): Orientation {
  if (width < VERTICAL_BELOW_PX) return "vertical";
  if (width > HORIZONTAL_ABOVE_PX) return "horizontal";
  return previous ?? (width < SPLIT_PX ? "vertical" : "horizontal");
}

function flowDirection(current: string | undefined, to: Orientation): string | undefined {
  if (to === "horizontal") {
    if (current === "LR" || current === "RL") return current;
    return current === "BT" ? "RL" : "LR";
  }
  if (current === "LR") return "TD";
  if (current === "RL") return "BT";
  return current;
}

function stateDirection(current: string, to: Orientation): string {
  if (to === "horizontal") return current === "BT" ? "RL" : current === "TB" ? "LR" : current;
  return current === "RL" ? "BT" : current === "LR" ? "TB" : current;
}

/** Net `{` minus `}` on a line, ignoring double-quoted labels and a trailing `%%` comment. */
function braceDelta(line: string): number {
  let delta = 0;
  let quoted = false;
  for (let i = 0; i < line.length; i++) {
    const char = line[i];
    if (char === '"') quoted = !quoted;
    else if (quoted) continue;
    else if (line.startsWith("%%", i)) break;
    else if (char === "{") delta++;
    else if (char === "}") delta--;
  }
  return delta;
}

/** Index of the first line that is neither frontmatter, blank nor a `%%` comment. */
function headerIndex(lines: string[]): number {
  let i = 0;
  while (i < lines.length && lines[i]!.trim() === "") i++;
  if (lines[i]?.trim() === "---") {
    const close = lines.findIndex((line, index) => index > i && line.trim() === "---");
    if (close === -1) return -1;
    i = close + 1;
  }
  while (i < lines.length && (lines[i]!.trim() === "" || lines[i]!.trim().startsWith("%%"))) i++;
  return i < lines.length ? i : -1;
}

/**
 * The chart redrawn in the given orientation. Only flowchart/graph headers and
 * a top-level stateDiagram `direction` change; every other diagram type, and a
 * chart carrying the `%% multica:fixed-direction` marker, comes back as written.
 */
export function orientChart(source: string, orientation: Orientation): string {
  if (source.includes(FIXED_DIRECTION_MARKER)) return source;
  const lines = source.split(/\r?\n/);
  const header = headerIndex(lines);
  if (header === -1) return source;
  const line = lines[header]!;

  const flow = FLOWCHART_HEADER.exec(line);
  if (flow) {
    const [, indent, keyword, current, rest] = flow;
    const next = flowDirection(current, orientation);
    if (next === current) return source;
    lines[header] = `${indent}${keyword}${next ? ` ${next}` : ""}${rest}`;
    return lines.join("\n");
  }

  if (!STATE_HEADER.test(line)) return source;
  let depth = 0;
  for (let i = header + 1; i < lines.length; i++) {
    const text = lines[i]!;
    const direction = depth === 0 ? STATE_DIRECTION.exec(text) : null;
    if (direction) {
      const next = stateDirection(direction[2]!, orientation);
      if (next === direction[2]) return source;
      lines[i] = `${direction[1]}${next}${direction[3]}`;
      return lines.join("\n");
    }
    depth += braceDelta(text);
  }
  if (orientation === "vertical") return source;
  lines.splice(header + 1, 0, "  direction LR");
  return lines.join("\n");
}
