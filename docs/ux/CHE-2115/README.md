## Self-mention chip: mobile and @all (B1)

Same solid chip as CHE-2167 (`--brand` fill, `--background` text), extended to `@all` on web/desktop and to the viewer's mentions on mobile.

- Web/desktop: `@all` renders `mention mention-self` for a signed-in viewer (`packages/views/rich-content/rich-content.tsx`). Logged out: plain mention.
- Mobile: `lib/markdown/self-mention.ts` builds `linkVariants` for `^mention://member/<viewerId>$` and `^mention://all/`; `useMarkdownStyle` passes them with the auth user id. Known gap: enriched-markdown link variants cannot set font weight, so mobile has no bold.

### Test-first receipt

- Web red: [test-red-web.txt](test-red-web.txt), `4 failed | 2 passed (6)` (`@all` lacks `mention-self` / selector match). Green: [test-green-web.txt](test-green-web.txt), `6 passed (6)`.
- Mobile red: [test-red-mobile.txt](test-red-mobile.txt), `2 failed | 6 passed (8)` against a stub returning `{}`. Green: [test-green-mobile.txt](test-green-mobile.txt), `8 passed (8)`.

Commands: `pnpm --filter @multica/views exec vitest run rich-content/self-mention.test.tsx` and, in `apps/mobile`, `pnpm exec vitest run lib/markdown/self-mention.test.ts`.

Also run: mobile typecheck, lint, full test (40 files, 266 tests); views typecheck, eslint `rich-content`, `rich-content` suite (14 files, 126 tests). All pass.

### Screenshots (After)

Real `RichContent` HTML with production tokens, X99 Chromium, synthetic mentions. One self mention and `@all` are chips; `@Other` and `@Agent` unchanged.

| Theme | 390 | 1440 |
| --- | --- | --- |
| Light | [png](mentions-light-390-after.png) | [png](mentions-light-1440-after.png) |
| Dark | [png](mentions-dark-390-after.png) | [png](mentions-dark-1440-after.png) |

Computed colors match CHE-2167 (4.58:1 light, 5.80:1 dark).

**NOT-RUN: native mobile screenshots (light + dark).** No iOS/Android dev build was made on X99; enriched-markdown renders natively, so only the style config is unit-tested. Needs a device/emulator read-back before C00/mobile release sign-off.
