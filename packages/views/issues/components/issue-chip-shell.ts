/** Box of an issue chip. Shared with the loading box so both render the same
 *  border, padding, gap and line height. */
export const ISSUE_CHIP_BASE_CLASS =
  "issue-mention inline-flex min-w-0 max-w-[min(18rem,100%)] items-center gap-1.5 rounded-md border mx-0.5 px-2 py-0.5 text-caption";

/** Pins the chip to a fixed 14 rem (224 px) instead of its content, and to the
 *  container when that is narrower. Used by a bare key's loading box and its
 *  resolved chip, so the line breaks the same way before and after the lookup
 *  settles. 14 rem is the width selected in design/bare-key-chip-stability
 *  (PR #294); it stays within the 18 rem content cap above. */
export const ISSUE_CHIP_STABLE_WIDTH_CLASS = "w-[min(14rem,100%)]";
