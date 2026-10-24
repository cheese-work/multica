## What changed vs rev-0

rev-0 is the current DM view: header, messages, composer, and an offline banner above the composer. It has no list of pending decisions. Cheese: "DM should show active decision, so users won't get surprised if anything goes south." The current live DM was not captured: the mockups follow `chat-window.tsx`, `chat-column.ts` and `offline-banner.tsx`.

- Adds a list of the session's decision requests in three placements (A strip, B header chip, C dock).
- Each row: state in text and icon, issue key, title, class, age.
- Every direction draws: default, five requests, a request cancelled while the list is on screen, answered only, empty, loading, load failed, disconnected, long title.
- Cancelled rows linger 5 s with a struck title and the reason in text, then leave (decision 2 below).

## Directions

- A — Pinned strip under the DM header. Always visible. Folds to one line. Shows 3 rows, then "Show N more".
- B — Count chip in the DM header. A popover holds the list. No space cost; the list is one click away.
- C — One line above the composer shows the oldest ask waiting on you, "+N more" opens the list upward. The ask sits next to the answer box.

## Decisions for Cheese

1. Placement: A, B or C. The request says "top of a DM session view", which is A. B and C trade visibility for space.
2. A cancelled or closed row lingers 5 s with its reason, then leaves (all directions). The alternative is to remove it at once. Lingering keeps the cancel from being a silent change.
3. A fold by the person (A) lasts for this view only, and is not saved. The alternative is a saved preference.
4. Answered rows stay until the request closes. The alternative is to drop them at once.

## Verification limits

- Static HTML from `tokens.css` values, light theme only. Dark theme not drawn. Not rendered by React; fonts are the browser system font.
- Row data is invented. Class badge tone (red for spend, deploy, publish, irreversible) is a proposal.
- Icons are simplified lucide-style drawings.
- Keyboard order, focus return and the live-region text are described in captions, not run.
- `visual diff: NOT-RUN`. No before capture of the live DM.
- `build.py` regenerates A.html, B.html and C.html. PNGs: desktop 1440 px and phone 390 px, full page, captured on X99 with Playwright 1.58.2 on Chrome.
