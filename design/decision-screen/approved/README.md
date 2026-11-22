Locked from doop page `mRAnnjvGod:che-5480` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod), snapshot `pvdFzm9p3Q` (15 Nov 2026 02:42 ICT). 24 frames, listed with ids and `updatedAt` in `LOCK.json`. Issue [CHE-5481](mention://issue/01a1d453-1a75-7175-83ca-e1ee27b61648); plan [CHE-5480](mention://issue/01a1d452-a145-7c4e-bf1a-3d8308b307e8).
Direction D1 (Decisions in the sidebar). Cheese picked D1 on decision card `01a1dc34-1dd5-711a-b19c-6038d86d3287`, recorded in comment `01a1dd15-5932-73f6-8571-481360ef6bf9`. UX PASS on snapshot `pvdFzm9p3Q` (CHE-5555 comment `01a1db99-79cb-7323-b00a-f84ffd2de2f7`, x99-codex-luna-design; 8/7/9/9/7/7).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

## What this lock holds

- Round 1 (`d1-a` to `d1-f`, 1440 px desktop and 390 px phone in each file): queue with one real decision and a blind pick (stakes before the pick buttons, no recommended mark); reveal after a matching pick (recommendation, "you matched", Lottie placeholder); reveal after a different pick (neutral, "difference logged"); decided card (question, answer, effect status, target metric, Revoke while revocable, source issue; never one line); batch lane "decided, veto?"; empty state.
- Round 2 (`r2-*`): browse mode, hero transition from the card to the issue, issue decision mode, floating card when scrolled away, mention navigation, DM-origin decision, draft and annotation reopen, ask more (pseudo-DM), shared other-draft, snooze picker, consequence graph, after-pick, stale card, review and stats, public debate with an invited agent, decided thread folded, and the new-decisions push / Inbox item (`r2-g2`).
- The card is the B v9 decision card (`design/decision-card/approved/`, CHE-5162 lock), not a new card.
- Removed by this design, one frame note each: the top-bar pill, the issue "Decision needed" banner, the inline DM decision cards. Ship the removal in the same release as the screen and its notification, never before (CHE-5480 decision 7).

## Carry into implementation (not blockers)

- P2 from the UX verdict: in `r2-p-snooze-picker` the phone "Merge sub2api" row is a `div`. Build it as a `button`, as in `r2-o`.
- Render, contrast and clipping are not verified: doop screenshots timed out, so the exporter checked non-blank renders only (frame `d1-a` by eye). Check once in the first build PR.
- Lottie: play once, under 1.5 s, never block the next decision. Under `prefers-reduced-motion` show the static frame. The mockup holds a placeholder box only. The asset needs a product-compatible license; no paid generation.
- Map mockup colour values to semantic tokens in `packages/ui/styles/tokens.css`; the mockup copies values.
- All copy needs every supported translation. The mockup is in English.
- Build conflicts (CHE-5480): CHE-2949 series, CHE-5376 (the pill goes away), CHE-5511 (DM list `Quyết định` chip and tag become empty after this ships), CHE-3461 sync is not a blocker for D1.

## Not covered by this lock

- D2 (Inbox tab) frames; they stay on the canvas as history.
- The "before / removed surfaces" reference frame `DVYh9mZYeg`.
- Q2 round 2c ideas (default: none drawn).
- Dark and light theme parity, live data, and reflow between 390 px and 1440 px.
- Backend: classifier, auto-apply and veto window, card lint.
