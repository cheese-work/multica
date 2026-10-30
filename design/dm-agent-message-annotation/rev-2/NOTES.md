## What changed vs rev-1

`rev-1/` is frozen at the head the UX reviewer scored. Cheese chose A: "I like design A." (comment `01a15825-89b1-72c7-94ce-75f6a200e75d`). Rev-2 is direction A only. It answers the signed UX scores on `9bee133c2122420720aab4e116fe933672d36ec4` (A11y 6, Density 5) and Sol's ask in comment `01a15853-8b59-7a14-b45e-34ed6e258d22`.

- Every row in the list has a visible Edit button next to Remove. Reviewer: "saved rows show removal without a visible edit affordance".
- The list scrolls and shows all 20 comments. Frames 5 and 6 show rows 1–4 and 17–20, a scroll bar, "Comments 17–20 of 20" and a focused Edit on row 20. Reviewer: "no scroll or alternate route to clipped notes". Rev-1 drew `overflow: hidden`. The shipped list in `reply-annotations.tsx` already has `overflow-y-auto`, so that was a mockup error, not a code bug.
- Marker, quote and list row light up together (frames 3 and 4). The note box carries the label "Comment N" with the same number. Reviewer: "markers lack a shown association to notes".
- Frame 7 shows the keyboard and focus order, tagged "today" or "rev-2".
- Frames 8–11 repeat rev-1's sent, limits, reload and not-available states. Frame 10 now shows where the "source unavailable" message sits under Edit 2.

## Direction

- A — Same as issues, revised. Everything above.

## Build requirements this design adds

1. Each list row gets an Edit button (today the whole quote is the edit button but looks like text).
2. The scroll area is a tab stop with a name and keyboard scrolling. Focus moving to a row scrolls it into view.
3. Hover or focus on a row, or focus on a marker, sets the same active state on its marker, quote highlight and row.
4. Markers must be reachable in reading order. Today `CommentSelectionBubble` renders them with `createPortal` into `document.body`, so Tab reaches them after the whole page. This needs a DOM or focus-management change.
5. Focus return: after Add, to the new marker. After Esc in the note box, to the reply (today's `close(true)`). After Remove, to the next row's Edit, or the count when none is left.
6. Focus ring on these controls is `--brand` 2 px. The `--ring` token is about 2.6:1 against white (estimate from the token values), under the 3:1 non-text minimum. `--brand` is about 4.7:1.
7. The note box focuses itself only on a fine pointer today. On touch the user taps the note box.

## Decisions for Cheese

None new. Premises from the CHE-1876 description stand: agent replies only, web and desktop, mobile later.

## Verification limits

- Static HTML from `tokens.css` values. Not rendered by the real app; the browser system font, simplified icons, one small script that scrolls the list in frame 6.
- Marker and popover positions are drawn by hand. Real ones come from floating-ui and the CSS Custom Highlight API.
- The keyboard order is described and drawn, not run. No screen reader test. Light theme only.
- The 20-comment frames show 2 markers in the reply. The other 18 quotes sit in lower text that the mockup omits.
- PNGs are 256-colour palette files, 250.8 KB (1440 px, 3842 high) and 256.1 KB (390 px, 7545 high), under the 300 KB budget.
- `visual diff: NOT-RUN`. No before capture of the current DM.
