# Design

Product design record for the web and desktop app. Mockups live in `design/<topic>/rev-<n>/`; a design PR merge is the approval. This file starts from what the code already does. It records Cheese's selection of direction C for Jev / Governance (below). A selection is not an approval.

Standard: `standards/general/design-record.md` in `cheese-work/multica-dotfiles`. Procedure: skill `multica-cheese-work-design-loop`.

## Token source

| Need | Source |
| --- | --- |
| Colour, surface, ring, radius, text scale | `packages/ui/styles/tokens.css` (`:root` light, `.dark`). Use semantic names (`surface`, `surface-border`, `muted-foreground`, `destructive`, `ring`, `brand`), never raw values. |
| Text sizes | Role-named `--text-micro` to `--text-display` in `tokens.css`: caption 12/16, label 13/18, body 14/20, title-lg 20/28. |
| Radius | `--radius` ladder in `tokens.css`: xs micro controls, sm nested surfaces, md rows and menus, lg controls, xl cards and dialogs. |
| Base behaviour | `packages/ui/styles/base.css`. At 767 px and below, or on a coarse pointer, text inputs are 16 px so iOS does not zoom. |

Mockups copy these values into a local `shared.css`. A mockup never invents a token.

## Components and patterns

| Pattern | Source | Rule |
| --- | --- | --- |
| Settings page | `packages/views/settings/components/settings-layout.tsx`: `SettingsTab`, `SettingsSection`, `SettingsCard`, `SettingsRow` | One page title with scope badge, sections of `text-body font-semibold`, rows of label, optional help, control. |
| Settings navigation | `packages/views/settings/components/settings-page.tsx` | Sidebar from `md` up. Below `md`: a native `<select id="settings-navigation">` with optgroups, `h-10`, under the page title. A phone layout keeps it. |
| Read-only member view | `SettingsReadOnlyNotice` in `settings-layout.tsx` | Members see the page. A `role="note"` block names who can change it (owners and admins, up to three names). |
| Save state | `SettingsSaveState` in `settings-layout.tsx` | `role="status"`: saving, saved, error. |
| Buttons | `packages/ui/docs/button.md`, `packages/ui/components/ui/button.tsx` | One dominant action per dialog or form. Pending: `disabled` + `aria-busy`. Icon-only buttons need a name. |
| Dialogs | `packages/ui/docs/dialog.md`, `packages/ui/components/ui/dialog.tsx`, `alert-dialog.tsx` | Dialog for a focused task. AlertDialog for a consequential confirmation. Keep the footer reachable at narrow widths. |
| Tabs | `packages/ui/components/ui/tabs.tsx` (Base UI) | Roving focus, Left/Right moves, Home/End jump, one `tabpanel` per tab. Prefer it to `SettingsViewTabs`, which has no panel semantics and is 28 px high. |
| Copy | `AGENTS.md`, "UI Copy" | Help only for a non-obvious choice, constraint, consequence or next step. State each fact once. |

## Required states

Every page shows: loading, empty, error with a retry, offline or disconnected, read-only (member), and any 409 or conflict with the user's edits kept. Disabled controls say why in visible text.

## Accessibility baseline

- Focus is visible and sits on a control that can take it. A disabled control cannot hold focus.
- Touch targets: button heights come from `--button-height-*` in `tokens.css` (24, 28, 32, 36 px). The Jev / Governance mocks use 44 px tabs and 40 px buttons at phone width. That is a proposal, not current code.
- Status is never colour only; pair it with text or an icon that has a name.
- Credentials are never rendered. Show set or not set.

## Known gaps

- `SettingsViewTabs` is not a full tab pattern (no panel link, 28 px). A new tabbed Settings page needs `Tabs` or a fix to that component first.
- Phone page title and navigation are owned by `settings-page.tsx`, not by each tab. A tab design must not hide them.
- DM follow-up strip (`dm-wakeup-chip` rev-2 A): the strip has no visible group label, so a user can overlook it (UX verdict `01a158ba-682c-71b3-a029-92ea70549b96`). Desktop Cancel is 24 px. Live focus order and screen-reader output were not tested. The build slice settles the label and the target size.

