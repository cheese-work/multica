## Current-user mention: solid fill

Cheese's direction is recorded in the CHE-2167 issue body: solid, high-contrast fill with background-coloured text. No separate design round is needed.

The only production change is the existing `.mention.mention-self` rule in `packages/views/rich-content/rich-content.css`: `--brand` fill and `--background` text replace the faint warning tint. The shared renderer serves issue descriptions, comments and every chat message in web and desktop. No platform-specific fork or identity logic changes.

### Test-first receipt

Parent: `dcfa7c9484f042cacd40a269dcee81725bc82187`.

The new render cases load the production CSS into the DOM. Both densities assert that the viewer's member mention matches the self selector and gets the new tokens. Other members, agents with the same ID, and `@all` do not match that selector and retain the ordinary mention class.

Command, run before and after the production edit:

```sh
pnpm --filter @multica/views exec vitest run rich-content/self-mention.test.tsx --maxWorkers=2 --reporter=verbose
```

Observed red: `2 failed | 4 passed (6)`. Both new cases expect `var(--brand)` but receive `color-mix(in oklab, var(--warning) 25%, var(--background))`.

Observed green: `6 passed (6)`. Raw output is preserved in [test-red.txt](test-red.txt) and [test-green.txt](test-green.txt). An earlier test-loading error was corrected before this behavioral red; it is not counted as red evidence. Log start times use ICT.

Additional verification:

- `pnpm --filter @multica/views exec vitest run rich-content --maxWorkers=2 --reporter=verbose`: 14 files, 123 tests pass.
- `pnpm --filter @multica/views typecheck`: pass.
- `pnpm --filter @multica/views exec eslint rich-content/self-mention.test.tsx`: pass.
- `git diff --check`: pass.

### UI review pack

Preview: not available. Design: the recorded CHE-2167 direction above. Sheet: [`../CHE-2167.yml`](../CHE-2167.yml). Visual diff: NOT-RUN; manual before/after comparison and computed-style checks only.

All screenshots include both an issue comment and a DM. Captures use the real shared `RichContent` component, production CSS and theme tokens, a registered synthetic viewer, and synthetic mention text. They are not authenticated full-product route captures or an Electron-runtime smoke test. Native mobile is outside this issue's web/desktop scope.

Capture host/toolchain: X99, Node `22.23.1`, pnpm `10.28.2`, Chromium `145.0.7632.6`, device scale factor 1. Before captures use the parent CSS; after captures use this PR's two-line production change.

| Surfaces | Theme | Width | Before | After |
| --- | --- | --- | --- | --- |
| Comment + DM | Light | 1440 | [Before](mentions-light-1440-before.png) | [After](mentions-light-1440-after.png) |
| Comment + DM | Dark | 1440 | [Before](mentions-dark-1440-before.png) | [After](mentions-dark-1440-after.png) |
| Comment + DM | Light | 390 | [Before](mentions-light-390-before.png) | [After](mentions-light-390-after.png) |
| Comment + DM | Dark | 390 | [Before](mentions-dark-390-before.png) | [After](mentions-dark-390-after.png) |

All eight PNGs were opened and visually checked. Both surfaces have one self mention and three unchanged non-self mentions in every capture. No horizontal overflow or browser page errors occurred.

Chromium's computed foreground and opaque fill were rasterized to sRGB before calculating WCAG contrast. Both surfaces and widths measure **4.58:1 light** and **5.80:1 dark**, exceeding 4.5:1. See [browser-before.json](browser-before.json) and [browser-after.json](browser-after.json).

Keep / Change / Remove check:

- Keep: `member-mention` elements, label case, other members, agents and `@all` remain present. Non-self computed color, background and weight are identical before and after.
- Change: only the current-user chip has solid brand fill and background-coloured text. Weight 700, radius 6px and horizontal padding 4px remain unchanged.
- Remove: none.
