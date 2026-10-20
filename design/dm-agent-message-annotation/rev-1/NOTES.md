## What changed vs rev-0

rev-0 is the current DM. An agent reply has one action, Copy, and the composer has no comment list. Not captured: the mockups follow the source of the current screens. Compared against rev-0:

- An agent reply gains a floating "Add comment" button over a text selection, then a note box.
- Saved comments show as a highlight and a numbered marker on the reply.
- The composer gains a place that lists the pending comments (it differs per direction).
- Send turns the comments into Markdown quotes with notes above the typed text. The sent bubble is the same in all directions because it is plain Markdown (no server change).
- Every direction shows: select, note, saved, sent, limits (20 comments, 4,000 characters), reload with one lost quote, and the cases with no button (streaming reply, own bubble) and the offline composer.

## Directions

- A — Same as issues. Marker at the right edge; a "N comments" button above the composer opens the list.
- B — Notes stay visible. Same markers; the comments are cards above the composer, no popover.
- C — Margin notes. The note sits beside the quote (below it on a phone); the composer shows only a count and Clear all.

## Decisions for Cheese

1. Pending comments: a list behind a button (A), cards always visible (B) or notes on the reply (C).
2. Word for the count: "comments" (DM request) or "annotations" (issue locale today). The mockups use "comments".
3. Agent replies only (premise 1). Your own bubbles have no button in every direction.
4. Mobile is a follow-up. Phone width here is the web layout at 390 px, not the Expo app.

## Verification limits

- Static HTML from `tokens.css` values. Not rendered by the real app; the browser system font, simplified icons.
- Marker and popover positions are drawn by hand. The real ones come from floating-ui and the CSS Custom Highlight API.
- Dark theme not drawn.
- `visual diff: NOT-RUN`. No before capture of the current DM.
- PNGs are 256-colour palette files (131.9 to 140.8 KB) to fit the 300 KB image budget. Same dimensions; antialiased edges and tints shift slightly.