## Repository rules (from main)

Merged from the `DESIGN.md` that landed on `main` with PR 255. Both records apply.

### Source

Derived from the repository rules in `AGENTS.md` (UI Copy, Web/Desktop UI Rules) and the tokens in `packages/ui/styles/tokens.css`. It records no decision beyond them. Cheese approves a design by merging its design PR.

### UX principles

- Descriptions are optional. Add one only for a non-obvious choice, constraint, consequence or next step, and state each fact once beside its control.
- Keep permissions, cost, destructive consequences, execution prerequisites and error recovery visible.
- Selected states stay identifiable on hover. Handle overflow, long text and scrolling on purpose.
- Prefer the existing shadcn/Base UI primitives.

### Tokens

Use semantic tokens from `packages/ui/styles/tokens.css`: surface, border, muted-foreground, primary, destructive, warning, and the role-named `--text-*` type scale (caption 12 px, body 14 px, title-sm 16 px, title 18 px). Mockups copy the token values and never invent colors.

### Patterns

- Settings tab: a section title, an optional one-line description, then a card of rows. A row has a label, an optional description and one control on the right.
- Notices inside a tab or panel use the muted notice for information and the warning tone for a state that stops work.
- Admin-only controls stay visible to members, disabled, with a one-line reason.
- Mobile notification settings (`apps/mobile`, `dm-notifications-mobile` rev-1 A): one section per source (Inbox notifications, Direct messages), each led by an In-app switch row. With Inbox In-app off, the event rows are dimmed and locked. While disconnected, the switches are locked under a warning banner.
- Mobile Inbox DM rows (same revision): the issue-row layout with a chat glyph where the status icon sits, a "New reply" / "N new replies" / "Decision needed" second line, and filter chips under the header (All, Unread, Direct messages).
- DM follow-up strip (`dm-wakeup-chip` rev-2 A, web and desktop): a quiet bordered strip tucked behind the composer, the same layer as the chat queue (`packages/views/chat/components/chat-queue.tsx`). One row per pending follow-up: clock icon, "<agent> checks back 11:40 ICT" (or "when CHE-1812 is done, or by 11:40 ICT" for a condition), the agent's instruction on a second line (two lines at phone width), and a Cancel button at the row's end. At most 3 rows; the strip is 168 px high at most and scrolls. Nothing renders when none is pending. Times are in ICT. Cancel is natively disabled while cancelling and while offline, with the reason in a visible banner.
- DM decision quote card (`dm-decision-quote-cards` rev-8 B, web and desktop): the issue comment shows the quote as Cheese's, in a bordered card whose strip carries the class and state pill, with Open DM and Disclaim inside the card; the agent is a sub-line. In the DM, a rounded chip under the quoted bubble reads "Quoted in <issue>" with the state pill and Disclaim. On a forked DM the chip reads "Quoted in <issue> · from the original DM" with an Inherited badge and no Disclaim, and opens the original issue comment. Disclaim confirms in an alert dialog on both surfaces, with no reason field. A 409 shows a notice on the card and keeps any draft: "decision context changed" offers Refresh and keeps Disclaim disabled until refreshed, and "already withdrawn" mutes the card and offers Review.
- Viewer media page (`viewer-media-pages` rev-1 A, web and desktop): the viewer dialog toolbar gains one icon button, "Open in new tab" ("Open in new window" in the desktop app), in the right group beside Download and before Close. The page shows only that media item with the same toolbar and stage, a title-only header, no issue, no comments and no Close button; Copy link replaces the open button. No access or not found shows the standard not-found page with no media and no hint which case. Loading disables the toolbar, a load error offers Retry, offline keeps the last loaded copy under a banner. At phone width the toolbar wraps to two rows and the actions beyond zoom and fit sit behind a More menu with 40 px targets; the menu items must be real focusable buttons.
- DM fired wakeup row (`dm-wakeup-event-row` C, web and desktop; selected, approved when the lock PR merges): the wakeup is a system event on the thread, not a user bubble. A 24 px clock node on a left rail runs down into the agent's reply avatar. Line 1 reads "Lịch hẹn 17:00 ICT" with "<agent> đặt lúc 10:10 ICT"; line 2 is the title (`--title`, or the first line of the instruction on one line with an ellipsis). The instruction is collapsed behind a "Xem chỉ dẫn" toggle (`aria-expanded`, `aria-controls`, 44 px high at phone width); opened, it shows verbatim in a labelled region with the note that the agent receives it in full. Issue mentions are compact chips and appear once. The model-facing header is never shown to people. While the agent has not replied, the reply slot shows "<agent> đang trả lời" in a `role="status"` region; reduced motion stops the dots. The clock node is `role="img"` with a label.
- Own-comment highlight (`own-comment-background` rev-1 C, web and desktop; selected, approved when PR 405 merges): a comment authored by the viewing member gets a cool brand-tinted fill and a compact "You" label after the author name. Ownership is a member `actor_id` match only; an agent never qualifies by ID alone. The fill covers one comment, never its sibling replies, and the sticky header carries it. The jump-target fill stays distinct and wins while the target is active. The CHE-2113 amber self-mention stays inside a filled comment. "You" needs every supported translation.
- Web notification settings (`dm-notifications-web` rev-1 A): Settings → Notifications has Inbox and Direct messages sections, each led by In-app and Push rows, then a This device section for browser permission and push registration. Web Inbox DM rows get a chat glyph and filter chips (All, Unread, Direct messages). Banners carry the sender and "New reply" / "N new replies" / "Decision needed" only.

