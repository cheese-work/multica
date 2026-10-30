"use client";

/**
 * MermaidViewer — full-screen diagram viewer.
 *
 * Built on the shared `Dialog` (Base UI) rather than a bespoke portal, so it
 * inherits the behaviors the old Mermaid lightbox was missing: a real backdrop,
 * focus trap, background scroll lock, focus restore, and Escape-to-close.
 *
 * The diagram stays inside an `sandbox=""` iframe — the isolation boundary is
 * not relaxed to buy interactivity. Instead the iframe is `pointer-events:
 * none` and pan/zoom is applied from this document as a transform on the
 * wrapper (see `useDiagramCanvas`). A side effect worth keeping: because the
 * iframe never takes focus, key events cannot get stranded in a document the
 * dialog can't hear, which is why Escape used to stop working after a click.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Check,
  Code as CodeIcon,
  Copy,
  Download,
  Image as ImageIcon,
  X,
} from "lucide-react";
import { cn } from "@multica/ui/lib/utils";
import { copyText } from "@multica/ui/lib/clipboard";
import { Dialog, DialogContent, DialogTitle } from "@multica/ui/components/ui/dialog";
import { TooltipHint } from "@multica/ui/components/common/tooltip-hint";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import { useT } from "../i18n";
import { CodeBlockStatic } from "./code-block-static";
import { useZoomCanvas } from "./hooks/use-zoom-canvas";
import { ZoomCanvas, ZoomControls } from "./zoom-canvas";
import {
  buildExportSvg,
  diagramFilenameStem,
  downloadBlob,
  renderSvgToPngBlob,
} from "./utils/mermaid-export";
import type { Size } from "./utils/zoom-transform";
import { MediaLinkAction, ViewerActions } from "../media/media-context";

const COPY_FEEDBACK_MS = 2000;

// iOS Safari zooms the page on a pinch via `gesturestart`; React has no
// synthetic event for it, so cancel it natively. Passive false, or
// preventDefault is ignored. Ref callbacks may return a cleanup (React 19).
function containGesture(node: HTMLElement | null) {
  if (!node) return;
  const cancel = (event: Event) => event.preventDefault();
  node.addEventListener("gesturestart", cancel, { passive: false });
  return () => node.removeEventListener("gesturestart", cancel);
}

interface MermaidViewerContentProps {
  /** Mermaid source, for the source view and `.mmd` export. */
  chart: string;
  /** Rendered SVG markup, for export. */
  svg: string | null;
  /** Sandboxed document that draws the diagram at natural size. */
  viewerDocument: string | null;
  /** Natural size of the diagram. */
  layout: Size;
  /** Colors resolved from the host theme, so exports match what is on screen. */
  exportBackground: string;
  exportFontFamily: string;
  /** Reports the canvas width, so the caller can draw the diagram to fit it. */
  onViewportWidth?: (width: number) => void;
}

export interface MermaidViewerProps extends MermaidViewerContentProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Focus returns here on close — the button that opened the viewer. */
  finalFocusRef?: React.RefObject<HTMLElement | null>;
}

// Viewer-only chrome (source toggle, copy, close). The zoom buttons come from
// the shared <ZoomControls>.
function ToolbarButton({
  onClick,
  disabled,
  label,
  children,
}: {
  onClick: () => void;
  disabled?: boolean;
  label: string;
  children: React.ReactNode;
}) {
  return (
    <TooltipHint label={label} side="bottom">
      <button
        type="button"
        onClick={onClick}
        disabled={disabled}
        aria-label={label}
        className="flex size-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-secondary hover:text-foreground disabled:pointer-events-none disabled:opacity-40"
      >
        {children}
      </button>
    </TooltipHint>
  );
}

