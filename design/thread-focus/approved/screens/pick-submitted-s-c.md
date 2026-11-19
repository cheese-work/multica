## Pick submitted (S-c) · doc — direction B, frame `JcaWYZ7tyo` `updatedAt` `2026-11-16T14:38:40.518Z`

Copied unchanged from doc frame `KRpLoTHVJ0` (`updatedAt` `2026-11-18T15:15:57.095Z`). Frame S-c, shared by both directions, desktop 1440 and phone 390. The state right after "Submit pick".

### Purpose
Confirm the decision and show what is left: the card collapsed in its comment, a "Pick saved" bar, and one thread still waiting.

### UX reasoning
The bar says what happened and what remains ("You picked 30 s. 1 thread still waits on you."), so the next step is clear without searching. The decided thread stays visible in Focus and folds into the "Decided" row in Follow. In the prototype, one button returns to the issue view of the direction under review.

### UI reasoning
The saved bar uses the tinted info style with a check icon and a bold lead, the same as other confirmations on the page. The remaining waiting thread keeps its filled "Waiting on you" chip.

### Ergonomics
On phone, the Back to the issue button is full width and 44 px, directly under the collapsed card. The counter on the bar gives the next target without scrolling.

### States
Pick saved with one thread left (this frame). With nothing left, the page shows the empty state in B-f. A failed save shows the error state in B-f.

### Motion
The card collapses with a 150 ms dissolve; the counter changes with a swap. With reduced motion, both are instant.

### Accessibility
The saved bar is `role=status`, so the result is announced. The decided state is shown in text ("Decided", "You picked 30 s"), not in colour alone. Buttons have visible focus rings.
