/**
 * Split Markdown into native prose, React code blocks, and interactive images.
 * Use lexer token boundaries so fenced/indented code and paragraph images are
 * handled without fragile regex reconstruction. Preserve prose through token.raw.
 *
 * A `<details>` block is one `details` segment. `preprocessMobileMarkdown`
 * encodes it as a single HTML-comment marker (`DETAILS_MARKER_*`); lexing sees
 * one html token, and `body` holds the inner segments.
 *
 * Only top-level code is extracted. Nested list/quote code stays in the native
 * renderer to preserve its surrounding Markdown structure.
 */
import { marked, type Tokens } from "marked";

export type MarkdownSegment =
  | { type: "prose"; content: string }
  | { type: "code"; lang: string | undefined; code: string }
  | { type: "image"; uri: string; alt: string }
  | DetailsSegment;

export interface DetailsSegment {
  type: "details";
  summary: string;
  body: MarkdownSegment[];
}

export const DETAILS_MARKER_PREFIX = "<!--multica:details:";
export const DETAILS_MARKER_SUFFIX = "-->";

export function encodeDetailsMarker(summary: string, body: string): string {
  const payload = encodeURIComponent(JSON.stringify({ summary, body }));
  return `${DETAILS_MARKER_PREFIX}${payload}${DETAILS_MARKER_SUFFIX}`;
}

function decodeDetailsMarker(raw: string): { summary: string; body: string } | null {
  const text = raw.trim();
  if (!text.startsWith(DETAILS_MARKER_PREFIX) || !text.endsWith(DETAILS_MARKER_SUFFIX)) {
    return null;
  }
  try {
    const parsed: unknown = JSON.parse(
      decodeURIComponent(text.slice(DETAILS_MARKER_PREFIX.length, -DETAILS_MARKER_SUFFIX.length)),
    );
    const { summary, body } = parsed as { summary?: unknown; body?: unknown };
    return typeof summary === "string" && typeof body === "string" ? { summary, body } : null;
  } catch {
    return null;
  }
}

export function splitMarkdown(input: string): MarkdownSegment[] {
  if (!input) return [];

  const tokens = marked.lexer(input);
  const out: MarkdownSegment[] = [];
  let proseBuffer = "";

  const flushProse = () => {
    const trimmed = proseBuffer.replace(/^\s+|\s+$/g, "");
    if (trimmed.length > 0) {
      out.push({ type: "prose", content: trimmed });
    }
    proseBuffer = "";
  };

  for (const token of tokens) {
    if (token.type === "code") {
      flushProse();
      const t = token as Tokens.Code;
      out.push({
        type: "code",
        lang: t.lang ? t.lang : undefined,
        code: t.text,
      });
      continue;
    }

    if (token.type === "html") {
      const details = decodeDetailsMarker((token as Tokens.HTML).raw);
      if (details) {
        flushProse();
        out.push({
          type: "details",
          summary: details.summary,
          body: splitMarkdown(details.body),
        });
        continue;
      }
    }

    if (token.type === "paragraph") {
      const para = token as Tokens.Paragraph;
      const inline = para.tokens ?? [];
      const hasImage = inline.some((t) => t.type === "image");

      if (!hasImage) {
        proseBuffer += para.raw;
        continue;
      }

      // Split paragraph at each image. Text fragments before/after each
      // image rejoin the prose buffer as their own short paragraph.
      let textBuffer = "";
      const flushText = () => {
        const trimmed = textBuffer.trim();
        if (trimmed.length > 0) {
          // Append as its own paragraph so enriched treats consecutive
          // fragments as separate blocks rather than running them together.
          proseBuffer += trimmed + "\n\n";
        }
        textBuffer = "";
      };

      for (const t of inline) {
        if (t.type === "image") {
          flushText();
          flushProse();
          const img = t as Tokens.Image;
          out.push({
            type: "image",
            uri: img.href,
            alt: img.text ?? "",
          });
        } else {
          textBuffer += (t as { raw?: string }).raw ?? "";
        }
      }
      flushText();
      continue;
    }

    // Every other top-level block (heading, list, blockquote, table,
    // hr, html, space, def, …) goes to prose verbatim via token.raw.
    proseBuffer += (token as { raw?: string }).raw ?? "";
  }

  flushProse();
  return out;
}
