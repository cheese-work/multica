## Request

Cheese, in the CHE-1789 request (verbatim): "Feat: DM notifications", "Feat: opt in inbox noti and DM noti in setting". Approved behavior: `docs/designs/dm-search-notifications-push.md` at `62be22982301893996ef88fa5fb1b964fe99021e` (PR #271), section "N5: mobile settings parity and DM inbox rows". Approval 2026-10-16 in a web DM (D1 = A, D2 = A, D3 = B, D4 = A).

- Issue: [CHE-1797](mention://issue/01a140fb-3453-7e77-b963-7e9c877309d3)
- Pages in scope: Settings → Notifications (`apps/mobile/app/(app)/[workspace]/more/settings/notifications.tsx`), Inbox tab (`apps/mobile/app/(app)/[workspace]/(tabs)/inbox.tsx`, `components/inbox/inbox-row.tsx`)
- Tokens: `apps/mobile/global.css`. The repo has no `DESIGN.md` on `main`; this PR does not add one, to avoid an add/add conflict with the other open design PRs. The selection step adds the approved-revisions row.
- Status: rev-1 posted for UX review. No direction selected.
