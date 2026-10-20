## Request

Cheese, in a DM (20 October 2026), recorded as decision quotes on CHE-1870:

- Source comment `01a15793-7660-719c-8dad-eff2984fe448`: "B. And i need when click to quoted message in issue, multica client route to exact DM and scroll to exact message then gentle flash the DM bubble animation"
- Amendment 1, comment `01a1579f-ff30-75f2-9bc3-b5cc439e56df`: "improve this UI by making an info card right below the decision bubble message from user to inform that that message is quoted. Clicking into that info card will route user to the quoted message in issue, scroll to exact message position"
- D3 = A (comment `01a157a7-31ea-7b1e-b862-a86a651ad56d`): hide the separate "Decision recorded" message when the card renders.
- Also from the issue: the issue quote shows Cheese as author with the recording agent as a sub-line, and both surfaces have a Disclaim button.

- Issue: [CHE-1870](mention://issue/01a15793-484c-7f2b-80ea-3b120169de04)
- Pages in scope: issue detail timeline (`packages/views/issues/components/comment-card.tsx`), DM message list (`packages/views/chat/components/chat-message-list.tsx`)
- Tokens: `packages/ui/styles/tokens.css`.
- Status: rev-1 posted
