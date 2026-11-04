## What changed vs rev-0

rev-0 is the current web Settings → Notifications page and Inbox. They were not captured: they need a signed-in session and an account, which this task does not have, so the baseline is `NOT-CAPTURED`. The mockups follow the source of the current screens (`notifications-tab.tsx`, `browser-notification-setting.tsx`, `settings-layout.tsx`, `inbox-list-item.tsx`) instead. Compared with rev-0:

- Settings → Notifications gains an In-app and a Push switch for Inbox and for Direct messages. The existing six event rows stay.
- A new "Push on this device" control registers this browser. It covers browser permission, a blocked browser, an unsupported browser, a server without push keys, a failed registration and the iPhone Home Screen steps.
- First-run defaults follow CHE-1848: Inbox In-app on, everything else off. Direct message replies reach the Inbox only after an opt-in. Decision requests in a DM are separate and always arrive.
- The Inbox shows DM records (`dm_reply`) with the DM title, "New reply" / "N new replies" / "Decision needed", and a way to show DMs only.
- Banners follow the content-private push payload: sender as title, only "New reply", "N new replies" or "Decision needed" as text. No message text and no DM title.
- Every direction shows the same 14 settings frames (including the Electron variant), then four or five Inbox frames and one banner frame.

## Directions

- A — Sections with channel rows. Web parity with approved mobile direction A: Inbox and Direct messages sections each led by In-app and Push rows, then a This device section. DM rows get a chat glyph and chips filter the list.
- B — Matrix and device summary. A This device card first, then one compact grid of sources by channel. DM rows get an agent avatar with a chat badge and a count pill. The existing filter menu gains Direct messages.
- C — Device first, guided. One guided card with three visible steps and one action, then compact In-app and Push checkboxes per source. The Inbox groups DM records under their own header.

## Decisions for Cheese

1. Settings layout: A (sections, mobile parity), B (grid plus device summary) or C (guided device card, checkboxes).
2. Two permission-related rows exist today ("Browser notifications" for in-tab banners) and one is new ("Push on this device"). The mockups keep both. Merging them into one row is possible but changes a shipped control.
3. Push switches stay editable even when this device has push off, because they are account-wide choices for every device. The alternative is to lock them until a device is registered.
4. DM placement in the Inbox: chips (A), the existing filter menu plus count pill (B) or a separate group (C).
5. The Inbox row shows the DM title. Push and OS banners never do, because a DM title is derived from the first message.
6. The "blocked in this browser" notice on app load: show it only when a Push switch is on, or always while permission is denied.

## Required states, in every direction

First run, Inbox In-app off, opted in with push on, browser blocked, iPhone Safari outside the Home Screen, push not configured, unsupported browser, registering, register failed, loading, load failed, offline, save failed, Electron. Inbox: default, filtered, no DMs, tapped DM no longer available. Banners: desktop, iPhone lock screen, blocked notice, no banner while the DM is open.

## Accessibility limits

- Every switch has an accessible name from its row label; checkbox labels wrap their control. Status is text and icon, never colour alone.
- Switch targets copy the shipped control (36 x 20 px) on desktop. Phone frames show a 44 px target as a proposal, not current code.
- Matrix column heads (B) need names when the grid collapses on a phone; the mockup shows them as inline labels.
- No screen-reader pass, no keyboard run, no dynamic-type check. Do not read this as an accessibility pass.

## Verification limits

- Static HTML from `packages/ui/styles/tokens.css` values, copied through the `jev-governance-tab` rev-4 stylesheet. The app did not render it. The font is the browser system font.
- Current screens: `NOT-CAPTURED`. `visual diff: NOT-RUN`.
- Not run: the service worker, any live push, an iPhone Home Screen run, native behavior, any device. Nothing here approves N3's setup waiver as device authority.
- The six PNGs are 128-colour palette files, full page, captured on X99 with headless Chrome at 1440 px and 390 px. They are 313 to 532 KB, over the 300 KB image budget; the PR description carries the repository's exemption line.
