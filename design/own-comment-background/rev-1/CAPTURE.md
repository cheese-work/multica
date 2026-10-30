## Author capture receipt

- Host: `congvc-X99`; Codex CLI `0.155.1`, workspace-write; Google Chrome `150.0.7871.181`.
- Source: this revision's self-contained `A.html`, `B.html`, `C.html`. Synthetic issue comments, not live application data.
- Method: isolated headless Chrome profile, DevTools Protocol; 1440 × 900 and 390 × 844 CSS viewports, device scale 1; full-page PNG captures.
- Every direction has `light` and `dark` captures at both widths. Desktop PNGs are 1440 × 2324; phone PNGs are 390 × 2932.
- Browser checks: 11 comment elements including one hidden in a folded thread; no page-level horizontal overflow; no external subresource requests.
- All 12 PNGs opened and checked as nonblank. The designer inspected each direction at desktop and phone widths in both themes.

## Measured contrast

Canvas-converted computed foreground and background colours, sRGB relative luminance. The body-text measurement uses a viewer-authored comment without the temporary jump-target override. It is not a whole-page accessibility audit.

| Theme | Own-comment body text | Amber self-mention text | Directions / widths |
| --- | --- | --- | --- |
| Light | 17.97:1 | 16.12:1 | A, B, C / 1440 and 390 |
| Dark | 14.19:1 | 12.61:1 | A, B, C / 1440 and 390 |

The README's Mermaid flow parsed and rendered successfully with the repository-pinned Mermaid `11.14.0` in headless Chrome. No SVG was saved, attached or used to create the PNGs. The comment mockups show Mermaid source only; real rich-content rendering remains an implementation check.

## Limits and next gate

- Live signed-in before capture: NOT-CAPTURED. No authorized X99 browser session was available; no credentials or browser sessions were imported.
- Native mobile, sticky-header scroll behavior, screen readers and actual auth changes: NOT-RUN.
- Application behavioral red/green: NOT-RUN. This is a design-only revision; production code is unchanged. Observe red and green during the later implementation run after design approval.
- UX scores, Cheese's selection and design merge: pending. Keep the PR draft. Neither selection nor these captures permit merging implementation or deploying C00.
