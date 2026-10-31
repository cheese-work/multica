## What changed vs rev-0

rev-0 is the current web and native chat, which has no scroll-to-bottom control. Current screens were not captured. No device or simulator is admitted for this task. The mockups follow the source of `chat-message-list.tsx`, `chat-column.ts` and the token files instead.

- New round control above the composer, centered on the reading column (`CHAT_COLUMN`, `max-w-4xl`).
- Shown only when scrolled up. Hidden at the live end and while loading.
- Each direction shows: desktop scrolled up (light), desktop live end (dark), phone web light and dark, live end, restored at anchor, anchor not loaded (start at newest, button shown), keyboard focus ring, disconnected, loading, and native phone light, dark, restored, disconnected.
- Size: 36 px on desktop (`--button-height-lg`), 44 px on a coarse pointer and on native. 44 px is a proposal; current button heights stop at 36 px.

## Directions

- A — Neutral raised disc: surface-raised fill, surface-border ring, menu shadow.
- B — Brand-filled disc with floating shadow. Highest contrast; competes with the composer send button.
- C — Translucent blurred disc, muted arrow, no shadow. Lowest emphasis.

## Decisions for Cheese

1. Look: A, B or C. Placement and behavior are already decided.
2. 44 px touch target on phone widths (all directions) against the current 36 px button ladder.

## Verification limits

- Static HTML from copied tokens. Not rendered by React, Virtuoso, FlashList or React Native. Fonts are the browser system font.
- Native frames reuse the web token values for dark mode and draw the status bar and home indicator as simple shapes.
- The "anchor" frames outline the anchor message and the saved item label. That outline is a mockup annotation, not UI.
- Disconnected banner copy is a placeholder; the real offline banner component is not reused.
- Contrast of the button boundary against the page is below 3:1 in A and C. The arrow icon carries the identification. Run a contrast check before build.
- Scroll motion, keyboard-open layout (visual viewport) and reduced-motion behavior are specified in the page text and not drawn.
- `visual diff: NOT-RUN`. No before capture of the current screens: needs a browser session or device on X99.
