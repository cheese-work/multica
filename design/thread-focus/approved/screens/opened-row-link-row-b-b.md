## Opened row and link row (B-b) · doc — direction B, frame `Vf27zqFPd7` `updatedAt` `2026-11-16T20:23:28.642Z`

Copied unchanged from doc frame `qS1_ldy9zi` (`updatedAt` `2026-11-18T15:13:55.588Z`). Frame B-b, desktop 1440 and phone 390. Follow view with one folded row opened, and Focus view with a thread opened by link.

### Purpose
Let you read one folded agent thread without leaving the page, and explain why a thread is on screen when your Focus view would have hidden it.

### UX reasoning
Reading one folded thread should not change the whole view. A folded line opens in place and Fold closes it; only that line opens. When you arrive through a link to a thread your Focus view hides, the page shows it anyway with one line, "Shown because you opened a link", and a way back to your view. Under Follow nothing is hidden, so the line never appears.

### UI reasoning
An opened thread uses the same card as any thread, so there is no second style to learn. The link row is a tinted one-line strip above the thread, in the brand colour at low weight, so it informs without competing. Labels "Agents only" and "Without me" stay on every folded line.

### Ergonomics
Folded rows are 40 px on desktop and 48 px on phone; Fold and Back are 44 px on phone. Tab walks folded rows in reading order; Enter opens or folds one. Esc returns to B-a.

### States
Row folded, row opened, link row shown (Focus override only), hidden-threads row. Loading, error and offline are in B-f.

### Motion
A folded row opens in 150 ms with a chevron turn (0 to 90 degrees, 120 ms). The link row appears with the page, with no motion of its own. With reduced motion, the row swaps at once.

### Accessibility
Folded rows are buttons with `aria-expanded`. The link row is `role=status`, so a screen reader hears why the thread is shown. The thread card is a labelled section. Focus stays on the row that was opened, and returns to it after Fold.
