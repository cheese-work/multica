## What changed vs rev-0

There is no earlier revision. This is the first revision, compared against the current live viewer (Cheese's screenshot on CHE-2123: title, zoom out, 86 %, zoom in, fit, fullscreen on the left; source, copy, download, close on the right).

- Both directions add one icon button, "Open in new tab" (the `ExternalLink` icon and `attachment.open_in_new_tab` copy the attachment viewer already uses; the desktop app says "Open in new window").
- Both pages keep the viewer's own toolbar and stage and nothing else. The page has no Close button, because the tab or window closes natively. Copy link replaces the open button.
- States shown: loading (toolbar disabled), not found or no access (standard page, no toolbar, no media, no hint which case), load error with Retry, offline with the last loaded copy.
- Phone width: the toolbar wraps to two rows; the actions beyond zoom and fit go behind a More menu with 40 px targets.

## Directions

- A — Media only, same bar. "Open in new tab" in the right group beside Download, before Close. The page header is only the title and the toolbar.
- B — Media plus a source line. "Open in new tab" in the left group beside Fullscreen. The page header adds one line, "From CHE-2112 · comment · #n", a link back to the source. The stage stays media-only.

No direction is selected or approved. Only Cheese selects, and approves by merging this PR.

## Capture

X99, 26 October 2026. Static HTML, no network. Google Chrome headless, window 1440 px and 390 px wide, trimmed to the content and palette-quantized (64 colours) with Pillow. All four PNGs are 79–96 KB, under the 300 KB budget. Each was opened and read: not blank (71–81 % non-background pixels). Phone panes in the 1440 capture are laid out at 390 px with container queries.

## Rubric scores

NOT-SCORED. A reviewer from a different lab than the designer (Anthropic) scores each direction of this exact revision before Cheese sees it.
