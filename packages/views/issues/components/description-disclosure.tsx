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
  const [refreshing, setRefreshing] = useState(true);

  useLayoutEffect(() => {
    const root = rootRef.current;
    if (!root) return;
    let frame = 0;
    let disposed = false;
    const schedule = () => {
      cancelAnimationFrame(frame);
      setRefreshing(true);
      frame = requestAnimationFrame(() => {
        if (!disposed) {
          setMeasurement(measureDescription(root));
          setRefreshing(false);
        }
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
    setRefreshing(false);

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

  return { measurement, refreshing };
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
  const { measurement, refreshing } = useSettledMeasurement(editorRef, contentVersion);
  const canDisclose = measurement?.hasOverflow === true;
  // Initial measurement is a layout-effect read. Later invalidations retain
  // the last geometry so short descriptions never flicker into a collapsed UI.
  const collapsed = !expanded && (measurement === null || canDisclose);
  const editorId = `${id}-description-editor`;
  const previewText = measurement?.previewText;
  const buttonLabel =
    measurement && !refreshing && measurement.hiddenRows > 0
      ? `${labels.showMore} ${labels.moreLines(measurement.hiddenRows)}`
      : labels.showMore;

  return (
    <section data-description-disclosure>
      <div
        ref={editorRef}
        aria-hidden={collapsed || undefined}
        data-description-editor
        data-find-ignore={collapsed ? "true" : undefined}
        id={editorId}
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
            aria-controls={editorId}
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
          <button aria-controls={editorId} aria-expanded={expanded} onClick={() => onExpandedChange(!expanded)} type="button">
            {expanded ? labels.showLess : labels.showMore}
          </button>
        </div>
      ) : null}
    </section>
  );
}
