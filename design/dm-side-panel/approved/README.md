Locked from doop page `zGq4HKcar9:che-3556` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/zGq4HKcar9), frames `stW-Rsfrx2` at `updatedAt` `2026-11-04T08:16:47.836Z` and `PKZHxCx908` at `updatedAt` `2026-11-04T08:09:55.636Z`. Issue [CHE-3556](mention://issue/01a1a531-900a-7ce4-992f-958a68d216a1).
Direction A (right rail with stacked sections) selected by Cheese (decision request `2492308f-a123-424c-97d3-6cfc383024c3`, recorded as comment `01a1a658-df08-7a02-bf0d-f59f64125164`: "A: right rail with stacked sections"), UX PASS on these exact versions (`01a1a5a8-1671-76a5-85fd-f8eed35a57db`, x99-agy-gemini-ux).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `dm-side-panel-a-right-rail.html`: direction A as read from doop, unchanged. Hash is `screens[0].content_sha256` in `LOCK.json`.
- `dm-side-panel-shared-v2-header-search-list.html`: the v2 shared spec as read from doop, unchanged. Hash is `screens[1].content_sha256`.
- Shows: a 340 px right rail with six sections (work lanes, waiting on you, incidents, scheduled, recent decisions, router cost today); wave 1 open, wave 2 collapsed; 52 px collapsed rail; phone 390 layout; DM header with search, Stats and actions menu; compact confirm; DM list row with agent name.

## Accepted by default, not explicitly confirmed

Cheese's answer gave the letter only. The card's written default applies to these points (reversible).

- Compact means the session `/compact` context compaction with a confirm step, not a compact layout.
- Search in DM means finding messages inside the open DM. The DM-list search stays.
- Snapshot gap: the v1 frames are frozen. The `current UI` baseline `r7XQAGGAmp` was rebuilt from the fork source, not captured live. No live capture was asked for.

## Overlap with earlier designs

| Item | Verdict | Reason |
| --- | --- | --- |
| CHE-2769 "Chờ bạn" sidebar | dependency | Section 2 reads the same asks and uses `origin_dm` to put this DM first. The global sidebar stays as the workspace-wide list. |
| CHE-1898 Open decisions strip | superseded | The strip's count becomes the "Waiting" count on the panel entry. The panel shows the detail the strip never had. |
| CHE-2404 "Needs you" list | dependency | Section 2 reuses its row, receipt and expired semantics. Answering stays on the chat card. |
| CHE-3407 progress card row, direction A | dependency | Section 1 uses this row unchanged. If Cheese picks another row direction on CHE-3407, section 1 follows that pick. |

The v2 scope rows (CHE-1759 shipped and reused; CHE-1794, CHE-1789, CHE-1796, CHE-2169, CHE-2819 and the compaction issues as dependencies) are in section 3b of the design record, doop frame `pD_SrSJWVT`.

## Implementation must add and test

- Panel is read-only: no veto, no cancel. Answering an ask stays on the chat card.
- The stuck threshold (2 h) is a placeholder from the mockup, not source.
- In-DM search needs a per-chat message query that does not exist yet.
- Agent name on DM list rows is new. Pin, archive, rename, agent profile, compact, stats, avatar, title and last message exist today.
- The Compact confirm step is new. Compaction failure and retry states are not drawn; they follow CHE-2894, CHE-2904 and CHE-2916.
- Collapsed rail (52 px) relies on hover tooltips in the mockup; add a non-hover name for each icon (reviewer A11y note, score 7).
- Wave 2 sections ship as empty and placeholder states until their data exists (router T4/T6, CHE-2399, decision records, D2 budget).
