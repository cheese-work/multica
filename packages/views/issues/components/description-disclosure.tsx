import { useLayoutEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import {
  DESCRIPTION_PREVIEW_LINES,
  measureDescription,
  type DescriptionMeasurement,
} from "./description-measurement";

export interface DescriptionDisclosureLabels {
  readonly preview: string;
  readonly loading: string;
  readonly showMore: string;
  readonly showLess: string;
  readonly moreLines: (count: number) => string;
}

interface DescriptionDisclosureProps {
  readonly id: string;
  readonly expanded: boolean;
  readonly onExpandedChange: (expanded: boolean) => void;
  readonly children: ReactNode;
  readonly labels: DescriptionDisclosureLabels;
  readonly contentVersion?: string | number;
  readonly collapseDisabled?: boolean;
}

function useSettledMeasurement(
  rootRef: RefObject<HTMLDivElement | null>,
  contentVersion: string | number | undefined,
) {
  const [measurement, setMeasurement] = useState<DescriptionMeasurement | null>(null);

  useLayoutEffect(() => {
    const root = rootRef.current;
    if (!root) return;
    let frame = 0;
    let disposed = false;
    const schedule = () => {
      cancelAnimationFrame(frame);
      setMeasurement(null);
      frame = requestAnimationFrame(() => {
        if (!disposed) setMeasurement(measureDescription(root));
      });
    };
    const resizeObserver = new ResizeObserver(schedule);
    const mutationObserver = new MutationObserver(schedule);
    resizeObserver.observe(root);
    mutationObserver.observe(root, { childList: true, characterData: true, subtree: true });
    root.addEventListener("load", schedule, true);
    root.addEventListener("error", schedule, true);
    void document.fonts?.ready.then(schedule).catch(() => undefined);
    document.fonts?.addEventListener("loadingdone", schedule);
    // This layout-effect read lands before paint. It avoids briefly exposing a
    // full long editor while the asynchronous observers settle subsequent work.
    setMeasurement(measureDescription(root));

    return () => {
      disposed = true;
      cancelAnimationFrame(frame);
      resizeObserver.disconnect();
      mutationObserver.disconnect();
      root.removeEventListener("load", schedule, true);
      root.removeEventListener("error", schedule, true);
      document.fonts?.removeEventListener("loadingdone", schedule);
    };
  }, [contentVersion, rootRef]);

  return measurement;
}

/**
 * A controlled shell around the one real description editor. The child stays
 * mounted across every state transition; issue-detail owns its lifecycle.
 */
export function DescriptionDisclosure({
  id,
  expanded,
  onExpandedChange,
  children,
  labels,
  contentVersion,
  collapseDisabled = false,
}: DescriptionDisclosureProps) {
  const editorRef = useRef<HTMLDivElement>(null);
  const measurement = useSettledMeasurement(editorRef, contentVersion);
  const canDisclose = measurement?.hasOverflow === true;
  // During invalidation, keep the editor clipped and expose only the loading
  // label. A fresh exact count arrives on the next animation frame.
  const collapsed = !expanded && (measurement === null || canDisclose);
  const controlsId = `${id}-description`;
  const previewText = measurement?.previewText;
  const buttonLabel =
    measurement && measurement.hiddenRows > 0
      ? `${labels.showMore} ${labels.moreLines(measurement.hiddenRows)}`
      : labels.showMore;

  return (
    <section id={controlsId} data-description-disclosure>
      <div
        ref={editorRef}
        aria-hidden={collapsed || undefined}
        data-description-editor
        data-find-ignore={collapsed ? "true" : undefined}
        inert={collapsed || undefined}
        onPointerDownCapture={(event) => {
          if (!collapsed) return;
          event.preventDefault();
          event.stopPropagation();
          onExpandedChange(true);
        }}
        style={
          collapsed
            ? {
                maxHeight: measurement
                  ? `${measurement.lineHeight * DESCRIPTION_PREVIEW_LINES}px`
                  : `${DESCRIPTION_PREVIEW_LINES}lh`,
                overflow: "hidden",
              }
            : undefined
        }
      >
        {children}
      </div>
      {collapsed && (
        <div className="sr-only" data-description-accessible-preview data-find-ignore="true">
          <span>{labels.preview}</span>
          <span>{previewText || labels.loading}</span>
        </div>
      )}
      {canDisclose ? (
        <div className="mt-2 flex justify-center">
          <button
            aria-controls={controlsId}
            aria-expanded={expanded}
            disabled={expanded && collapseDisabled}
            onClick={() => onExpandedChange(!expanded)}
            type="button"
          >
            {expanded ? labels.showLess : buttonLabel}
          </button>
        </div>
      ) : measurement === null ? (
        <div className="mt-2 flex justify-center">
          <button aria-controls={controlsId} aria-expanded={expanded} onClick={() => onExpandedChange(!expanded)} type="button">
            {expanded ? labels.showLess : labels.showMore}
          </button>
        </div>
      ) : null}
    </section>
  );
}