## Approved revisions

| Topic | Revision | Direction | Design PR | Revision commit SHA |
| --- | --- | --- | --- | --- |
| wakeups-pause | 3 | A | 255 | `de646d2bb8c50bb93653d98395f17f3dc2e0ceee` |
| jev-governance-tab | 4 | C | 268 | `a32ec00a3ee38cbdf9336c3c953f7cf840ace38b` |
| dm-notifications-mobile | 1 | A | 277 | `a768e4532ac3f193ece7b552308a95eabe97619b` |
| dm-stats-archive-menu | 2 | C | 284 | `686db5fa39eec1b2dc533bdff6d4590a02fdd2a1` |
| bare-key-chip-stability | 1 | B | 294 | `849203d51f285ed2807d1f75cf0890d32e5448a4` |
| dm-wakeup-chip | 2 | A | 323 | `89d22f879ac88d4ce2276702f7bdaef5524736ce` |
| dm-decision-quote-cards | 8 | B | 316 | `6433e6e1ba664ca30096075a61a56ad08b837d62` |
| viewer-media-pages | 1 | A | 403 | `462bf2fecdc4e3b63e911347a20638ca55671dff` |
| own-comment-background | 1 | C | 405 | `87dc762d3f23755f66dbc532156e9294e65d156e` |
| dm-notifications-web | 1 | A | 307 | `0b7065192a3d456ef5bd06173aff361ae0924387` |
| dm-search-web-desktop | 3 | A | 346 | `78d53cf894f7a89beff98c0e9af7c94a7c69c905` |
| dm-wakeup-event-row | doop `-Yr2vfvBHn` @ 2026-10-29T11:07:11.362Z | C | 581 | n/a (`design/dm-wakeup-event-row/approved/`) |
| machine-pause | doop `AvexgrxkBp` @ 2026-10-29T15:15:52.443Z | A | 631 | n/a (`design/machine-pause/approved/`) |
| you-are-approving-card | doop `t8_q2WrMLL` @ 2026-10-30T12:12:16.518Z | B | 664 | n/a (`design/you-are-approving-card/approved/`) |
| machine-pause-banner | doop `PX_YkzqAb-` @ 2026-10-31T03:10:54.092Z | B | 684 | n/a (`design/machine-pause-banner/approved/`) |
| hierarchy-minimap | doop `HZbKCzDxOj` @ 2026-10-31T14:53:12.877Z (A) with dialog `Kb3NjW8ywR` @ 2026-10-31T15:26:50.020Z, `wJLp5mllsX` @ 2026-10-31T15:26:43.046Z, `61GIbGrYNt` @ 2026-10-31T14:58:35.785Z | A | 712 | n/a (reference files deleted by CHE-2765; read them at merge SHA `5f47069e175dca73d4e5ea0d78942d679cd67793`) |
| artifact-viewer-dialog | doop `Bi32qQ55Hi` @ 2026-11-01T04:43:09.622Z | B | 762 | n/a (`design/artifact-viewer-dialog/approved/`) |
| run-mode-chip | doop `N4j_S11KaP` @ 2026-11-02T08:23:51.708Z | C | 815 | n/a (`design/run-mode-chip/approved/`) |
| dm-progress | doop `V0nBnIQ-OI` @ 2026-11-04T12:00:37.964Z (C) with states `TiPMSzRj-T` @ 2026-11-04T12:01:34.967Z | C | 1034 | n/a (`design/dm-progress/approved/`) |
| dm-attention | doop `y7DenM6GGW` @ 2026-11-07T12:07:33.378Z (B Light), `ztpwE4spMa` @ 2026-11-07T12:07:37.297Z (B Dark) | B | 1194 | n/a (`design/dm-attention/approved/`) |
| issue-sidebar-compact | doop `UVi0hFp3L_` @ 2026-11-05T10:56:09.927Z | A (v2: C placement, A mechanics) | 1048 | n/a (`design/issue-sidebar-compact/approved/`) |
| timeline-views | doop `huve-k7jLE` @ 2026-11-06T13:50:07.153Z (Gantt B), `20EKgO9pKX` @ 2026-11-06T14:03:42.763Z (Swimlane A), `mzbsoFU2MX` @ 2026-11-07T06:25:43.109Z (Hierarchy A), `aTIpEERfej` @ 2026-11-06T14:00:23.849Z (Sidebar A) | Gantt B, Swimlane A, Hierarchy A, Sidebar A | 1163 | n/a (`design/timeline-views/approved/`) |
| decision-card-fold | doop `HtwdZpoCai` @ 2026-11-07T12:47:11.565Z | A | 1183 | n/a (reference files deleted by CHE-4431; read them at merge SHA `6888ddb6d214950cb04bb21ec6a671736a0d65bb`) |
| workflow-lint-ui | doop `6mYQFWIMvR` @ 2026-11-06T07:54:26.758Z (D light) with dark `u9lNVhB-2k` @ 2026-11-06T07:55:08.558Z, dialog `-IGdS_B1PB` @ 2026-11-06T07:56:19.372Z, `Tz8ZFkiTdl` @ 2026-11-06T07:56:38.931Z, states `-op18JpCVJ` @ 2026-11-06T07:56:54.349Z | D | 1211 | n/a (`design/workflow-lint-ui/approved/`) |
| delegated-governed-writes | doop `Ocyo3e9IQD` @ 2026-11-08T09:29:28.407Z | B | 1256 | n/a (`design/delegated-governed-writes/approved/`) |
| decision-card | doop `Z3bizs5RyO` @ 2026-11-12T04:18:27.430Z (B v9) | B | 1379 | n/a (`design/decision-card/approved/`) |
| background-agents | doop `9L657IkfjZ` @ 2026-11-13T06:10:19.495Z (A2), `XyqcwrCaU8` @ 2026-11-13T06:10:21.146Z (B2), `IZ5zGPF9ZM` @ 2026-11-13T06:10:30.086Z (C2), `fLyQE0xYpu` @ 2026-11-13T06:10:50.374Z (D2), `ijhJ5xbn0u` @ 2026-11-13T06:25:14.888Z (E) | v2 | PRNUM | n/a (`design/background-agents/approved/`) |

