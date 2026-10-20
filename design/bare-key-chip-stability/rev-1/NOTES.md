## What changed vs rev-0

rev-0 is the live behavior at source `9eb6e5a363dd4ef10b3a591b7efc1f89c9588b7a`: a bare key is plain text, then a content-sized chip. The measured widths are 77 px pending and 138 px (short title) or 288 px (long title) resolved. On a 390 px window a medium or long title adds a line (+22.7 px paragraph height).

- Both directions give the pending box and the resolved chip one width, so nothing on the line moves.
- Both directions keep the non-link busy box, truncation, the narrow-container bound and plain-text settled misses.
- Explicit URL and UUID chips are unchanged and content-sized.

## Directions

- A — Coupled cap, 18 rem (288 px). The chip's existing cap. Long titles unchanged. A short title leaves about 150 px of empty space. On a 390 px window the chip sits on its own line, before and after resolution.
- B — Coupled cap, 14 rem (224 px). A short title leaves about 86 px of empty space. A long title truncates about 64 px earlier. On a 390 px window the chip stays on the first line.

Scores: NOT-SCORED. The dev team assigns a cross-lab UX reviewer after this revision exists.

## Evidence and provenance

- Design previews: `A.html`, `B.html`, with `A-1440.png`, `A-390.png`, `B-1440.png`, `B-390.png`. Static HTML with token values copied from `packages/ui/styles/tokens.css` and chip geometry copied from `issue-chip.tsx`. They are isolated design previews, not the product. `build.py` generates both HTML files.
- Capture: on X99, `google-chrome --headless=new --disable-gpu --hide-scrollbars --window-size=1440,730 --screenshot=<X>-1440.png file://$PWD/<X>.html` and the same with `--window-size=390,960` for `<X>-390.png`.
- Product cross-check, `product-crosscheck/`: production `RichContent`, chip and styles in headless Chromium, with a stubbed API, from the geometry harness at source `8187ea0ccf245bfa8dad12b0efe2850896c66968` (branch `author/CHE-1829/ec5b-width-stability`). `capture-product.mjs` is the driver. Direction A is that source unchanged. Direction B is the same source with the cap changed to `w-[min(14rem,100%)]` in the working tree only. That change is not committed anywhere. This is not the supported X99 product route (CHE-1832 owns it), and it is not independent verification.
- Measured, pending then resolved (box width x height, paragraph height). A, 1280 px window: 288x22 then 288x22, paragraph 24.8 px both. A, 390 px window: 288x22 then 288x22, paragraph 47.5 px both. B, 1280 px window: 224x22 then 224x22, paragraph 24.8 px both. B, 390 px window: 224x22 then 224x22, paragraph 24.8 px both. Short and long titles gave the same numbers.
- Not shown: dark theme, hover, keyboard focus. The mockups use the light theme only.
