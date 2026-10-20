## Request

Cheese, in the CHE-1876 description (design doc from `/office-hours`, 20 October 2026, DM about 11:50 ICT), verbatim: "feat: user have quoting UI from agent message like in issue (a.k.a annotation)". Cheese chose approach B: port issue reply annotation to DMs (selection comment `01a157ab-493d-73ec-86ff-8efaac5e47b6`).

- Issue: [CHE-1876](mention://issue/01a157ab-1ad3-7a70-902c-90ac2509a802)
- Pages in scope: agent message in a DM (`packages/views/chat/components/chat-message-list.tsx`, `AssistantMessage`), DM composer (`chat-input.tsx`), the sent user bubble. Issue annotation (`packages/views/issues/components/`) stays as is.
- Tokens: `packages/ui/styles/tokens.css`.
- Status: rev-1 posted
