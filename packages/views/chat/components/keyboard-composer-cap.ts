/** Share of the visible viewport the composer may take while the soft keyboard is open. */
export const KEYBOARD_COMPOSER_MAX_RATIO = 0.4;

/**
 * Inline max-height (px) for the composer while the soft keyboard is up, or
 * undefined to leave the CSS caps in charge (keyboard closed, desktop).
 */
export function keyboardComposerMaxHeight(viewportHeight: number | undefined): number | undefined {
  return viewportHeight === undefined ? undefined : Math.floor(viewportHeight * KEYBOARD_COMPOSER_MAX_RATIO);
}
