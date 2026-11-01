## What changed vs rev-1

- Feedback (x99-codex-luna, signed 20 Oct 2026 02:20 ICT, comment `01a155a9-57f9-78f0-854e-cd29d4afc6cd`): "The mockups omit the empty and error/retry states required by `DESIGN.md`." Each direction now shows empty, load failed with Retry, and jump failed with a Retry strip. Desktop, web phone and native phone.
- Feedback: "Notes report boundary contrast below 3:1" (A, C). A and C button boundary now uses `--muted-foreground`. Measured below.
- Focus ring uses `--foreground`. The `--ring` token measured 2.54:1 on the light page, below 3:1. This affects all three directions.
- B is unchanged: the brand fill already passes. Its remaining problem (competes with the composer send button) is a design choice for Cheese, not a defect.
- Baseline capture: blocked. See "Baseline capture" below.

## Directions

- A — Neutral raised disc: surface-raised fill, `--muted-foreground` ring, menu shadow.
- B — Brand-filled disc with floating shadow. Highest contrast; competes with the composer send button.
- C — Translucent blurred disc, muted arrow, `--muted-foreground` ring, no shadow. Lowest emphasis.

## Contrast measurements (WCAG 2.x ratio, token values from `packages/ui/styles/tokens.css`, button boundary against `--page-canvas`)

| Item | Light | Dark | Needs |
| --- | --- | --- | --- |
| rev-1 A/C boundary (`--surface-border`) | 1.23 | 2.78 | 3.0 |
| rev-2 A/C boundary (`--muted-foreground`) | 5.70 | 7.15 | 3.0 |
| rev-2 A/C boundary against a message bubble (`--muted`) | 5.36 | 5.66 | 3.0 |
| A icon (`--foreground`) on `--surface-raised` | 19.89 | 15.98 | 3.0 |
| C arrow (`--muted-foreground`) on the translucent fill | 5.84 | 6.85 | 3.0 |
| B boundary (`--brand`) | 4.58 | 5.78 | 3.0 |
| B icon (`--brand-foreground`) on `--brand` | 4.54 | 3.12 | 3.0 |
| rev-1 focus ring (`--ring`) | 2.54 | 3.90 | 3.0 |
| rev-2 focus ring (`--foreground`) | 19.22 | 18.02 | 3.0 |

B dark icon (3.12:1) passes with little margin. Check it again in the build.

## Empty and error states (all directions, same copy)

- Empty DM: "No messages yet / Send a message to start this DM with {agent}." No jump button, because there is nothing to jump to. Composer stays enabled.
- Load failed: `role="alert"` block, "Couldn’t load messages / Check your connection, then try again." Visible Retry button. No jump button.
- Jump failed (latest page did not load while the reader is scrolled up): the jump button stays; a `role="alert"` strip under the header says "Couldn’t load the latest messages." with Retry. Button and strip retry the same load; the button shows `aria-busy` while it retries.
- Retry is 36 px on desktop, 44 px on a coarse pointer and on native. Error colour is `--destructive`; the text is `--foreground`.

## Baseline capture (current web desktop and phone): not captured

Concrete blocker: the current chat page needs a signed-in Multica session, and X99 has none. Probes run on X99 on 20 Oct 2026:

- `curl https://congvc-c00.taila6fa8a.ts.net:19443/` → 200, title "Multica — Project Management for Human + Agent Teams". The web app answers, but the DM chat page is behind sign-in.
- `curl https://congvc-c00.taila6fa8a.ts.net:19444/health` → 200. That port is the API, not the web app.
- `apps/ui-lab` (the no-login workbench) has no chat scene: `grep -i chat apps/ui-lab/src/catalog.ts` finds nothing.
- Workspace rule: authenticated browser work routes through MBP ownership. No credential was injected into a headless browser.

Options to close it: (1) the owner of an authenticated browser session captures the DM page at 1440 and 390; (2) a build slice adds a chat scene to `apps/ui-lab` and captures it on X99. Option 2 also gives the "before" captures for the UI review pack.

## Verification limits

- Static HTML from copied tokens. Not rendered by React, Virtuoso, FlashList or React Native. Fonts are the browser system font.
- Native frames reuse the web token values for dark mode and draw the status bar and home indicator as simple shapes.
- The "anchor" frames outline the anchor message and the saved item label. That outline is a mockup annotation, not UI.
- Error and disconnected copy is a placeholder; the real banner components are not reused.
- Contrast figures are computed from token values, not measured on a rendered page. The translucent C fill is treated as opaque `--surface` at the measured ratio.
- Scroll motion, keyboard-open layout and reduced-motion behaviour are described in the page text and not drawn.
- `visual diff: NOT-RUN`.
