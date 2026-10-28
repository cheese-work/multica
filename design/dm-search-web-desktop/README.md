## Request

> Feat: DM search — Cheese, in the CHE-1789 request

- Issue: [CHE-1794](mention://issue/01a140fb-14aa-7688-a516-cbf2c60d7b23) (web and desktop search box; the code is built later)
- Parent: [CHE-1789](mention://issue/01a140fa-ca77-7f2c-aa9f-200fa58ab088). Behavior source: `docs/designs/dm-search-notifications-push.md`, section "S2: web and desktop search box". Server: `GET /api/chat/search` and the message anchor (S1, merged).
- Pages in scope: the Chat page left rail (full page and compact), the floating chat window's history popover, the DM opened at a matching message.
- Not in scope: the mobile app (S3), the search API, any change to pin, archive, statistics, rename or new-chat behavior.
- Status: rev-3 posted (direction A only; rev-1 and rev-2 kept). Cheese picked A on 22 October 2026 (CHE-1794 comment `01a160fd-22c9-75b8-9cb4-ca7421f09464`). On 28 October 2026 he chose A on all four product choices (comment `01a181fc-3992-777d-8315-c078f2150219`): Back restores the query, keep the three-line row, inline Unarchive on an archived hit, and bare `/` opens DM search. Static-only UX PASS on rev-3: comment `01a1615c-86ec-7b9b-a19a-596efb694215`. A pick is a selection, not an approval: approved only when PR #346 merges. Runtime checks are NOT-RUN.
