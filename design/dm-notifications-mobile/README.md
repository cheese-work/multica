## Request

Cheese, in the CHE-1789 request (verbatim): "Feat: DM notifications", "Feat: opt in inbox noti and DM noti in setting". Approved behavior: `docs/designs/dm-search-notifications-push.md` at `62be22982301893996ef88fa5fb1b964fe99021e` (PR #271), section "N5: mobile settings parity and DM inbox rows". Approval 2026-10-16 in a web DM (D1 = A, D2 = A, D3 = B, D4 = A).

- Issue: [CHE-1797](mention://issue/01a140fb-3453-7e77-b963-7e9c877309d3)
- Pages in scope: Settings → Notifications (`apps/mobile/app/(app)/[workspace]/more/settings/notifications.tsx`), Inbox tab (`apps/mobile/app/(app)/[workspace]/(tabs)/inbox.tsx`, `components/inbox/inbox-row.tsx`)
- Tokens: `apps/mobile/global.css`.
- Status: selected rev-1 direction A — approved when PR #277 merges. Selection: Cheese, CHE-1811 comment `01a14243-3674-7796-aaf2-b9354a94065e` (16 October 2026, 08:04 ICT). UX scores: comment `01a141c0-6442-7ea5-a127-0707163b62b5`.

## Settled by selecting A as drawn

1. Inbox filter: chips under the header. Not a segmented control (B) and not a header-menu option (C).
2. The "Inbox events" rename is B only. A keeps "Inbox notifications".
3. With Inbox In-app off, the event rows are dimmed and locked, as in A's mockup. The server keeps the group mutes.
4. Offline: switches locked under a warning banner, as in A's mockup.
5. DM rows show the DM title. The N2 head `c708c74c53534c153b6fd4ffc7e8294e976212c0` stores `session.Title` on the inbox item, so mobile can render it. The no-message-text rule in the design doc covers push payloads, banners and logs.

## Carried into the admit sheet

- Accessibility scored 4 of 10 on every direction: switches need associated labels, and their 32×18 px targets and the chips need native validation.
- Current screens are `NOT-CAPTURED`, native behavior is `NOT-RUN`. Selection waives neither.