Each row is written before its merge, as the standard requires. A design is approved when Cheese merges its PR: PR 255 for `wakeups-pause`, PR 268 for `jev-governance-tab`, PR 277 for `dm-notifications-mobile`, PR 284 for `dm-stats-archive-menu`, PR 307 for `dm-notifications-web`. The merge commit is read from GitHub and is not written here.

The `jev-governance-tab` row records a selection only. Cheese chose direction C at 03:18 ICT on 16 Oct 2026 (CHE-1780 comment `01a14136-ef91-73e1-867f-41c3747e6ad7`: "C  is direction"). The commit in that row is the `rev-4` revision commit. No approval exists until PR 268 merges.

The `dm-notifications-mobile` row records a selection only. Cheese chose direction A on CHE-1811 (comment `01a14243-3674-7796-aaf2-b9354a94065e`). The commit in that row is the `rev-1` revision commit. No approval exists until PR 277 merges.

The `dm-stats-archive-menu` row records a selection only. Cheese chose direction C on CHE-1782 (comment `01a15844-7da8-78f5-880b-7f891dde2142`: "C"). The commit in that row is the `rev-2` revision commit. No approval exists until PR 284 merges.

The `bare-key-chip-stability` row records a selection only. Cheese chose direction B (14 rem, 224 px) in the native quote on CHE-1827, comment `01a15750-f3e6-74f1-9675-771347dc32f2`, item 2. The commit in that row is the scored `rev-1` revision commit, not the commit that records the selection. No approval exists until PR 294 merges.
The `dm-wakeup-chip` row records a selection only. Cheese chose direction A in the native reply "A" on CHE-1874, comment `01a15952-f42e-7b9c-8b91-f8662101ff1a`. The commit in that row is the scored `rev-2` revision commit, not the commit that records the selection. The UX verdict for that commit is comment `01a158ba-682c-71b3-a029-92ea70549b96`. No approval exists until PR 323 merges.

