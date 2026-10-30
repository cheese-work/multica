export interface RowBox { top: number; height: number }

/** 1-based range of rows fully inside the viewport; the row in view when none fits whole. */
export function visibleRowRange(rows: RowBox[], scrollTop: number, viewport: number): { from: number; to: number } | null {
  if (!rows.length || viewport <= 0) return null;
  const bottom = scrollTop + viewport;
  const whole = rows.flatMap((r, i) => (r.top >= scrollTop - 1 && r.top + r.height <= bottom + 1 ? [i + 1] : []));
  if (whole.length) return { from: whole[0]!, to: whole.at(-1)! };
  const seen = rows.findIndex((r) => r.top + r.height > scrollTop && r.top < bottom);
  return seen < 0 ? null : { from: seen + 1, to: seen + 1 };
}
