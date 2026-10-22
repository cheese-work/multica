## Request

> Feat: DM search — Cheese, in the CHE-1789 request

- Issue: [CHE-1794](mention://issue/01a140fb-14aa-7688-a516-cbf2c60d7b23) (web and desktop search box; the code is built later)
- Parent: [CHE-1789](mention://issue/01a140fa-ca77-7f2c-aa9f-200fa58ab088). Behavior source: `docs/designs/dm-search-notifications-push.md`, section "S2: web and desktop search box". Server: `GET /api/chat/search` and the message anchor (S1, merged).
- Pages in scope: the Chat page left rail (full page and compact), the floating chat window's history popover, the DM opened at a matching message.
- Not in scope: the mobile app (S3), the search API, any change to pin, archive, statistics, rename or new-chat behavior.
- Status: rev-2 posted (direction A only). Cheese picked A on 22 October 2026 (CHE-1794 comment `01a160fd-22c9-75b8-9cb4-ca7421f09464`). A pick is a selection, not an approval: approved only when PR #346 merges. rev-1 stays as the record of A, B and C.
