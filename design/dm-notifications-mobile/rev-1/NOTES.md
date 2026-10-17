## What changed vs rev-0

rev-0 is the current mobile app, which has no DM switch, no In-app switch and no `dm_reply` row. It was not captured: no device or simulator is admitted for this task. The mockups follow the source of the current screens instead. Compared against rev-0:

- Settings → Notifications gains an In-app switch for Inbox and one for Direct messages. There is no push switch (design doc N5).
- The Inbox list gains `dm_reply` rows: DM title, "New reply" / "N new replies" / "Decision needed", unread dot, tap opens the DM.
- The Inbox gains a way to show DMs only (decided default 2). Today the mobile Inbox has no filter bar.
- Every direction shows light, dark, loading, load failed, disconnected, empty (filter), save failed, and a tap on a DM that is gone.

## Directions

- A — Each source is a section with an In-app row (web parity). DM row keeps the issue-row layout with a chat glyph. Filter chips under the header.
- B — One "In-app alerts" card of two switches on top. DM row has a chat badge on the avatar and a reply-count pill. Segmented control filter.
- C — Short settings screen with a "Choose events" drill-in. DM row uses a chat tile. Filter is a new option in the existing header action sheet; no new bar.

## Decisions for Cheese

1. Inbox filter: a new bar (A chips, B segmented control) or a header-menu option (C).
2. B renames the existing section "Inbox notifications" to "Inbox events" so the new switch can take that name. Web would need the same rename for parity.
3. When Inbox In-app is off, the six event groups are dimmed and locked (A, B) or the drill-in is dimmed (C). The server keeps the group mutes either way.
4. Offline: switches are locked with a banner (all directions). Today the mobile app has no offline banner on these screens.
5. Inbox rows show the DM title. The no-message-text rule covers push and banners only; confirm with N2 before build.

## Verification limits

- Static HTML from `apps/mobile/global.css` tokens. Not rendered by React Native; fonts are the browser system font, not SF Pro.
- Header and tab icons are simplified drawings.
- `visual diff: NOT-RUN`. No before capture of the current screens: needs an Android or iOS target (see the capture request on CHE-1797).
