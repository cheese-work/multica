## Request

CHE-1727 description, Cheese's decision of 15 October 2026 (DM, about 14:25 ICT), relayed on the issue: "a workspace-wide switch that pauses wakeup firing". Design in the issue: "an admin-only master switch at the top of Settings → Wakeups … While paused, the per-rule switches show as overridden. A banner "Wakeups paused by <name> at <time>" shows on the settings tab and in the issue wakeup panel."

- Issue: [CHE-1727](mention://issue/01a13e77-dcc3-79ab-8fee-4bfebf784441)
- Pages in scope: Settings → Wakeups (`packages/views/settings/components/wakeups-tab.tsx`), the issue wakeup panel (`packages/views/issues/components/wakeups-section.tsx`)
- Tokens: `packages/ui/styles/tokens.css`. The repo has no `DESIGN.md` yet; the first approved row creates it.
- Status: rev-1 posted for UX review

The implementation on PR #254 follows direction A.
