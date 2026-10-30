## What changed vs rev-1

- Direction A now puts file identity and all comparison controls in one row, with less document padding. Cheese: "Thu hẹp phần này hơn nữa bằng cách thay vì 2 dòng thì gộp vào 1 dòng thôi".
- A now has Unify and Split modes in the same toolbar; B is dropped. Cheese: "Thêm nữa là có 2 mode diff là unify và split; unify thì giống A, còn split thì giống B". Split pairs share row heights, including wrapped lines and insertion/deletion spacers, without a revision rail.
- The 339/342-line fixture has three changed blocks (+5/-2), three lines of context, collapsed unchanged ranges, and previous/next navigation. Three-digit gutters retain sufficient width at 390 px.
- Every base option shows the revision, ICT upload time, uploader and filename; duplicates expose immutable IDs. Diff reads off and counts disappear whenever comparison content is absent, including loading, first revision, error, offline, ambiguous base and identical source.
- `A.html` embeds all CSS, JavaScript and its licensed Noto font. An isolated copy is checked without sibling files or external requests. Static PNGs cover the selected direction and both layouts.

## Direction

- A — One-row revision viewer, compact rendered document, unified or row-aligned split source diff, collapsed context and change navigation.

Cheese selected A on 27 Oct 2026 in comment `01a17b0f-ac0d-759e-be25-27f067fbc0a2`. Rev-2 answers the five findings in the x99-claude-opus verdict `01a1777a-44ca-76d3-a27e-d698883f47b0` at rev-1 commit `9ddb71a5ea087a0251ee59b947ea6784ef8df288`. Fresh independent UX scoring is pending with x99-claude-sonnet as reviewer-selection owner. This is not a self-score or a new selection request.

## Evidence and limits

The PNGs are static HTML design previews captured on X99. The web and desktop labels are simulated platform chrome, not real browser-app or Electron integration evidence. The actual CSS/JavaScript/font render from `A.html` alone. The original font license is in `font-LICENSE.txt`.

The desktop-width toolbar is 48 px high; the narrow toolbar is 52 px high with local horizontal scrolling. All controls stay in the same row. The mock platform chrome is 36 px high. Document top padding is 16 px wide and 12 px narrow. Split keeps a minimum 740 px comparison width on narrow screens and scrolls locally; it never silently switches to unified mode.

`check.mjs` checks the self-contained artifact, full fixture reconstruction, exact +5/-2 counts, context grouping, numeric line coverage and empty/identical fixture behavior. It is not a production diff implementation. Run `node design/plan-revision-viewer/rev-2/check.mjs` from the repository root.

`validation.json` records 20 observed browser checks and 22 capture filenames. The browser harness verifies toolbar geometry, focus and keyboard controls, base selection, context expansion, next/previous changes, state consistency, paired split-row alignment, 390 px gutters and isolated-file rendering. Isolated-file PNGs for Diff off, Unify and Split match the served PNGs byte for byte. The raw harness is separate run evidence.

Product behavioral red/green, permission equivalence, actual web/desktop captures, native desktop routing and screen-reader verification remain NOT-RUN. The attempted fixture check before `A.html` existed failed with ENOENT; this is not behavioral red evidence. The isolated live baseline remains NOT-CAPTURED because X99 reaches sign-in; no authenticated session was borrowed. Automated product visual diff and real GitHub Mermaid rendering remain NOT-RUN.

Keep / Change / Remove, design-only: keep rendered Markdown, download, focus visibility and the source-issue boundary proposal; change toolbar density, diff layouts, picker labels and unchanged context; remove direction B's revision rail and contradictory active-diff states. This does not claim a product baseline comparison.

## Next gate

The PR stays draft. Do not merge. x99-claude-sonnet dispatches fresh independent different-lab UX scoring on the returned head and then presents rev-2 to Cheese. Implementation starts only after Cheese merges the selected design revision, unless Cheese records a design waiver. Cheese's implementation approval carries forward.
