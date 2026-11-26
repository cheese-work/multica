"use client";

<<<<<<< HEAD
import { useEffect, useMemo, useRef, useState } from "react";
||||||| 2fcb424b72
import { useEffect, useRef, useState } from "react";
=======
import { useCallback, useEffect, useRef, useState } from "react";
>>>>>>> 35b54a3742bb
import { NodeViewWrapper, NodeViewContent } from "@tiptap/react";
import type { NodeViewProps } from "@tiptap/react";
import { NodeSelection } from "@tiptap/pm/state";
import { CopyCheckIcon } from "@multica/ui/components/common/copy-check-icon";
import { TooltipHint } from "@multica/ui/components/common/tooltip-hint";
import { copyText } from "@multica/ui/lib/clipboard";
import { useDebouncedValue } from "../../common/use-debounced-value";
import { useT } from "../../i18n";
<<<<<<< HEAD
import { MediaSourceProvider, useMediaSource, usePersistedEditorMedia } from "../../media/media-context";
import { markdownMedia } from "../../media/markdown-media";
import { MermaidDiagram } from "../mermaid-diagram";
import { CodeBlockIframe } from "../code-block-iframe";
import { useThemeVersion } from "../hooks/use-theme-version";
import { readEmbedTheme } from "../../common/embed-theme";
import {
  HTML_BLOCK_EMBED_OPTIONS,
  NO_HTML_BLOCK_THEME,
  buildHtmlBlockDocument,
  htmlBlockTheme,
  mentionsThemeTokens,
} from "../utils/html-block-document";
||||||| 2fcb424b72
import { MermaidDiagram } from "../mermaid-diagram";
import { CodeBlockIframe } from "../code-block-iframe";
import { useThemeVersion } from "../hooks/use-theme-version";
import { readEmbedTheme } from "../../common/embed-theme";
import {
  HTML_BLOCK_EMBED_OPTIONS,
  NO_HTML_BLOCK_THEME,
  buildHtmlBlockDocument,
  htmlBlockTheme,
  mentionsThemeTokens,
} from "../utils/html-block-document";
=======
import type { DynamicBlockEditing, DynamicBlockView } from "../dynamic-block";
import { HtmlBlockPreview } from "../html-block-preview";
import { MermaidBlock } from "../mermaid-block";
import { parseFenceTitle, withFenceTitle } from "../utils/fence-title";
import { OPEN_BLOCK_SOURCE_META, isDynamicBlockLanguage } from "./code-block";
>>>>>>> 35b54a3742bb

// Coalesces fast keystrokes before re-rendering live previews.
// `mermaid.initialize()` mutates a process-global config, so back-to-back
// renders during typing can race a concurrent ReadonlyContent render
// (e.g. a comment card) and clobber its theme variables. 200ms keeps the
// "live preview" feel while making concurrent inits unlikely in practice.
// The HTML preview takes the same debounce: every new document reloads its
// iframe.
const PREVIEW_DEBOUNCE_MS = 200;

function CodeBlockView(props: NodeViewProps) {
  return isDynamicBlockLanguage(props.node.attrs.language) ? (
    <DynamicCodeBlockView {...props} />
  ) : (
    <PlainCodeBlockView {...props} />
  );
}

/**
 * ```html and ```mermaid in the editor: the same dynamic block the read-only
 * renderers draw (MUL-9760) — title bar, content height, collapse past 480px —
 * with ProseMirror's editable `<pre>` as its source view and a title that
 * renames in place.
 */
