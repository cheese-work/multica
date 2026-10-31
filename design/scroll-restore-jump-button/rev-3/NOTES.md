## What changed vs rev-2

- Feedback (x99-codex-luna, signed 20 October 2026 03:24 ICT, comment `01a155db-ee0e-7e8e-825b-a46a362e2467`): "No retrying `aria-busy` frame appears." Each direction now shows load retry in progress and jump retry in progress. Retry becomes a disabled "Retrying…" button, and visible text says "Retry is off until this attempt ends."
- Feedback: "The desktop jump-failure Retry control is 32 px." Desktop Retry is now 36 px in the block and in the strip. Phone and native stay 44 px.
- Feedback: "`DESIGN.md` also lists read-only and conflict states, which these directions do not show." Read-only and conflict frames added from current code (table below).
- Feedback: C "assumes an opaque surface rather than a variable underlay." C now has an opaque 25 px `--surface` backing under the arrow. Contrast is measured on pixels rendered over three real underlays (below).
- B gets a 2 px `--page-canvas` ring. Without it, B measured 2.56:1 against a light user bubble in dark mode.

## Directions

- A — Neutral raised disc: surface-raised fill, `--muted-foreground` ring, menu shadow.
- B — Brand-filled disc with floating shadow and a canvas ring. Highest contrast; competes with the composer send button.
- C — Translucent blurred halo around an opaque icon backing, muted arrow, `--muted-foreground` ring, no shadow. Lowest emphasis.

## Contrast over real underlays (rendered pixels)

`contrast-check.html` draws each button over the three surfaces it can sit on: page canvas, assistant bubble (`--muted`) and user bubble (`--primary`), in light and dark. Chrome rendered it at 1 px per CSS px (`contrast-check.png`). A script sampled pixels and applied the WCAG ratio (`contrast-check.json`, 18 rows).

- Icon: the arrow pixel with the most contrast against the backing pixel.
- Silhouette: the best of halo against underlay, border against underlay, or (when the halo contrasts 3:1 with it) the canvas ring against underlay.

| Dir | Theme | Icon vs backing | Lowest silhouette | Underlay of the lowest |
| --- | --- | --- | --- | --- |
| A | light | 19.90 | 4.92 | assistant bubble |
| A | dark | 15.93 | 5.21 | assistant bubble |
| B | light | 4.54 | 4.32 | assistant bubble |
| B | dark | 3.11 | 4.58 | assistant bubble |
| C | light | 5.91 | 4.85 | assistant bubble |
| C | dark | 6.75 | 5.21 | assistant bubble |

All 18 rows are at or above 3:1. B in dark mode on a user bubble is 14.85:1 with the ring and 2.56:1 without it. The B dark icon (3.11:1) passes with little margin. Check it again in the build.

Limits: variable content here means three flat surfaces. A bubble with an image, an anchor outline or a code chip under the button is not sampled. The blur is drawn by Chrome, not by native. The rev-2 token calculation for C assumed an opaque surface; this rendered check replaces it.

## Retry in progress

- Load retry: the block keeps its heading, gets `aria-busy="true"`, and shows the skeleton under it. Retry is a disabled button with a rotating arc and "Retrying…". Visible text: "Retry is off until this attempt ends." No jump button.
- Jump retry: the strip gets `aria-busy="true"` and the same disabled Retry and reason. The jump button gets `aria-busy="true"` and `aria-disabled="true"`, with the arc in place of the arrow. It cannot hold focus while disabled, which matches the `DESIGN.md` rule.
- The arc rotates in the build and is static in the mockup. Under `prefers-reduced-motion` it does not rotate.

## Read-only and conflict (`DESIGN.md` required states)

| State | Web | Native |
| --- | --- | --- |
| Read-only (member) | Applicable. Three causes exist: agent archived, access revoked, session archived. Frames show the first two. The third is the same pattern with the placeholder "This session is archived". Sources: `packages/views/chat/components/archived-agent-banner.tsx`, `agent-access-revoked-banner.tsx`, `packages/views/locales/en/chat.json` (`archived_agent_banner`, `agent_access_revoked_banner`, `placeholder_*`), `chat-window.tsx:1032-1040` (`disabled`). The transcript stays readable, so the jump button stays. | Applicable. `apps/mobile/components/chat/chat-composer.tsx` has `disabled` and `disabledReason`. `apps/mobile/app/(app)/[workspace]/(tabs)/chat.tsx` sets them for an archived session ("This chat is archived", `apps/mobile/locales/en/chat.json`). There is no banner. The frame shows the reason in the composer. |
| Conflict (409, edits kept) | Applicable. The server answers a send during compaction with 409 and code `chat_compacting` (`server/internal/handler/chat_auto_compact_test.go`, "send while compaction is …"). The client shows the toast `compacting_toast` (`use-chat-controller.ts:588-592`) and the composer is disabled with the "Compacting…" placeholder (`chat-window.tsx:1038-1040`). A failed send restores the user's own text (`use-chat-draft-restore.ts`, header comment). The frame shows the toast and the kept draft. | Not applicable today. Mobile chat code has no compacting state and no `chat_compacting` handling (`grep -i compact` over the mobile chat files finds only a layout prop in `chat-message-list.tsx`, unrelated to compaction). No frame drawn. For independent reviewer disposition. |

Other 409 responses in the repo (wakeup, pins, chat continue) belong to other pages, not the DM screen. Toast position is owned by `<Toaster />`. This design does not set it. The build checks that the toast does not cover the jump button.

## Baseline capture (current web desktop and phone): NOT-CAPTURED

Unchanged from rev-2. The chat page needs a signed-in session that X99 does not have. No session was created and no credential moved. Probes and the two options to close it are in `rev-2/NOTES.md`. Sol routes baseline access separately.

## Verification limits

- Static HTML from copied tokens. Not rendered by React, Virtuoso, FlashList or React Native. Fonts are the browser system font.
- Native frames reuse the web token values for dark mode and draw the status bar and home indicator as simple shapes.
- The "anchor" frames outline the anchor message and the saved item label. That outline is a mockup annotation, not UI.
- Error, banner and toast copy come from current locale files where one exists. Retrying and disabled-reason copy are new placeholders.
- Contrast was measured on the static mockup, not on product render.
- Keyboard behavior, scroll motion and reduced-motion behavior are described in the page text and not drawn.
- The six direction PNGs are palette-reduced to 256 colours so the upload fits. `contrast-check.png` is lossless and is the file the ratios were measured on.
- `visual diff: NOT-RUN`.
