## Question

`rev-1/B-390.png` wraps "after." onto a second line. `rev-1/product-crosscheck/B-14rem-mobile-short-2-resolved.png` keeps "now and after." on one line. Both show direction B (14 rem, 224 px) at a 390 px window.

## Retained evidence compared

| Factor | Mockup `B.html` | Product harness (`geometry/main.tsx` at source `8187ea0ccf245bfa8dad12b0efe2850896c66968`) |
| --- | --- | --- |
| Prose box | Bubble with a 1 px border and 12 px side padding, inside body padding of 12 px | Plain container with 12 px side padding, no bubble chrome |
| Width for prose at a 390 px window | 340 px | 366 px |
| Font | `system-ui`, which resolves to Noto Sans on X99 (`fc-match system-ui`) | Inter Variable, loaded by the app styles |
| Text size and line height | 14 px, 22.75 px | 14 px, `leading-relaxed` |
| Chip | 224 px wide, margin 2 px each side | 224 px wide, margin 2 px each side (measured 224x22) |

The harness result for B is in `rev-1/product-crosscheck/product-B-14rem.json`: paragraph height 24.8 px, one line, before and after resolution.

## Measurement

`measure-b-wrap.mjs` loads the retained `rev-1/B.html` in headless Chromium at 390 px and changes one factor at a time with injected CSS. Static file only. Output: `b-wrap-measurements.json`.

| Variant | Prose width | Sentence needs | "after." on first line |
| --- | --- | --- | --- |
| As retained (Noto Sans, bubble chrome) | 340 px | 350.9 px | No |
| Bubble chrome removed (Noto Sans) | 366 px | 350.9 px | Yes |
| Inter font, bubble chrome kept | 340 px | 353.0 px | No |
| Bubble chrome removed, Inter | 366 px | 353.0 px | Yes |

"Sentence needs" is the width of "see ", the chip with its margins, and " now and after." on one line.

## Disposition

Observed: the sentence needs about 351 to 353 px. The mockup gives it 340 px and wraps. The harness gives it 366 px and fits, with about 13 px to spare. The font changes the need by about 2 px. The bubble border and padding change the available width by 26 px. So the two images disagree because the prose box differs in width, not because B's cap behaves differently.

Not observed: the width of a real chat bubble or description column in the product. The product route is not admitted for this step. So whether B keeps this sentence on one line in the product is NOT-VERIFIED. It depends on the host width and on the prose around the key. Any longer sentence wraps at 390 px under B as well. Do not promise one-line mobile prose for B.

The retained previews are unchanged. A replacement `B-390.png` that uses the harness width would be a new revision, and a new revision needs a new independent UX score. This step does not make one.

## Commands run

- `node measure-b-wrap.mjs <abs path to rev-1/B.html> <Inter woff2 from @fontsource-variable/inter 5.2.8> b-wrap-measurements.json` on X99, from `apps/ui-lab` of a checkout with `pnpm install --ignore-scripts` done.
- `fc-match system-ui` and `fc-match sans-serif`: both Noto Sans.
