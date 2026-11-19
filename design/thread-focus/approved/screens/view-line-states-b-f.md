## View line states (B-f) · doc — direction B, frame `HHAmeNMDw9` `updatedAt` `2026-11-16T14:41:20.320Z`

Copied unchanged from doc frame `14mFx77nWp` (`updatedAt` `2026-11-18T15:15:31.875Z`). Frame B-f, desktop 1440 and phone 390. Four states: loading, empty (nothing waits), relation error, disconnected.

### Purpose
Show what the view line and the list do when data is loading, when nothing waits on you, when the relation that says which threads involve you fails, and when you are offline.

### UX reasoning
Every state names what happened and gives one way out. Loading says "Loading threads…" and keeps the layout. Empty is good news: "Nothing waits on you", with a Show all button. On a relation error the page shows all 9 threads in time order, so nothing is hidden by mistake, and offers Retry and Use All. Offline keeps a read-only copy, shows the last sync time in ICT, and says picks and replies wait until you are back.

### UI reasoning
The same view line and the same message block are used in each state, so the page does not change shape. Error uses the error colour with a warning icon and a text title; offline is neutral with an off icon. Skeleton rows match the thread card size.

### Ergonomics
Phone buttons are 44 px, full width, stacked in the error state with the primary action first. The way out is always in the first screen of the list, no scroll needed.

### States
This frame is the states set. Default, popover and overrides are in B-a to B-e.

### Motion
Loading uses a 1200 ms shimmer on the skeleton rows, looping. Other states swap with the 150 ms dissolve. With reduced motion, the skeleton is static and swaps are instant.

### Accessibility
Loading sets `aria-busy` on the view line. Error and offline use `role=alert`, so they are announced at once. Icons are decorative and every message has text. Actions are buttons with a visible focus ring.