function DynamicCodeBlockView({ node, editor, getPos, selected, updateAttributes }: NodeViewProps) {
  const text = node.textContent;
  // A new, empty block has nothing to preview; it opens where it is written.
  const [view, setView] = useState<DynamicBlockView>(() => (text.trim() ? "preview" : "source"));
  const debounced = useDebouncedValue(text, PREVIEW_DEBOUNCE_MS);
  const meta: string | null = node.attrs.meta ?? null;
  const title = parseFenceTitle(meta);

  // Read by editor listeners, which run outside React's render.
  const viewRef = useRef(view);
  const showView = useCallback((next: DynamicBlockView) => {
    viewRef.current = next;
    setView(next);
  }, []);

  // The source the preview hides must never hold the caret: typing there
  // would change the block unseen. A selection that lands inside it — arrow
  // keys, undo, switching back to the preview — selects the whole block
  // instead, and Enter opens the source (CodeBlockExtension).
  const containSelection = useCallback(() => {
    if (viewRef.current !== "preview" || editor.isDestroyed) return;
    const pos = getPos();
    if (typeof pos !== "number") return;
    const block = editor.state.doc.nodeAt(pos);
    const { selection } = editor.state;
    if (!block || selection instanceof NodeSelection) return;
    if (selection.from > pos && selection.to < pos + block.nodeSize) {
      editor.commands.setNodeSelection(pos);
    }
  }, [editor, getPos]);

  useEffect(() => {
    const onTransaction = ({ transaction }: { transaction: { getMeta: (key: string) => unknown } }) => {
      const pos = getPos();
      if (typeof pos === "number" && transaction.getMeta(OPEN_BLOCK_SOURCE_META) === pos) {
        showView("source");
      }
    };
    // After the dispatch that moved the selection has finished.
    const onSelectionUpdate = () => queueMicrotask(containSelection);
    editor.on("transaction", onTransaction);
    editor.on("selectionUpdate", onSelectionUpdate);
    return () => {
      editor.off("transaction", onTransaction);
      editor.off("selectionUpdate", onSelectionUpdate);
    };
  }, [editor, getPos, showView, containSelection]);

  useEffect(() => {
    if (view === "preview") containSelection();
  }, [view, containSelection]);

  const editing: DynamicBlockEditing = {
    source: (
      <pre spellCheck={false} className="!m-0 !rounded-none">
        {/* @ts-expect-error -- NodeViewContent supports as="code" at runtime */}
        <NodeViewContent as="code" />
      </pre>
    ),
    view,
    onViewChange: showView,
    onTitleChange: editor.isEditable
      ? (next) => updateAttributes({ meta: withFenceTitle(meta, next) })
      : undefined,
    selected,
  };

  return (
    // Block, against the editor's inline default for NodeView wrappers; the
    // margin matches the read-only block's shell, so the doc does not shift
    // when the live editor replaces the read-only render.
    <NodeViewWrapper className="my-5 !block" data-dynamic-code-block="">
      {node.attrs.language === "mermaid" ? (
        <MermaidBlock chart={debounced} title={title} editing={editing} />
      ) : (
        <HtmlBlockPreview html={debounced} title={title} editing={editing} />
      )}
    </NodeViewWrapper>
  );
}

