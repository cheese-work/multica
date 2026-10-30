"use client";

/**
 * Pan/zoom controller for content that cannot zoom itself — a Mermaid diagram
 * in an empty-sandbox iframe, or an image in the attachment preview.
 *
 * Every gesture is captured on the host viewport element and applied as a CSS
 * transform on the wrapper around the content. For the Mermaid case that
 * inversion is also what keeps Escape working: the iframe is `pointer-events:
 * none`, so it never takes focus, and key events always stay in the host
 * document where the Dialog can hear them.
 *
 * Transform math lives in `../utils/zoom-transform`, gesture logic (pointer
 * pan/pinch, wheel, Safari pinch) in `../utils/gesture-session`; this hook owns
 * only event plumbing and state.
 */

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
  type KeyboardEvent as ReactKeyboardEvent,
  type MouseEvent as ReactMouseEvent,
} from "react";
import {
  MAX_SCALE,
  PAN_STEP_PX,
  ZOOM_STEP,
  centerTransform,
  clampTransform,
  computeFitScale,
  computeFitTransform,
  computeMinScale,
  panBy,
  resizeTransform,
  zoomByAtCenter,
  zoomToAt,
  type Point,
  type Size,
  type ZoomTransform,
} from "../utils/zoom-transform";
import {
  EMPTY_POINTER_SESSION,
  IDLE_SAFARI_GESTURE,
  classifyWheel,
  gestureChange,
  gestureEnd,
  gestureStart,
  pointerDown,
  pointerMove,
  pointerUp,
  shouldIgnoreWebkitWheel,
} from "../utils/gesture-session";

interface UseZoomCanvasOptions {
  /** Natural (unscaled) size of the content, or null while it is unknown. */
  content: Size | null;
  /**
   * Whether Left/Right pan the canvas. Set false when the surrounding viewer
   * binds those keys itself — image-sequence prev/next (MUL-5752) — so the two
   * handlers can't both fire on one press. Up/Down keep panning either way,
   * and drag / wheel still pan horizontally.
   */
  horizontalArrowPan?: boolean;
}

export interface ZoomCanvasApi {
  /**
   * Callback ref for the viewport element. Deliberately not a RefObject: the
   * canvas is portaled by the Dialog and attaches in a later commit than this
   * hook's first layout effect, so an effect keyed on `[]` would find no
   * element, skip measuring, and never observe a resize — leaving the canvas
   * permanently unfitted and inert.
   */
  setViewportNode: (node: HTMLDivElement | null) => void;
  transform: ZoomTransform;
  /** Whole-percent zoom for the toolbar readout. */
  zoomPercent: number;
  canZoomIn: boolean;
  canZoomOut: boolean;
  isPanning: boolean;
  /**
   * True when the last change came from a discrete control (button/keyboard)
   * rather than direct manipulation, so the transform can be eased. Dragging
   * and wheel/pinch must track the input 1:1 and are never animated.
   */
  isAnimated: boolean;
  zoomIn: () => void;
  zoomOut: () => void;
  zoomToActualSize: () => void;
  fit: () => void;
  handlePointerDown: (event: ReactPointerEvent<HTMLElement>) => void;
  handlePointerMove: (event: ReactPointerEvent<HTMLElement>) => void;
  handlePointerUp: (event: ReactPointerEvent<HTMLElement>) => void;
  handleKeyDown: (event: ReactKeyboardEvent<HTMLElement>) => void;
  /** Double-click / double-tap toggles between the default fit and 100%. */
  handleDoubleClick: (event: ReactMouseEvent<HTMLElement>) => void;
}

const IDENTITY: ZoomTransform = { scale: 1, x: 0, y: 0 };
const EMPTY_SIZE: Size = { width: 0, height: 0 };

function nearlyEqual(a: number, b: number): boolean {
  return Math.abs(a - b) < 0.5;
}

