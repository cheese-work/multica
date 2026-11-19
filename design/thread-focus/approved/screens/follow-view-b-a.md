## Follow view (B-a) · doc — direction B, frame `iaeZnixDyD` `updatedAt` `2026-11-18T15:09:37.422Z`

Copied unchanged from doc frame `W8qKsyBUvQ` (`updatedAt` `2026-11-18T15:13:48.780Z`). Frame B-a, desktop 1440 and phone 390. Direction B, picked by Cheese. Follow is the default.

### Purpose
Open an issue with 9 threads and see at once which ones need you. The page says it in one line, folds the 5 threads that do not involve you, and keeps the 2 that wait on you at the top.

### UX reasoning
The old page showed every thread at equal weight, so a decision hid among agent chatter. Follow keeps every thread in reach and changes only what is folded. The view line says what you see ("Following all 9 threads. 4 with you, 5 folded"), so nothing disappears without a sign. The Next waiting pill takes you to the next decision in one step. A dot with "1 new" marks a thread with comments you have not seen; it is the only unread signal.

### UI reasoning
One quiet line replaces a toolbar of presets, so the thread list stays the focus. Waiting threads carry a filled "Waiting on you" chip in the brand colour; folded rows are muted and one line high. Colours, radius and spacing come from the CHE-6385 tokens frame. The view line, pill, fold row, decision card placement, minimap and unread dot are on the Components page.

### Ergonomics
Phone targets are 44 px or higher; folded rows are 48 px. The pill sits at the bottom right, inside thumb reach with one hand. Desktop controls are 28 px high with a 2 px focus ring, plus keys `]` and `[` for next and previous waiting thread.

### States
Default (this frame), view popover open (B-c), a folded row opened in place (B-b), Focus or All for this issue (B-d, B-e), and loading, nothing waiting, relation error and offline (B-f). Hover and focus are shown on the Components page.

### Motion
View change dissolves in 150 ms. Jump to a waiting thread scrolls in 250 ms and tints the thread for 1200 ms. A fold row opens in 150 ms. The unread dot fades in 120 ms when the thread has been in view for 1 s. With reduced motion, each change is an instant swap; the tint still holds 1200 ms. Specs are on the Motion page.

### Accessibility
The view line is `role=status`. Every row and control is a real button, reachable by Tab, with a visible focus ring. Folded rows use `aria-expanded`. The pill has an accessible name that includes its position, "1 of 2". The unread dot has a text label "1 new", so it does not rely on colour. Contrast of text on its background is at least 4.5:1 (see the tokens frame).
