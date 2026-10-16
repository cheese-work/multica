## Request

CHE-1727 description, Cheese's decision of 15 October 2026 (DM, about 14:25 ICT), relayed on the issue: "a workspace-wide switch that pauses wakeup firing". Design in the issue: "an admin-only master switch at the top of Settings → Wakeups … While paused, the per-rule switches show as overridden. A banner "Wakeups paused by <name> at <time>" shows on the settings tab and in the issue wakeup panel."

- Issue: [CHE-1727](mention://issue/01a13e77-dcc3-79ab-8fee-4bfebf784441)
- Pages in scope: Settings → Wakeups (`packages/views/settings/components/wakeups-tab.tsx`), the issue wakeup panel (`packages/views/issues/components/wakeups-section.tsx`)
- Tokens: `packages/ui/styles/tokens.css`. The repo has no `DESIGN.md` yet; the first approved row creates it.
- Status: selected rev-3 direction A — approved when PR #255 merges

The implementation on PR #254 follows direction A.

Selection: Cheese selected direction A of rev-3 at `de646d2bb8c50bb93653d98395f17f3dc2e0ceee`. Source: CHE-1811 decision receipt `01a14243-3674-7796-aaf2-b9354a94065e`, Cheese's reply at 08:04 ICT on 16 October 2026, recorded on CHE-1727 in comment `01a14242-984f-74f7-a82e-86d52f5abab9`. A chat selection is not approval. Cheese approves by merging PR #255.
