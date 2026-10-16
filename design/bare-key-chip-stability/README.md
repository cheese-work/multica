## Request

Reported by Cheese on 16 October 2026 (DM with x99-claude-opus, about 23:30 ICT) and recorded on CHE-1827: the web client, also in a mobile browser, "flickers a lot when a bare issue key such as `CHE-1824` renders as a rich chip". Cheese chose the full fix (D1 = B) at 23:50 ICT the same day. That decision includes a loading box and a resolved chip of the same size, so the key does not move the text after it.

The coupled-width fix makes a resolved bare-key chip as wide as its loading box. A short-title chip grows from 138 px to 288 px. This record shows that look and one smaller alternative. It asks Cheese for no decision until a UX reviewer scores the directions.

- Issue: [CHE-1827](mention://issue/01a14594-3b86-77d5-b2b1-45415e7e761a), slice CHE-1829
- Pages in scope: the bare-key presentation only (an issue key typed in a chat message or an issue description). `packages/views/issues/components/issue-chip.tsx`, `issue-chip-loading.tsx`, `packages/views/rich-content/rich-content.tsx`. Explicit authored URL chips and UUID mention chips stay content-sized.
- Tokens: `packages/ui/styles/tokens.css`
- Status: rev-1 posted, unscored
