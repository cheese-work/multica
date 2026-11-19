## Decision card placement (S-a) · doc — direction B, frame `LZpAu8plwy` `updatedAt` `2026-11-16T14:39:22.731Z`

Copied unchanged from doc frame `vRhBLklmy3` (`updatedAt` `2026-11-18T15:15:40.261Z`). Frame S-a, shared by both directions, desktop 1440 and phone 390. The card itself is CHE-5480, locked at a36972b8; this frame draws only where it sits.

### Purpose
Place the decision card inside the agent comment that asks, in the open state (waiting on you) and the decided state (collapsed to one line).

### UX reasoning
A decision belongs with the question that raised it, so the card lives in its own comment, never in a list of its own. While the card is open the thread cannot be folded, so a decision never hides. After the pick the card collapses to one line, "You picked 30 s", and the thread folds into the "Decided" row in Follow, or stays visible in Focus because it has you in it.

### UI reasoning
The "Waiting on you" chip is filled in the brand colour; the "Decided" chip is outlined and muted, so state is readable without colour. The card keeps the CHE-5480 layout and tokens; nothing is redrawn.

### Ergonomics
On phone, options are 48 px rows and Submit pick is 44 px and full width, inside the comment. The draft reply row stays below the card, so a question can be asked without losing the card.

### States
Open (waiting on you), decided and collapsed, on desktop and phone. Ask thread is S-b; the submitted result is S-c.

### Motion
On Submit pick, the card collapses with a 150 ms dissolve (no height animation). With reduced motion, it swaps at once.

### Accessibility
The card is a `role=group` named "Decision card" and options are one radio group. The selected option has a filled radio and a border, not colour alone. The collapsed line is a button that opens the full record.
