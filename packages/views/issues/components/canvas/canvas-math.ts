export interface Box { x: number; y: number; w: number; h: number }
export interface View { x: number; y: number; scale: number }

const center = (box: Box) => box.x + box.w / 2;

/**
 * Zoom to `scale`, keeping the world point under `anchor` (screen px) fixed.
 * There is no fixed bound; only a scale that is not a positive finite number
 * (underflow, overflow, NaN) or one whose offsets overflow is refused, and the
 * view stays as it was.
 */
export function zoomAt(view: View, scale: number, anchor: { x: number; y: number }): View {
  const next = scale;
  if (!(Number.isFinite(next) && next > 0)) return view;
  const ratio = next / view.scale;
  const x = anchor.x - (anchor.x - view.x) * ratio;
  const y = anchor.y - (anchor.y - view.y) * ratio;
  return Number.isFinite(x) && Number.isFinite(y) ? { scale: next, x, y } : view;
}

export function centerOn(box: Box, viewport: { w: number; h: number }, scale: number): View {
  return { scale, x: viewport.w / 2 - center(box) * scale, y: viewport.h / 2 - (box.y + box.h / 2) * scale };
}

export function fitView(bounds: Box, viewport: { w: number; h: number }, pad = 32): View {
  const scale = Math.min((viewport.w - pad * 2) / bounds.w, (viewport.h - pad * 2) / bounds.h, 1);
  return centerOn(bounds, viewport, scale);
}

export const panBy = (view: View, dx: number, dy: number): View => ({ ...view, x: view.x + dx, y: view.y + dy });

export const intersects = (a: Box, b: Box) => a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
