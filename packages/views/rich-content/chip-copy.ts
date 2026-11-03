/**
 * Copy handler for inline chips (issue, comment, project): a selection that
 * touches a chip copies the chip's key, not its rendered label (CHE-3371).
 *
 * Chips opt in with `data-copy-token="<key>"` on their root. The anchor around
 * a chip supplies the link in the text/html channel.
 */

const CHIP = "[data-copy-token]";
const DOUBLE = new Set(["P", "H1", "H2", "H3", "H4", "H5", "H6", "PRE", "BLOCKQUOTE", "UL", "OL", "TABLE"]);
const SINGLE = new Set(["LI", "TR", "DIV"]);

function elementOf(node: Node): Element | null {
  return node instanceof Element ? node : node.parentElement;
}

function chipReplacement(live: Element, clone: Element): [Element, Node] {
  const token = live.getAttribute("data-copy-token") ?? "";
  const href = live.closest("a")?.href;
  const target = clone.closest("a") ?? clone;
  if (!href) return [target, document.createTextNode(token)];
  const a = document.createElement("a");
  a.href = href;
  a.textContent = token;
  return [target, a];
}

// Blocks are separated from their siblings, never padded at the ends, so the
// copied text keeps exactly the newlines the selection held.
function separator(node: Node): string {
  if (!(node instanceof Element)) return "";
  if (DOUBLE.has(node.tagName)) return "\n\n";
  return SINGLE.has(node.tagName) ? "\n" : "";
}

function plainText(node: Node): string {
  if (node.nodeType === Node.TEXT_NODE) return node.textContent ?? "";
  if (node instanceof Element && node.tagName === "BR") return "\n";
  let out = "";
  node.childNodes.forEach((child, i) => {
    const prev = node.childNodes[i - 1];
    if (prev) out += [separator(prev), separator(child)].sort((x, y) => y.length - x.length)[0];
    out += plainText(child);
  });
  return out;
}

/**
 * Writes chip keys to the clipboard event. Returns false, leaving the default
 * copy untouched, when the selection holds no chip.
 */
export function copyChipKeys(e: ClipboardEvent): boolean {
  const range = window.getSelection()?.rangeCount
    ? window.getSelection()!.getRangeAt(0)
    : null;
  if (!range || range.collapsed || !e.clipboardData) return false;

  const out = document.createElement("div");
  const within = elementOf(range.commonAncestorContainer)?.closest(CHIP);
  if (within) {
    const [, node] = chipReplacement(within, within);
    out.append(node);
  } else {
    const root = elementOf(range.commonAncestorContainer);
    const live = [...(root?.querySelectorAll(CHIP) ?? [])].filter((c) => range.intersectsNode(c));
    if (!live.length) return false;
    out.append(range.cloneContents());
    const clones = [...out.querySelectorAll(CHIP)];
    // A mismatch means the DOM moved under us; the default copy is safer.
    if (clones.length !== live.length) return false;
    clones.forEach((clone, i) => {
      const [target, node] = chipReplacement(live[i]!, clone);
      target.replaceWith(node);
    });
  }

  e.clipboardData.setData("text/plain", plainText(out));
  e.clipboardData.setData("text/html", out.innerHTML);
  e.preventDefault();
  return true;
}