export function MermaidViewer({
  open,
  onOpenChange,
  finalFocusRef,
  ...content
}: MermaidViewerProps) {
  const { t } = useT("editor");
  const canvasRef = useRef<HTMLDivElement>(null);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        ref={containGesture}
        showCloseButton={false}
        initialFocus={canvasRef}
        finalFocus={finalFocusRef}
        // Dynamic viewport units: on mobile Safari and in narrow Electron
        // panes `vh` includes retracted browser chrome, which would push the
        // toolbar off-screen — the exact "can't find the close button"
        // failure this viewer exists to remove.
        // `touch-none` + `overscroll-contain`: a finger anywhere on the dialog
        // (header included) stays here and never scrolls or zooms the page
        // behind. Below md it fills the screen, so no backdrop strip is left
        // to touch; the `!` overrides beat the base popup's centring classes.
        className="touch-none overscroll-contain !max-w-[calc(100vw-2rem)] !h-[min(90dvh,calc(100dvh-2rem))] !w-[calc(100vw-2rem)] flex flex-col gap-0 overflow-hidden p-0 max-md:!inset-0 max-md:!h-dvh max-md:!max-h-none max-md:!w-screen max-md:!max-w-none max-md:!translate-x-0 max-md:!translate-y-0 max-md:!rounded-none max-md:!ring-0 xl:!max-w-[80rem]"
        aria-label={t(($) => $.mermaid.viewer_title)}
      >
        {/* Body state (zoom, pan, source toggle) deliberately lives below the
            portal boundary so it is destroyed on close. Each open is a fresh
            read that re-fits, rather than restoring a stale zoom from the last
            time this diagram happened to be opened. */}
        <MermaidViewerContent
          {...content}
          canvasRef={canvasRef}
          onClose={() => onOpenChange(false)}
        />
      </DialogContent>
    </Dialog>
  );
}

