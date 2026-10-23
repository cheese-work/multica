## What changed vs rev-1

Source: `git diff --no-index design/dm-decision-quote-cards/rev-1 design/dm-decision-quote-cards/rev-2` (exit 1, differences). `rev-1` is unchanged.

- UX review of rev-1 (Luna, comment `01a1580e-77c8-7203-b722-001c3e93b87b`): C overflowed and its text overlapped the action at 390 px. Now the DM card has no minimum width and everything wraps. B's phone strip was crowded. Now actions sit in their own row and the DM chip wraps. A's DM card was understated. Now it is a tinted card with an accent bar and the words "Quoted in CHE-1870".
- Missing states, from the same review: every direction now shows loading (skeleton), empty (a bubble with no quote has no card), offline (Disclaim disabled, reason in visible text), error (decision unchanged, Try again), and disclaiming (spinner, disabled).
- Phone targets: buttons and links are 44 px at 390 px width (36 px on desktop).
- Live captures: the current screens are in `rev-2/live/` (issue timeline and DM, desktop 1440 and phone 390). They were taken on X99 from the running web app at the PR 1 head `fc9824eac1ac0cdc4212b61ed016a6f7921dd537`. The head has no UI change, so they show the current UI. The round dark "N" badge at the bottom-left is the Next.js development indicator, not product UI. The DM live capture shows the "Decision recorded" message as plain text with dead "Open issue" and "Disclaim" lines.
- The mockups now follow the live look: assistant text is plain, the user bubble is muted, the issue comment shows avatar, name and time.
- Images are WebP, each under 300 KB (`image-budget`). The `rev-1` PNGs are untouched and still over budget; see the PR comment.

## Directions

- A — Quiet quote: human header with a "via DM" chip, quote under a rule, muted sub-line, actions in their own row. DM: tinted info card with an accent bar.
- B — Receipt card: the quote sits in a bordered decision card. DM: a rounded chip that says "Quoted in CHE-1870".
- C — Timeline row: a plain message, state in a footer row, actions in a ··· menu (shown open). DM: a two-line card with a state stepper.

## Keep / Change / Remove (against the live screens)

- Keep: the issue comment card layout (avatar, name, time, body, reply row) and the DM layout (plain assistant text, muted user bubble, composer).
- Change: the agent-authored `>` blockquote becomes Cheese's quote card with the agent in a sub-line. The plain-text "Decision recorded" message is replaced by a card under the quoted bubble.
- Remove: the "Open issue" and "Disclaim" text lines of the "Decision recorded" message (D3 = A hides the message). The row stays in the database.

## Limits

- Static HTML. The flash is shown at its peak frame, not recorded. Live captures show the current UI only; the new cards exist only as mockups.
- Keyboard and screen-reader behaviour is NOT-RUN.