The `dm-decision-quote-cards` row records a selection only. Cheese chose direction B (Receipt card) on CHE-1870, comment `01a160f2-1c1d-702e-b019-083e356949f4` ("I choose B, receive card"), corrected by comment `01a160f2-9379-7720-80c3-8eec261036fe`. B was scored in `rev-4` (comment `01a1603d-6c50-7c36-a806-f5a07d151c79`). The commit in that row is the `rev-8` revision commit. rev-5 added the visible "from the original DM" text to B and was scored (comment `01a16132-4c22-787a-8a97-4eb7fbc869f3`). rev-6 fixed the unmuted disclaimed card and the revision heading, and was scored (comment `01a16162-6394-7a98-b8a5-77dca41b6142`). rev-7 added the two Disclaim 409 states and was scored (comment `01a16191-f63a-7e2a-b551-e1c7940e1eca`). rev-8 replaces the raw colours of the conflict callouts with semantic token references and is not yet scored. The native request `f583b643-7097-43a9-a2f8-087a791f4916` still reads `open`. No approval exists until PR 316 merges.

The `own-comment-background` row records a selection only. Cheese chose direction C (fill + "You" label) on CHE-2133, comment `01a17b8a-b0d9-7b52-8123-83de7ddddd0c` ("C"). C scored 44/60 in the UX verdict, comment `01a177dd-00aa-78d1-8656-0848b58ab034`; the reviewer pick was B. The commit in that row is the scored `rev-1` revision commit, not the commit that records the selection. No approval exists until PR 405 merges.

The `dm-notifications-web` row records a selection only. Cheese chose direction A on CHE-1798 (comment `01a17c84-c39f-7b25-952e-b018529aa34d`: "A"). The commit in that row is the `rev-1` revision commit. No approval exists until PR 307 merges.
The `dm-search-web-desktop` row records a selection only. Cheese chose direction A (field above the list) on CHE-1794, native answer `A` in decision-record comment `01a160fd-22c9-75b8-9cb4-ca7421f09464` on ask `01a15fbf-91b0-7e0f-adc6-a56e398b8862`. Cheese then chose A on all four product choices (CHE-1794 comment `01a181fc-3992-777d-8315-c078f2150219`, "I choose A for four decisions", answering packet `01a16163-894a-70d8-a9fe-3915f38d7db4`): Back restores the query and results, keep the three-line result row, inline Unarchive on an archived hit, and bare `/` binds to DM search outside editable controls. The commit in that row is the scored `rev-3` revision commit, not the commit that records the selections. Its static UX verdict (x99-codex-luna) is CHE-1794 comment `01a1615c-86ec-7b9b-a19a-596efb694215`. No approval exists until PR 346 merges.