export function useZoomCanvas({
  content,
  horizontalArrowPan = true,
}: UseZoomCanvasOptions): ZoomCanvasApi {
  const [viewportNode, setViewportNode] = useState<HTMLDivElement | null>(null);
  const [viewport, setViewport] = useState<Size>(EMPTY_SIZE);
  const [transform, setTransform] = useState<ZoomTransform>(IDENTITY);
  const [isPanning, setIsPanning] = useState(false);
  const [isAnimated, setIsAnimated] = useState(false);

  // Fit is deferred until both sizes are known; until then any transform we
  // computed would be against a 0x0 viewport and would have to be thrown away.
  const hasFittedRef = useRef(false);
  const sessionRef = useRef(EMPTY_POINTER_SESSION);
  const touchPointerIdsRef = useRef(new Set<number>());
  const safariGestureRef = useRef(IDLE_SAFARI_GESTURE);

  const contentSize = content ?? EMPTY_SIZE;
  const hasContent = contentSize.width > 0 && contentSize.height > 0;
  const hasViewport = viewport.width > 0 && viewport.height > 0;
  const ready = hasContent && hasViewport;

  // Latest-value refs: the native wheel listener below is registered once with
  // `{ passive: false }` and would otherwise close over stale state.
  const stateRef = useRef({ transform, contentSize, viewport, ready });
  stateRef.current = { transform, contentSize, viewport, ready };

  useLayoutEffect(() => {
    const element = viewportNode;
    if (!element) return;

    // offsetWidth/offsetHeight, NOT getBoundingClientRect: both viewers open
    // inside a container that scales in (`zoom-in-95` on the shared Dialog,
    // motion's scale-in on the attachment viewer's stage). A client rect is
    // transform-scaled, so measuring mid-animation would fit the content
    // against a viewport a few percent too small — and because ResizeObserver
    // reports the untransformed layout box, it never fires when the animation
    // lands, leaving that mis-fit on screen for the whole session.
    const measure = () => {
      const width = element.offsetWidth;
      const height = element.offsetHeight;
      setViewport((previous) =>
        nearlyEqual(previous.width, width) && nearlyEqual(previous.height, height)
          ? previous
          : { width, height },
      );
    };

    measure();

    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(measure);
    observer.observe(element);

    return () => observer.disconnect();
  }, [viewportNode]);

  // Fit once, on first open — a layout effect, so the first painted frame is
  // already fitted. Later viewport changes never snap a zoom the user chose
  // back to fit (that would lose their place); only a view still at fit
  // follows a resize, below.
  useLayoutEffect(() => {
    if (!ready || hasFittedRef.current) return;
    hasFittedRef.current = true;
    setTransform(computeFitTransform(contentSize, viewport));
  }, [ready, contentSize, viewport]);

  // Genuinely different content (new natural size) starts fresh. Also a
  // layout effect, and never eased: swapping to another image (sequence
  // navigation) is content replacement, not a camera move on the same
  // content. The new frame must paint already fitted — a plain effect would
  // paint it once under the previous image's pan/zoom, and a leftover
  // `isAnimated` from the last discrete control would ease the correction.
  const contentKey = `${contentSize.width}x${contentSize.height}`;
  const previousContentKeyRef = useRef(contentKey);
  useLayoutEffect(() => {
    if (previousContentKeyRef.current === contentKey) return;
    previousContentKeyRef.current = contentKey;
    if (!ready) return;
    setIsAnimated(false);
    setTransform(computeFitTransform(contentSize, viewport));
  }, [contentKey, ready, contentSize, viewport]);

  // Keep the content in view when the window/pane is resized under it — and
  // keep a still-fitted image fitted (see `resizeTransform`).
  const previousViewportRef = useRef(viewport);
  useEffect(() => {
    const previous = previousViewportRef.current;
    previousViewportRef.current = viewport;
    if (!ready) return;
    setTransform((current) =>
      resizeTransform(current, contentSize, previous, viewport),
    );
  }, [ready, contentSize, viewport]);

  // Direct-manipulation updates also write the ref: events outrun renders, and
  // a pointer-set change re-baselines from the transform the user last saw.
  const applyGestureTransform = useCallback((next: ZoomTransform) => {
    stateRef.current.transform = next;
    setTransform(next);
  }, []);

  const fit = useCallback(() => {
    const { contentSize: c, viewport: v, ready: r } = stateRef.current;
    if (!r) return;
    setIsAnimated(true);
    setTransform(computeFitTransform(c, v));
  }, []);

  const zoomToActualSize = useCallback(() => {
    const { contentSize: c, viewport: v, ready: r } = stateRef.current;
    if (!r) return;
    setIsAnimated(true);
    setTransform(clampTransform(centerTransform(c, v, 1), c, v));
  }, []);

  const zoomBy = useCallback((factor: number) => {
    const { transform: t, contentSize: c, viewport: v, ready: r } = stateRef.current;
    if (!r) return;
    setIsAnimated(true);
    setTransform(zoomByAtCenter(t, factor, c, v));
  }, []);

  const zoomIn = useCallback(() => zoomBy(ZOOM_STEP), [zoomBy]);
  const zoomOut = useCallback(() => zoomBy(1 / ZOOM_STEP), [zoomBy]);

  // Wheel and Safari gesture events must be native non-passive listeners:
  // React routes them through a passive root listener, where preventDefault is
  // ignored and the page would scroll or zoom behind the dialog instead.
  useEffect(() => {
    const element = viewportNode;
    if (!element) return;

    const toLocal = (event: { clientX: number; clientY: number }): Point => {
      const rect = element.getBoundingClientRect();
      return { x: event.clientX - rect.left, y: event.clientY - rect.top };
    };
    // WebKit only (macOS and iOS Safari): pinch arrives as `gesture*` events.
    const webkit = typeof window !== "undefined" && "GestureEvent" in window;

    const onWheel = (event: WheelEvent) => {
      const { transform: t, contentSize: c, viewport: v, ready: r } = stateRef.current;
      if (!r) return;
      event.preventDefault();
      if (webkit && shouldIgnoreWebkitWheel(safariGestureRef.current, event, event.timeStamp)) {
        return;
      }
      setIsAnimated(false);

      const action = classifyWheel(event);
      applyGestureTransform(
        action.type === "zoom"
          ? zoomToAt(t, t.scale * action.factor, toLocal(event), c, v)
          : panBy(t, action.dx, action.dy, c, v),
      );
    };

    // Gesture events carry clientX/clientY and a cumulative `scale`; they are
    // not in lib.dom, so read them through a narrow structural type.
    type GestureLike = Event & { scale: number; clientX: number; clientY: number };

    const onGestureStart = (event: Event) => {
      event.preventDefault();
      if (!stateRef.current.ready) return;
      safariGestureRef.current = gestureStart(
        safariGestureRef.current,
        stateRef.current.transform,
        toLocal(event as GestureLike),
      );
    };
    const onGestureChange = (event: Event) => {
      // Always cancel: on iOS an unhandled gesture zooms the page.
      event.preventDefault();
      const { contentSize: c, viewport: v, ready: r } = stateRef.current;
      if (!r) return;
      const next = gestureChange(
        safariGestureRef.current,
        (event as GestureLike).scale,
        touchPointerIdsRef.current.size > 0,
        { content: c, viewport: v },
      );
      if (!next) return;
      setIsAnimated(false);
      applyGestureTransform(next);
    };
    const onGestureEnd = (event: Event) => {
      event.preventDefault();
      safariGestureRef.current = gestureEnd(safariGestureRef.current, event.timeStamp);
    };

    element.addEventListener("wheel", onWheel, { passive: false });
    if (webkit) {
      element.addEventListener("gesturestart", onGestureStart, { passive: false });
      element.addEventListener("gesturechange", onGestureChange, { passive: false });
      element.addEventListener("gestureend", onGestureEnd, { passive: false });
    }
    return () => {
      element.removeEventListener("wheel", onWheel);
      element.removeEventListener("gesturestart", onGestureStart);
      element.removeEventListener("gesturechange", onGestureChange);
      element.removeEventListener("gestureend", onGestureEnd);
    };
  }, [viewportNode, applyGestureTransform]);

  const localPoint = useCallback(
    (event: { clientX: number; clientY: number }): Point => {
      const rect = viewportNode?.getBoundingClientRect();
      return {
        x: event.clientX - (rect?.left ?? 0),
        y: event.clientY - (rect?.top ?? 0),
      };
    },
    [viewportNode],
  );

  const handlePointerDown = useCallback(
    (event: ReactPointerEvent<HTMLElement>) => {
      if (!stateRef.current.ready) return;
      // Ignore secondary mouse buttons so right-click never starts a pan.
      if (event.pointerType === "mouse" && event.button !== 0) return;

      setIsAnimated(false);
      if (event.pointerType === "touch") touchPointerIdsRef.current.add(event.pointerId);
      sessionRef.current = pointerDown(
        sessionRef.current,
        event.pointerId,
        localPoint(event),
        stateRef.current.transform,
      );
      event.currentTarget.setPointerCapture?.(event.pointerId);
      setIsPanning(sessionRef.current.pointers.length === 1);
    },
    [localPoint],
  );

  const handlePointerMove = useCallback(
    (event: ReactPointerEvent<HTMLElement>) => {
      const { contentSize: c, viewport: v, ready: r } = stateRef.current;
      if (!r) return;
      const { session, transform: next } = pointerMove(
        sessionRef.current,
        event.pointerId,
        localPoint(event),
        { content: c, viewport: v },
      );
      sessionRef.current = session;
      if (next) applyGestureTransform(next);
    },
    [localPoint, applyGestureTransform],
  );

  // pointerup and pointercancel both land here: only the lifted pointer leaves
  // and the survivors re-baseline, so the canvas never jumps.
  const handlePointerUp = useCallback((event: ReactPointerEvent<HTMLElement>) => {
    touchPointerIdsRef.current.delete(event.pointerId);
    event.currentTarget.releasePointerCapture?.(event.pointerId);
    sessionRef.current = pointerUp(
      sessionRef.current,
      event.pointerId,
      stateRef.current.transform,
    );
    setIsPanning(sessionRef.current.pointers.length === 1);
  }, []);

  const handleKeyDown = useCallback(
    (event: ReactKeyboardEvent<HTMLElement>) => {
      const { contentSize: c, viewport: v, ready: r } = stateRef.current;
      if (!r) return;

      // Escape is the Dialog's; never swallow it here.
      switch (event.key) {
        case "+":
        case "=":
          event.preventDefault();
          zoomIn();
          return;
        case "-":
        case "_":
          event.preventDefault();
          zoomOut();
          return;
        case "0":
          event.preventDefault();
          fit();
          return;
        case "ArrowLeft":
        case "ArrowRight":
          // Left to the viewer (image sequence prev/next) when it owns them.
          // No preventDefault, so the event keeps bubbling to its handler.
          if (!horizontalArrowPan) return;
        // fallthrough
        case "ArrowUp":
        case "ArrowDown": {
          event.preventDefault();
          setIsAnimated(true);
          const step = event.shiftKey ? PAN_STEP_PX * 3 : PAN_STEP_PX;
          const deltaX =
            event.key === "ArrowLeft" ? step : event.key === "ArrowRight" ? -step : 0;
          const deltaY =
            event.key === "ArrowUp" ? step : event.key === "ArrowDown" ? -step : 0;
          setTransform((current) => panBy(current, deltaX, deltaY, c, v));
          return;
        }
        default:
      }
    },
    [zoomIn, zoomOut, fit, horizontalArrowPan],
  );

  // Double-click toggles fit <-> 100%, anchored under the cursor on the way in.
  // The universal image-viewer gesture (macOS Preview, browsers): one gesture
  // gets from "the whole thing" to "readable detail" and back.
  const handleDoubleClick = useCallback(
    (event: ReactMouseEvent<HTMLElement>) => {
      const { transform: t, contentSize: c, viewport: v, ready: r } = stateRef.current;
      if (!r) return;

      setIsAnimated(true);
      const fitScale = computeFitScale(c, v);
      if (Math.abs(t.scale - fitScale) >= 0.001) {
        setTransform(computeFitTransform(c, v));
        return;
      }
      // Already fitted. Go to 100% — or to 200% when fit already is 100%, so a
      // small image still has somewhere to go instead of ignoring the gesture.
      setTransform(zoomToAt(t, fitScale < 0.999 ? 1 : 2, localPoint(event), c, v));
    },
    [localPoint],
  );

  return {
    setViewportNode,
    transform,
    zoomPercent: Math.round(transform.scale * 100),
    canZoomIn: transform.scale < MAX_SCALE - 0.001,
    // Against the dynamic floor, not MIN_SCALE: for content that only fits
    // below 25% the button must stay live all the way down to that fit scale.
    canZoomOut: transform.scale > computeMinScale(contentSize, viewport) + 0.001,
    isPanning,
    isAnimated,
    zoomIn,
    zoomOut,
    zoomToActualSize,
    fit,
    handlePointerDown,
    handlePointerMove,
    handlePointerUp,
    handleKeyDown,
    handleDoubleClick,
  };
}

export type { Size, ZoomTransform };
export { MAX_SCALE };
