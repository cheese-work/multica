## Focus and All views (B-d, B-e) · doc — direction B, frame `L_jGz1tA6p` and `NX-cgUem2y` `updatedAt` `2026-11-18T15:32:37.474Z and `2026-11-16T20:22:22.461Z`

Copied unchanged from doc frame `VkrFK83ytF` (`updatedAt` `2026-11-18T15:32:34.473Z`). Frames B-d (Focus for this issue) and B-e (All for this issue), desktop 1440 and phone 390.

### Purpose
Show the two per-issue overrides. Focus keeps only the 4 threads with you and hides 5. All lists every thread in time order with nothing folded.

### UX reasoning
An override is easy to forget, so the line names it ("Focus: 4 threads with you. 5 hidden. Your default is Follow"), shows a "This issue only" chip and offers "Reset to my default". Hidden threads are never lost: one dashed row at the bottom opens them. Waiting threads stay visible in Focus even if an agent opened them. In All the order is time, so the pill is the way to the 2 waiting threads.

### UI reasoning
The view line is the same component in all three presets, with only its text and the chip changing, so the page does not shift. The hidden-threads row is dashed and muted so it reads as an exit, not as a thread.

### Ergonomics
Reset to my default is a full-width 44 px button on phone. The pill has a Previous and a Next button, 44 px each. Desktop keys: `]` next waiting, `[` previous, Esc back to the default view.

### States
Focus with hidden row (B-d), Focus with the 5 hidden threads shown in place (B-d2, frame SrxqJi90Xa), All with all threads, Reset pressed (returns to B-a). "Show 5 threads without you" opens B-d2 and keeps the Focus view; "Hide 5 threads" or Esc returns to B-d. Expanded is local UI state and is not saved. Loading, error and offline are in B-f; on a relation error the page falls back to All so nothing is hidden.

### Motion
Changing view dissolves the list in 150 ms. A jump to a waiting thread scrolls in 250 ms and tints the thread for 1200 ms. No height or width animates, so no thread moves under the pointer. With reduced motion, swaps are instant.

### Accessibility
The view line is `role=status`, so the change is announced. The hidden row is a button with a text count. Waiting is shown by a text chip, not by colour alone. Every control is reachable by Tab with a visible focus ring.
