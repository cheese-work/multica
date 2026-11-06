Locked from doop page `zGq4HKcar9:che-3407` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/zGq4HKcar9, "Cheese Work Setup"), frames `V0nBnIQ-OI` at `updatedAt` `2026-11-04T12:00:37.964Z` and `TiPMSzRj-T` at `updatedAt` `2026-11-04T12:01:34.967Z`. Issue [CHE-3407](mention://issue/01a1a294-a784-73b5-98e2-856ad7a59f5a), part of CHE-3369.
Direction C (Attention first) selected by Cheese (decision request `1fb8d9d9-764c-4032-8e11-6850f7dc4af8`, pick quoted in comment `01a1ab02-66d6-71c4-ad3f-313a99f4abb3`, recorded as comment `01a1ab0d-e919-7b79-87bf-a093a9a7b4af`), UX PASS on these exact versions (`01a1a673-783f-76c4-b35a-d5e7bb320087`, x99-agy-gemini-ux).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `progress-card-c.html`: frame C as read from doop, unchanged. Its hash is `screens[0].content_sha256` in `LOCK.json`.
- `progress-states-s.html`: frame S (shared states for A, B and C), unchanged. Its hash is `screens[1].content_sha256`. Only the states and the C layout apply; A and B were not selected.
- Shows: the DM progress card on desktop 540 and phone 358: a verdict line first, tinted blocks for Blocked, In review and Done, one-line rows for routine moves, the immediate blocked and done cards, the `/progress` reply with the caret menu (Open issue, Mute updates, Unfollow), muted and unavailable rows, and a hostile issue title shown as plain text.

## Known weakness

- The 4th blocked issue falls back to a one-line row. Only 3 tinted blocks show, so with 4 or more issues that need a person, the extra ones lose the detail and the tint. The verdict line still counts them.

## Implementation must add and test

The mockup does not show these, and neither frame was edited for them.

- Verdict text comes from fixed i18n templates and enum counts. Order is Blocked, In review, Done, In progress, then the rest. The issue title is escaped plain text, 120 characters; it never renders markdown, mentions or HTML.
- The caret calls REST (Open issue, Mute updates, Unfollow). Cards have no reply box and start no run.
- Banners that appear after load use `role="alert"` (failures) or `role="status"` (info). A card that arrives after a reconnect renders by message id, so it never shows twice.
- Status is glyph plus word, never colour alone. Phone touch targets are 44 px; the desktop caret is 28 px in the mockup, which is a proposal, not source.
- The push line holds a count only, with no title, name or key.
- The Vietnamese preview in frame S shows boxes for stacked-mark letters because of the screenshot renderer, not the HTML. Judge layout and length there, not the glyphs.
