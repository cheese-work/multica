## Self-mention evidence

Scope: CHE-2113 item A only, web and desktop. Other members, agents and `@all` stay unchanged. Label case stays unchanged.

The assigned specification is `self-mention-highlight-design.md`, attachment `01a176ce-20f6-7465-9f0b-10bc18e348ed`. The attachment records Cheese's approval in DM message `01a176c6-0377-747d-b458-01bfa89d44e8`. No merged design revision is claimed.

### Captures

The PNGs show the real shared `RichContent` renderer and production theme tokens in an X99 Chromium fixture. The fixture uses synthetic mention text, a registered viewer auth store and both document and compact densities. The panels represent issue descriptions, comments and DMs; these are not authenticated screenshots of the complete product routes. No production service was changed.

Before captures use base `76b6e2071e5c4d11cbb9ac6ca057ca1054beae73`. After captures use the implementation in this PR.

| Theme | Width | Before | After |
| --- | --- | --- | --- |
| Light | 1440 | [Before](self-mention-light-1440-before.png) | [After](self-mention-light-1440-after.png) |
| Light | 390 | [Before](self-mention-light-390-before.png) | [After](self-mention-light-390-after.png) |
| Dark | 1440 | [Before](self-mention-dark-1440-before.png) | [After](self-mention-dark-1440-after.png) |
| Dark | 390 | [Before](self-mention-dark-390-before.png) | [After](self-mention-dark-390-after.png) |

All eight PNGs were opened and visually checked. At both widths in both themes, the browser observes 12 mentions and exactly 3 self variants after the change, one per panel. There is no horizontal page overflow and no browser page error.

Computed self styles: weight `700`, radius `6px`, horizontal padding `4px`. The browser rasterizes the computed foreground and opaque highlight colors to sRGB for WCAG contrast calculation: light `16.12:1`, dark `12.61:1`, both above `4.5:1`. Detailed observations are in `browser-before.json` and `browser-after.json`.

Keep: label case, other-member mentions, agent mentions and `@all` are present and retain their existing appearance. Change: only viewer-member mentions get the rounded highlight. Remove: none. Visual diff: NOT-RUN; this is a manual comparison and computed-style check.

### Test-first receipt

Before implementation, run:

```sh
corepack pnpm --filter @multica/views exec vitest run rich-content/self-mention.test.tsx --reporter=verbose
```

Observed behavioral red on the base: `3 failed | 1 passed (4)`. The compact and document cases expect `mention mention-self` but receive `mention`. The auth-switch case expects `mention-self` but receives `mention`.

After implementation, the same command observes `4 passed (4)`. The tests verify self/non-self separation, unchanged case, agent-ID collision, unchanged `@all`, missing current user and reactive viewer changes/logout. The PR body includes the observed output.

The broader `rich-content` suite observes `14 passed` files and `121 passed` tests. Existing jsdom canvas diagnostics remain non-failing. No native-mobile test or Electron-runtime smoke is claimed.

The existing editor table-export fixture initially failed on implementation head `941f098aaddc657c75bed9442a8ca3ff4bc92903`: `Auth store not initialised`. The failure was reproduced locally before the first fixture workaround: `1 failed | 4 passed (5)`.

The guest auth mock has since been removed. The regression test now asserts ordinary mention styling without a registered auth store, while retaining table filtering and export assertions. Before the production correction, the table and self-mention tests together observe `1 failed | 8 passed (9)` on parent `e900b9aaf2bb0333a65c6d42c9266c684fc220ef`. After the correction, the same command observes `9 passed (9)`:

```sh
corepack pnpm --filter @multica/views exec vitest run editor/extensions/table-view.test.tsx rich-content/self-mention.test.tsx --maxWorkers=2 --reporter=verbose
```

The renderer uses a native external-store subscription when auth is registered and otherwise keeps ordinary mention styling. The original self-mention tests are unchanged, including reactive viewer changes and logout. Auth bootstrap remains strict for other consumers.

All four after PNGs were recaptured with the production correction and match the committed PNGs byte for byte. The refreshed browser observations still pass the same mention counts, styles, contrast, overflow and page-error assertions.

The complete local views suite passes with `corepack pnpm --filter @multica/views test --maxWorkers=4`: `545 passed` files and `6883 passed` tests. Views typecheck, scoped ESLint and `git diff --check` also pass. Existing jsdom canvas and navigation diagnostics remain non-failing.
