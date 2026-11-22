# Notification settings

## What it does

Per workspace and member, each notification group is `all` or `muted`, and each source (`inbox`, `dm`) has per-channel switches (`in_app`, `push`). DM inbox replies are coalesced; the channel switches are off by default (opt-in).

Sub-features:
- Group mute: `server/internal/handler/notification_preference.go` (`/api/notification-preferences`), web `packages/views/settings/components/notifications-tab.tsx`.
- Channel switches: `server/internal/handler/notification_channel.go`.
- Not driven yet: every row; this is a seed map.

## How to reach it

- **Web**: workspace settings, Notifications tab.
- **Desktop**: same shared settings view.
- **CLI**: none found for preferences. Read the API with a personal access token only on the isolated stack.
- **Mobile**: `apps/mobile/app/(app)/[workspace]/more/settings/notifications.tsx` (data in `apps/mobile/data/queries/notification-preferences.ts`). Device evidence required; this skill cannot produce it.

## Settings

Groups the server accepts: `unassigned_activity`, `assignments`, `status_changes`, `comments`, `mentions`, `updates`, `agent_activity`, `reactions`, `autopilot`, `system_notifications`. Values: `all`, `muted`. The web type `NotificationGroupKey` lists 8 of these (no `reactions`, `autopilot`). Default for an absent key: all.

## States

Per group: `all` or `muted`. Per channel: on or off. A saved change must still hold after a reload.

## Edge cases

- Go and web group lists are a coupled pair that changes in stages: a key the server accepts and the web does not render is declared server-only in the contract test (CHE-6551 plan, vocabulary ownership). A new key needs both halves, or a settings toggle is missing.
- `system_notifications` is a delivery-channel toggle, not an inbox event group (comment in `notification_preference.go`).
- Mobile keeps its own copy of the group list and its own copy text.

## Ledger row

`docs/fork/FORK-FEATURES.md`: C16 (per-channel notification settings and coalesced DM inbox replies, off by default); C75 for the ask guard.
