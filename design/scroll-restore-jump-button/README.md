## Request

Cheese, in a DM office-hours with x99-claude-opus (20 October 2026, 01:32 ICT, DM message `01a15570-11d0-78f7-a1a5-254b0f178914`): choose D1 = C (anchor-based restore of the reading position) and add a jump-to-latest button at the bottom middle of the DM screen. Behavior source: `design-scroll-restore.md`, attached to CHE-1857.

- Issue: [CHE-1860](mention://issue/01a1557c-ff34-72e5-9734-f89389ec7d2e) (parent [CHE-1857](mention://issue/01a15576-df1b-7d17-bb98-679321eb4e78))
- Pages in scope: web chat DM (`packages/views/chat/components/chat-message-list.tsx`, `chat-window.tsx`, shared by desktop and mobile web); native chat DM (`apps/mobile/components/chat/chat-message-list.tsx`)
- Tokens: `packages/ui/styles/tokens.css` (web), `apps/mobile/global.css` (native, same neutral and brand roles)
- Status: rev-4 posted, awaiting independent UX review (rev-1, rev-2 and rev-3 kept unchanged)

## Already decided (not open in rev-1)

1. Round button, bottom middle of the DM screen, above the composer, inside the safe area, down-arrow icon.
2. Accessible name "Jump to latest messages".
3. Shown when the reader is more than one viewport above the newest message; hidden at the live end.
4. Tap scrolls to the newest message and turns auto-follow on.
5. No new-message count.

Rev-1 varies only the look: fill, shadow, emphasis.