The `dm-wakeup-event-row` row records a selection only. Cheese chose direction C (thread rail) on CHE-2459, decision record `01a18753-e957-7ba2-a312-4400a43808a1`. The row names the doop frame `-Yr2vfvBHn` at `updatedAt` `2026-10-29T11:07:11.362Z`, locked in `design/dm-wakeup-event-row/approved/`. Its UX verdict (x99-agy-gemini-ux, 55/60) is CHE-2459 comment `01a1875a-c9e1-7656-a1a7-965a9b0d1628`. No approval exists until PR 581 merges.

The `you-are-approving-card` row records a selection only. Cheese chose direction B (tabbed viewer) on CHE-2611, decision record `01a18d7e-2504-73ce-9bba-a1cf0ef5c649`. The row names the doop frame `t8_q2WrMLL` at `updatedAt` `2026-10-30T12:12:16.518Z`, locked in `design/you-are-approving-card/approved/`. Its UX verdict (x99-codex-luna-design) is CHE-2611 comment `01a18cc3-3989-77c8-b495-e8bd0114dfc0`. Implementation must add tab and panel linkage (`aria-controls`) and arrow-key movement; the mockup does not show them. No approval exists until PR 664 merges.

The `machine-pause-banner` row records a selection only. Cheese chose direction B (one summary banner, count on the header icon) on CHE-2684, decision request `e8e5a058-1bb5-4afe-b731-aa89fcacc53b`, recorded as comment `01a18fda-6a9c-72c5-a3a4-f1bcd67ccea6`. The row names the doop frame `PX_YkzqAb-` at `updatedAt` `2026-10-31T03:10:54.092Z`, locked in `design/machine-pause-banner/approved/`. Its UX verdict (x99-codex-luna-design) is CHE-2684 comment `01a18ff2-ff1f-784c-a067-0c979f9c340b`. No approval exists until PR 684 merges.

The `hierarchy-minimap` row records a selection only. Cheese chose direction A (Tree) on CHE-2749, decision request `c3cf1f8d-f695-473b-a122-058b5accfd11`, comment `01a19285-0f08-7cbb-8cd6-8c5d8330ad16` ("A"), and accepted the semantic-zoom dialog frames D5 and D6 on decision request `73b93d4e-0816-499f-9656-cf3d104e0d13`, receipt `01a19293-4843-7853-8c95-9828c2a9c610`. The row names the doop frames at the `updatedAt` values above, locked in `design/hierarchy-minimap/approved/`. Its UX verdicts (x99-agy-gemini-ux) are listed in `LOCK.json`. The issue sidebar shows one Hierarchy minimap (parent, siblings and children, the current issue highlighted, leaf style D) in place of the Relations graph button and the Parent issue section; expanding it opens a pannable, zoomable canvas dialog that shows group totals below 70% zoom and single issues from 70%. PR 712 merged at `5f47069e175dca73d4e5ea0d78942d679cd67793`; CHE-2765, the last implementing PR, deletes the locked reference files. The dialog zoom (groups split at 70%) is superseded by the `timeline-views` row (CHE-4313).

The `artifact-viewer-dialog` row records a selection only. Cheese chose direction B (dialog replaces the inline Changes / Preview strip) on CHE-2810, decision request `ed911608-f115-4f1b-b455-aef59abf3448`, recorded as comment `01a19580-8cd4-7537-bd61-7b94239f23b8`. The row names the doop frame `Bi32qQ55Hi` at `updatedAt` `2026-11-01T04:43:09.622Z`, locked in `design/artifact-viewer-dialog/approved/`. Its UX verdict (x99-agy-gemini-ux) is CHE-2810 comment `01a19570-7789-7e31-b745-f95f1c476323`. No approval exists until PR 762 merges.

