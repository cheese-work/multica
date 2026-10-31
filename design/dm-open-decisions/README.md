## Request

Cheese, in a DM with x99-claude-opus on 20 Oct 2026 (about 17:20 ICT), as recorded in the CHE-1898 description: "DM should show active decision, so users won't get surprised if anything goes south." Cheese chose "A then B"; B is this slice. A is CHE-1897 (the clear cancel message).

- Issue: [CHE-1898](mention://issue/01a158e4-7641-7026-950a-d6045221c0eb)
- Pages in scope: the DM session view in `packages/views/chat/` (`chat-window.tsx`, `chat-page.tsx`, `chat-session-header.tsx`). Web and desktop share it. Mobile is out of scope.
- Data shown per request: issue key and title (link), escalation class, asked age, state (`open` = waiting on you, `answered`). Source: `decision_request` (`server/migrations/597_decision_parents.up.sql`).
- Tokens: `packages/ui/styles/tokens.css`
- Status: rev-1 posted — three directions, awaiting UX review and Cheese's pick

## Settled by the request, not open

1. The list holds only `open` and `answered` requests of this DM session.
2. A request that is cancelled or closed changes or leaves without a reload.
3. The strip is not rendered when the list is empty.
4. More than about 3 rows can be collapsed.
