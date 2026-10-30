## What changed vs rev-2

Format-only revision. The direction HTML is byte-identical to `rev-2` (`A.html`, `B.html`, `C.html` compare equal with `cmp`). `rev-1` and `rev-2` are not edited. No direction is selected or approved.

- Evidence PNGs. Each direction is rendered at 1440 px and 390 px from the unchanged HTML with Playwright (Chromium, full page, device scale 1). A full-page PNG is over the 300 KB image budget, so each render is cut into 2 lossless tiles at a blank row: `A-1440-1of2.png`, `A-1440-2of2.png`, `A-390-1of2.png`, and so on for B and C. Putting the tiles back together reproduces the full page exactly (checked pixel by pixel). Tiles are compressed with oxipng and zopfli (lossless).
- Live captures as PNG, in `rev-3/live/`: `issue-live-1440.png`, `issue-live-390.png`, `dm-live-1440.png`, `dm-live-390.png`. Taken on X99 from the running web app at the PR 315 head `fc9824eac1ac0cdc4212b61ed016a6f7921dd537`. That head has no UI change, so they show the current UI. The data is seeded test data. The Next.js development badge is hidden by an injected style in the capture script; nothing else is altered.
- The `rev-2` WebP images stay as posted. They are previews, and they are not the evidence images.

## Directions

- A — Quiet quote: unchanged from rev-2.
- B — Receipt card: unchanged from rev-2.
- C — Timeline row: unchanged from rev-2.

## Limits

- Static HTML. The flash is shown at its peak frame. Keyboard and screen-reader behaviour is NOT-RUN.
- The live captures show only the current UI. The new cards exist only as mockups.