The `run-mode-chip` row records a selection only. Cheese chose direction C (summary chip with modes inline) on CHE-3066, decision request `161b2a5b`, recorded as comment `01a19b78-92b6-73e0-a63d-56ad3461879d`. The row names the doop frame `N4j_S11KaP` at `updatedAt` `2026-11-02T08:23:51.708Z`, locked in `design/run-mode-chip/approved/`. Its UX verdict (x99-agy-gemini-ux) is CHE-3066 comment `01a19b65-38f7-75ea-bd5d-3418b783635e`. Implementation must let the agent-name column flex or show a tooltip for names over 18 characters, and add a non-colour cue for the mode on the collapsed chip; the mockup shows neither. No approval exists until PR 815 merges.

The `dm-progress` row records a selection only. Cheese chose direction C (Attention first) on CHE-3407, decision request `1fb8d9d9-764c-4032-8e11-6850f7dc4af8`, recorded as comment `01a1ab0d-e919-7b79-87bf-a093a9a7b4af`. The row names the doop frame `V0nBnIQ-OI` at `updatedAt` `2026-11-04T12:00:37.964Z` and the shared-states frame `TiPMSzRj-T` at `2026-11-04T12:01:34.967Z`, locked in `design/dm-progress/approved/`. Its UX verdict (x99-agy-gemini-ux) is CHE-3407 comment `01a1a673-783f-76c4-b35a-d5e7bb320087`. The DM progress card opens with a verdict line, then tinted blocks (at most 3) for Blocked, In review and Done, and one-line rows for routine moves; the 4th such issue falls back to a one-line row. No approval exists until PR 1034 merges.

The `issue-sidebar-compact` row records a selection only. Cheese chose direction A (summary row) on CHE-3986, decision request `5ef6ed71-e1af-4a2e-98b2-4d500f30552d`, recorded as comment `01a1ab4e-d139-78ee-a909-d1cb43492c25`, with the note "C but adapt A", and approved the v2 reading in the issue run. The row names the doop frame `UVi0hFp3L_` at `updatedAt` `2026-11-05T10:56:09.927Z`, locked in `design/issue-sidebar-compact/approved/`. Its UX verdict (x99-agy-gemini-ux) is CHE-3986 comment `01a1ab5b-be84-74f7-9cfe-5098833dfa85`. The issue sidebar shows Pull requests above Wakeups; the default wakeup rules leave the list and sit behind one `Defaults · N on` button in the Wakeups header, which opens an inline panel (no popover or sheet) with a one-line row and a more menu per rule; a paused rule turns the button warm with an icon and the word "paused". Override takes three clicks from the closed state; the 390 px live capture was not taken. No approval exists until PR 1048 merges.

The `timeline-views` row records a selection only. Cheese chose "Recommended mix: Gantt B, Swimlane A, Hierarchy A, Sidebar A" on CHE-4313, decision request `af1dc36e-fabc-49e3-8f5f-10e8691b3638`, record comment `01a1b4b3-c28c-761e-8bc1-98834f375436`. The row names the doop frames at the `updatedAt` values above, locked in `design/timeline-views/approved/`; each frame holds two directions and the row names the locked one. Gantt v2 (B) draws a wakeup lane with a focus mode; Swimlane 2.0 (A) lays lanes by group-by over causal columns; the Hierarchy dialog (A) zooms by focus depth with a fan and a breadcrumb; the sidebar top card (A) shows a mini preview with an N-mode switch. UX verdicts (x99-agy-gemini-ux) are CHE-4313 comment `01a1b42c-a6b8-7385-b8c7-c0af079c76d3` for Gantt, Swimlane and Sidebar, and comment `01a1b4c7-12ab-78c2-b188-639996083873` for Hierarchy on its current version. The Hierarchy dialog zoom supersedes the 70% split of the `hierarchy-minimap` row (CHE-2749).

