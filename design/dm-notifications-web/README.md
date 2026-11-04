## Request

> Feat push noti on mobile web shortcuts (safari/chrome) and desktop browser — Cheese, in the CHE-1789 request
> Feat: opt in inbox noti and DM noti in setting — Cheese, in the CHE-1789 request

- Issue: [CHE-1798](mention://issue/01a140fb-3ecf-7751-a93b-4bbafd5d51ec) (web notification settings, DM inbox rows and banners; the service worker is built later)
- Parent: [CHE-1789](mention://issue/01a140fa-ca77-7f2c-aa9f-200fa58ab088). Behavior source: `docs/designs/dm-search-notifications-push.md`, section "N4: web client", with CHE-1848 (DM switches off by default) superseding the old DM defaults.
- Pages in scope: Settings → Notifications (web and the Electron shell), the Inbox list with DM records, in-app and OS banners, browser permission and device push states, iPhone Home Screen guidance.
- Not in scope: the service worker and any runtime push test, the mobile app (its own approved design is `dm-notifications-mobile`), Electron push.
- Status: selected rev-1 direction A — approved when PR #307 merges