function PlainCodeBlockView({ node }: NodeViewProps) {
  const { t } = useT("editor");
  const [copied, setCopied] = useState(false);
  const language = node.attrs.language || "";
<<<<<<< HEAD
  const isMermaid = language === "mermaid";
  const isHtml = language === "html";
  const showHtmlPreview = isHtml && view === "preview";
  const chart = node.textContent;
  const debouncedChart = useDebouncedValue(
    isMermaid ? chart : "",
    PREVIEW_DEBOUNCE_MS,
  );
  const debouncedHtml = useDebouncedValue(
    isHtml ? chart : "",
    PREVIEW_DEBOUNCE_MS,
    showHtmlPreview || fullscreen,
  );
  const source = useMediaSource();
  const persistedMarkdown = usePersistedEditorMedia();
  const persistedBlocks = useMemo(() => markdownMedia(persistedMarkdown ?? ""), [persistedMarkdown]);
  const previewContent = isHtml ? debouncedHtml : debouncedChart;
  const persisted = chart === previewContent && persistedBlocks.some((block) => block.kind === language && block.content === chart);
  // The same document a read-only ```html block gets, so the embed tokens
  // resolve while the block is being edited too. HTML that names no token
  // gets the same document under every theme: a new srcDoc reloads the frame
  // and loses whatever was typed or toggled in it.
  const previewRef = useRef<HTMLDivElement>(null);
  const themeVersion = useThemeVersion();
  const [srcDoc, setSrcDoc] = useState("");
  useEffect(() => {
    const host = previewRef.current;
    if (!host || !debouncedHtml) {
      setSrcDoc("");
      return;
    }
    const theme = mentionsThemeTokens(debouncedHtml)
      ? htmlBlockTheme(readEmbedTheme(host, HTML_BLOCK_EMBED_OPTIONS))
      : NO_HTML_BLOCK_THEME;
    setSrcDoc(buildHtmlBlockDocument(debouncedHtml, theme));
  }, [debouncedHtml, themeVersion]);
||||||| 2fcb424b72
  const isMermaid = language === "mermaid";
  const isHtml = language === "html";
  const showHtmlPreview = isHtml && view === "preview";
  const chart = node.textContent;
  const debouncedChart = useDebouncedValue(
    isMermaid ? chart : "",
    PREVIEW_DEBOUNCE_MS,
  );
  const debouncedHtml = useDebouncedValue(
    isHtml ? chart : "",
    PREVIEW_DEBOUNCE_MS,
    showHtmlPreview || fullscreen,
  );
  // The same document a read-only ```html block gets, so the embed tokens
  // resolve while the block is being edited too. HTML that names no token
  // gets the same document under every theme: a new srcDoc reloads the frame
  // and loses whatever was typed or toggled in it.
  const previewRef = useRef<HTMLDivElement>(null);
  const themeVersion = useThemeVersion();
  const [srcDoc, setSrcDoc] = useState("");
  useEffect(() => {
    const host = previewRef.current;
    if (!host || !debouncedHtml) {
      setSrcDoc("");
      return;
    }
    const theme = mentionsThemeTokens(debouncedHtml)
      ? htmlBlockTheme(readEmbedTheme(host, HTML_BLOCK_EMBED_OPTIONS))
      : NO_HTML_BLOCK_THEME;
    setSrcDoc(buildHtmlBlockDocument(debouncedHtml, theme));
  }, [debouncedHtml, themeVersion]);
=======
>>>>>>> 35b54a3742bb

  const handleCopy = async () => {
    const text = node.textContent;
    if (!text) return;
    if (await copyText(text)) {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  };

  return (
    <MediaSourceProvider source={persisted ? source : null}>
    <NodeViewWrapper className="code-block-wrapper group/code relative my-3">
<<<<<<< HEAD
      {isMermaid && debouncedChart.trim() && (
        <div
          contentEditable={false}
          className="mermaid-diagram-preview mb-1"
        >
          <MermaidDiagram chart={debouncedChart} />
        </div>
      )}
      {isHtml && (
        // CSS-hidden when toggled off so the `<pre>` below stays mounted —
        // unmounting either side would either lose ProseMirror bindings
        // (source) or thrash iframe.srcDoc (preview).
        <div
          ref={previewRef}
          contentEditable={false}
          className={showHtmlPreview ? "mb-1" : undefined}
          hidden={!showHtmlPreview && !fullscreen}
        >
          <HtmlPreviewSurface
            mediaContent={debouncedHtml}
            open={fullscreen}
            inlineHeight={showHtmlPreview ? HTML_PREVIEW_HEIGHT_PX : 0}
            onOpenChange={setFullscreen}
            onReload={reload}
            title={t(($) => $.code_block.html_preview)}
          >
            <CodeBlockIframe
              key={reloadKey}
              html={srcDoc}
              title={t(($) => $.code_block.html_preview)}
              heightClassName={fullscreen ? "h-full" : "h-[480px]"}
              className={fullscreen ? "rounded-none border-0" : undefined}
            />
          </HtmlPreviewSurface>
        </div>
      )}
||||||| 2fcb424b72
      {isMermaid && debouncedChart.trim() && (
        <div
          contentEditable={false}
          className="mermaid-diagram-preview mb-1"
        >
          <MermaidDiagram chart={debouncedChart} />
        </div>
      )}
      {isHtml && (
        // CSS-hidden when toggled off so the `<pre>` below stays mounted —
        // unmounting either side would either lose ProseMirror bindings
        // (source) or thrash iframe.srcDoc (preview).
        <div
          ref={previewRef}
          contentEditable={false}
          className={showHtmlPreview ? "mb-1" : undefined}
          hidden={!showHtmlPreview && !fullscreen}
        >
          <HtmlPreviewSurface
            open={fullscreen}
            inlineHeight={showHtmlPreview ? HTML_PREVIEW_HEIGHT_PX : 0}
            onOpenChange={setFullscreen}
            onReload={reload}
            title={t(($) => $.code_block.html_preview)}
          >
            <CodeBlockIframe
              key={reloadKey}
              html={srcDoc}
              title={t(($) => $.code_block.html_preview)}
              heightClassName={fullscreen ? "h-full" : "h-[480px]"}
              className={fullscreen ? "rounded-none border-0" : undefined}
            />
          </HtmlPreviewSurface>
        </div>
      )}
=======
>>>>>>> 35b54a3742bb
      <div
        contentEditable={false}
        className="code-block-header absolute top-0 right-0 z-10 flex items-center gap-1.5 px-2 py-1.5 opacity-0 transition-opacity group-hover/code:opacity-100 focus-within:opacity-100 [@media(hover:none)]:opacity-100"
      >
        {language && (
          <span className="text-caption text-muted-foreground select-none">
            {language}
          </span>
        )}
        <TooltipHint label={t(($) => $.code_block.copy_code)}>
          <button
            type="button"
            onClick={handleCopy}
            className="flex h-6 w-6 items-center justify-center rounded-xs text-muted-foreground hover:bg-muted hover:text-foreground transition-colors"
            aria-label={t(($) => $.code_block.copy_code)}
          >
            <CopyCheckIcon copied={copied} />
          </button>
        </TooltipHint>
      </div>
      <pre spellCheck={false}>
        {/* @ts-expect-error -- NodeViewContent supports as="code" at runtime */}
        <NodeViewContent as="code" />
      </pre>
    </NodeViewWrapper>
    </MediaSourceProvider>
  );
}

export { CodeBlockView };