The `workflow-lint-ui` row records a selection only. Cheese chose direction D (Checks sheet) on CHE-3660, decision ask record `01a1b5d0-57af-7636-a7cc-eab5a9259538`, answer `01a1b684-52be-7c07-87fa-0037df7f2600`. The row names the doop frames at the `updatedAt` values above, locked in `design/workflow-lint-ui/approved/`. Its UX verdict (x99-codex-luna-design) is CHE-3660 comment `01a1b692-77fa-7ca1-8cff-6af646b7a3e2`. No approval exists until PR 1211 merges.

The `delegated-governed-writes` row records a selection only. Cheese chose direction B (read first, edit opens a live preview) on CHE-4616, decision request `d3c47807-0687-4e54-9473-cd76b0f14d6d`, recorded as comment `01a1ba7e-0503-7223-b312-b67987c5089b`. The row names the doop frame `Ocyo3e9IQD` at `updatedAt` `2026-11-08T09:29:28.407Z`, locked in `design/delegated-governed-writes/approved/`. Its UX verdict (x99-agy-gemini-ux) is CHE-4616 comment `01a1ba8a-9636-7775-83df-7924195c6a9b`, a re-verification after Cheese edited the frame (HTML byte-identical to the first PASS, `01a1ba08-0c84-7002-bf36-124056f02488`). The owner/admin-only Workspace settings section sets `delegated_governed_writes` for `agent.instructions`, `agent.skills` and `agent.model`. No approval exists until PR 1256 merges.

The `decision-card` row records a selection only. Cheese chose direction B v9 on CHE-5162 on 13 Nov 2026 08:27 ICT ("Chọn B v9, B thay fold A", decision card `c482d36e`, recorded in comment `01a1d284-a560-7d3e-86ea-e6fe32ad5e50`). B v9 supersedes fold direction A of CHE-4431 (the `decision-card-fold` row; design PR 1183 and code PR 1271 are already merged, so the build slices re-scope that code onto B v9). It also supersedes the v4 picks (`01a1cd63-7807-7887-ae08-22e9e410cf81`, `01a1cce0-7263-747f-ba85-d09e4ac44ae4`). The row names the doop frame `Z3bizs5RyO`, scored at `updatedAt` `2026-11-12T04:18:27.430Z` and locked in `design/decision-card/approved/` (content sha256 `300fd764…98d99`). Its UX verdict (x99-codex-luna-design) is CHE-5162 comment `01a1cdfe-0045-7601-a4e0-a94d9d82ea6e`. Two choices (Approve, Reject; pick one or neither), one always-present composer, and a button that names the result and its send key (Discuss more, Approve with passkey, Confirm approval, Confirm Reject). The action's `requiresPasskey` flag picks the Approve label; a typed word is never parsed. Implementation must add real radio and keyboard semantics and pick the final passkey glyph from the product icon set; the mockup shows neither. No approval exists until the PR in this row merges.

The `background-agents` row records a selection only. Cheese chose "Approve v2 after the two P2 fixes" on CHE-4122 on 13 Nov 2026 (decision card message `01a1d37d-7345-7c9b-b377-d19b8933e68e`, recorded in comment `01a1d38a-7f8b-74fb-87b9-00947319496a`). The row names the five v2 doop frames at the `updatedAt` values above (Workspace tab, Personal tab, Compact row, States, Agent page notice), locked in `design/background-agents/approved/` (export sha256 `60c3f9f2…c50fb6`). Its UX verdict (x99-codex-terra) is CHE-4122 comment `01a1d39a-0472-701d-9154-24333e55005c`; the PASS is render-scoped, and frame HTML, accessibility semantics, sloplint, privacy enforcement, interaction and component reuse were NOT-RUN. A personal pick wins over the workspace pick, and a failed or archived personal pick runs the no-agent path with no fall-through. Implementation must add real radio and select semantics, the server-side eligibility check on write and at run, and the `dm_title`, `quick_actions`, `compact` and `routing` key mapping; the mockup shows none of these. No approval exists until the PR in this row merges.