function MermaidViewerContent({
  chart,
  svg,
  viewerDocument,
  layout,
  exportBackground,
  exportFontFamily,
  onViewportWidth,
  canvasRef,
  onClose,
}: MermaidViewerContentProps & {
  canvasRef: React.RefObject<HTMLDivElement | null>;
  onClose?: () => void;
}) {
  const { t } = useT("editor");
  const [showSource, setShowSource] = useState(false);
  const [copied, setCopied] = useState(false);
  const canvas = useZoomCanvas({ content: layout });

  useEffect(() => {
    const node = canvasRef.current;
    if (!onViewportWidth || !node) return;
    const report = () => onViewportWidth(node.getBoundingClientRect().width);
    report();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(report);
    observer.observe(node);
    return () => observer.disconnect();
  }, [canvasRef, onViewportWidth, showSource]);

  const handleCopySource = useCallback(async () => {
    if (await copyText(chart)) {
      setCopied(true);
      setTimeout(() => setCopied(false), COPY_FEEDBACK_MS);
    }
  }, [chart]);

  const standaloneSvg = useMemo(() => {
    if (!svg) return null;
    return buildExportSvg(svg, {
      background: exportBackground,
      fontFamily: exportFontFamily,
      width: layout.width,
      height: layout.height,
    });
  }, [svg, layout, exportBackground, exportFontFamily]);

  const filenameStem = useMemo(() => diagramFilenameStem(chart), [chart]);

  const handleDownloadSource = useCallback(() => {
    downloadBlob(
      new Blob([chart], { type: "text/vnd.mermaid;charset=utf-8" }),
      `${filenameStem}.mmd`,
    );
  }, [chart, filenameStem]);

  const handleDownloadSvg = useCallback(() => {
    if (!standaloneSvg) return;
    downloadBlob(
      new Blob([standaloneSvg], { type: "image/svg+xml;charset=utf-8" }),
      `${filenameStem}.svg`,
    );
  }, [standaloneSvg, filenameStem]);

  const handleDownloadPng = useCallback(async () => {
    if (!standaloneSvg) return;
    try {
      const blob = await renderSvgToPngBlob(standaloneSvg, layout);
      downloadBlob(blob, `${filenameStem}.png`);
    } catch {
      // Rasterization is best-effort: `.svg` and `.mmd` remain available, and
      // failing loudly here would put an error dialog over the diagram the
      // user is reading.
    }
  }, [standaloneSvg, layout, filenameStem]);

  const canExportImage = standaloneSvg !== null;

  return (
    <>
      {/* Sibling of the scrolling canvas, never inside it: the toolbar must
          stay put no matter how far the diagram is panned or zoomed. */}
      <header data-testid="media-toolbar" className="flex shrink-0 flex-wrap items-center gap-1 border-b border-border bg-muted/30 px-3 py-2 max-md:[&_button]:min-h-10 max-md:[&_button]:min-w-10">
          {onClose ? <DialogTitle className="mr-1 w-full truncate text-body font-medium md:w-auto">{t(($) => $.mermaid.viewer_title)}</DialogTitle> : <h1 className="mr-1 w-full truncate text-body font-medium md:w-auto">{t(($) => $.mermaid.viewer_title)}</h1>}

          <ZoomControls canvas={canvas} disabled={showSource} />

          <ViewerActions>
            <ToolbarButton
              onClick={() => setShowSource((value) => !value)}
              label={
                showSource
                  ? t(($) => $.mermaid.show_diagram)
                  : t(($) => $.mermaid.show_source)
              }
            >
              <CodeIcon className={cn("size-4", showSource && "text-foreground")} />
            </ToolbarButton>
            <ToolbarButton
              onClick={handleCopySource}
              label={t(($) => $.mermaid.copy_source)}
            >
              {copied ? <Check className="size-4" /> : <Copy className="size-4" />}
            </ToolbarButton>
            <DropdownMenu>
              <TooltipHint label={t(($) => $.mermaid.export)} side="bottom">
                <DropdownMenuTrigger
                  render={
                    <button
                      type="button"
                      aria-label={t(($) => $.mermaid.export)}
                      className="flex size-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-secondary hover:text-foreground"
                    >
                      <Download className="size-4" />
                    </button>
                  }
                />
              </TooltipHint>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onClick={handleDownloadPng} disabled={!canExportImage}>
                  <ImageIcon className="size-4" />
                  {t(($) => $.mermaid.export_png)}
                </DropdownMenuItem>
                <DropdownMenuItem onClick={handleDownloadSvg} disabled={!canExportImage}>
                  <ImageIcon className="size-4" />
                  {t(($) => $.mermaid.export_svg)}
                </DropdownMenuItem>
                <DropdownMenuItem onClick={handleDownloadSource}>
                  <CodeIcon className="size-4" />
                  {t(($) => $.mermaid.export_source)}
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
            <MediaLinkAction kind="mermaid" content={chart} />
          </ViewerActions>
            {onClose && <ToolbarButton
              onClick={onClose}
              label={t(($) => $.mermaid.close_viewer)}
            >
              <X className="size-4" />
            </ToolbarButton>}
        </header>

        {showSource ? (
          <div className="min-h-0 flex-1 overflow-auto bg-background p-3">
            <CodeBlockStatic language="mermaid" body={chart} />
          </div>
        ) : (
          <ZoomCanvas
            canvas={canvas}
            content={layout}
            label={t(($) => $.mermaid.canvas_label)}
            className="bg-muted"
            canvasRef={canvasRef}
          >
            {viewerDocument && (
              <iframe
                className="mermaid-viewer-frame"
                sandbox=""
                srcDoc={viewerDocument}
                title={t(($) => $.mermaid.viewer_title)}
                style={{ width: `${layout.width}px`, height: `${layout.height}px` }}
              />
            )}
          </ZoomCanvas>
        )}
    </>
  );
}

export function MermaidViewerStandalone(props: MermaidViewerContentProps) {
  const canvasRef = useRef<HTMLDivElement>(null);
  return <div className="flex h-full min-h-0 w-full flex-col" data-testid="media-stage"><MermaidViewerContent {...props} canvasRef={canvasRef} /></div>;
}
