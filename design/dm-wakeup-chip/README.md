## Request

Cheese's decision of 20 October 2026 (DM, D2 = B, native DM wakeup), recorded in the description of CHE-1874: "Cheese sees and can cancel each wakeup: DM chip "⏰ <agent> checks back 11:40 ICT · Cancel"." Source comment: `01a157a7-2dbb-719b-9c39-47b7cc060d5e`.

- Issue: [CHE-1874](mention://issue/01a157a7-29bd-7e01-bf38-8f8c02b24e02)
- Pages in scope: the web and desktop DM window (`packages/views/chat/components/chat-window.tsx`, next to `chat-queue.tsx`). Mobile is a follow-up and is not designed here.
- Tokens: `packages/ui/styles/tokens.css`, copied into each mockup.
- Data the chip shows: `GET /api/chat/sessions/{id}/wakeups` (PR 2b, #320): `fire_at` (the time, or the deadline when a condition is set), `instruction`, `condition` (watched issue identifier plus status, pull request event or commenter), `status`. A wakeup set in an earlier DM of the same conversation is listed too.
- Status: exploring — rev-1 has three directions and is not posted to Cheese. A different-lab UX score is required first.
