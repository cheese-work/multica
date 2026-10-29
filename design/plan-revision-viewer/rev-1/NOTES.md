## What changed vs the current viewer

- The current shared Markdown viewer has no revision controls. A adds a compact comparison bar; B adds a revision rail.
- Both directions keep Diff off on open. Diff on defaults to the previous available revision and offers earlier bases.
- Both directions expose loading, first/no earlier revision, empty file, failed base load with Retry, offline cached viewing, and duplicate-base selection.
- This answers Cheese's request: "các revision có thể diff view on/off khi mở 1 file revision ra và chọn được revision để diff".
- Live before capture: NOT-CAPTURED. The isolated X99 browser reaches the sign-in page. No authenticated session is borrowed or provisioned.

## Directions

- A — Compact compare bar and unified Markdown-source diff. The reviewer reads one column in both wide and narrow layouts.
- B — Revision rail and aligned base/current columns at wide widths. Narrow layouts use the same single-column diff as A.

Independent UX scores are pending. No direction is selected or approved. These files are for agent UX scoring, not a direction-selection request to Cheese.

## Evidence and limits

All PNGs are static design previews captured on X99. The web and desktop labels describe mock chrome, not running product clients. The base-picker PNG uses an expanded native listbox to illustrate the options, not an Electron or browser popup capture.

- `A-web-diff-off-1440.png`, `A-web-diff-on-1440.png`, `A-web-base-picker-1440.png`: wide web mockups. Equivalent B files show the second direction.
- `A-desktop-diff-off-1440.png`, `A-desktop-diff-on-1440.png`, `A-desktop-base-picker-1440.png`: wide desktop mockups. Equivalent B files show the second direction.
- `A-diff-off-390.png`, `A-diff-on-390.png`: narrow mockups. Equivalent B files show the second direction.
- `A-{loading,first,empty,error,offline,ambiguous}-1440.png`: required-state mockups. Equivalent B files show the second direction.

The embedded Noto font includes its original license in `font-LICENSE.txt`. The mockups make no external network requests. UI semantic values come from the product light-theme tokens.

The mock diff contains fixed fixture changes, not a product diff algorithm. Run `node design/plan-revision-viewer/rev-1/check.mjs` from the repository root to check exact before/after reconstruction and the displayed line counts for all four fixture base IDs.

`validation.json` records 23 browser interaction/state checks, 28 captures, no browser JavaScript errors, and Mermaid parse/render bounds. The checks include keyboard toggle and focus retention, alternate-base selection, error retry, duplicate-base disambiguation, and no page-wide horizontal overflow at 390 px.

The browser harness and its raw output are run evidence, not product tests. No behavioral red/green for the issue's revision lookup or diff is claimed. Source-issue permission equivalence, real web/desktop integration, native desktop routing, screen-reader behavior, and the full Keep / Change / Remove check remain NOT-RUN.

## Next gate

x99-claude-sonnet selects an independent different-lab UX reviewer for this exact revision commit. Score both directions on clarity, hierarchy, states, consistency, accessibility, and density before asking Cheese to select a direction.

UI implementation starts only after Cheese merges the selected design revision, unless Cheese records a design waiver. The later implementation PR must supply the observed product red/green and the real web/desktop captures required by CHE-2124.
