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
| dm-search-web-desktop | 3 | A | 346 | `78d53cf894f7a89beff98c0e9af7c94a7c69c905` |
| dm-wakeup-event-row | doop `-Yr2vfvBHn` @ 2026-10-29T11:07:11.362Z | C | 581 | n/a (`design/dm-wakeup-event-row/approved/`) |
| machine-pause | doop `AvexgrxkBp` @ 2026-10-29T15:15:52.443Z | A | PR_NUM | n/a (`design/machine-pause/approved/`) |

Each row is written before its merge, as the standard requires. A design is approved when Cheese merges its PR: PR 255 for `wakeups-pause`, PR 268 for `jev-governance-tab`, PR 277 for `dm-notifications-mobile`, PR 284 for `dm-stats-archive-menu`. The merge commit is read from GitHub and is not written here.

The `jev-governance-tab` row records a selection only. Cheese chose direction C at 03:18 ICT on 16 Oct 2026 (CHE-1780 comment `01a14136-ef91-73e1-867f-41c3747e6ad7`: "C  is direction"). The commit in that row is the `rev-4` revision commit. No approval exists until PR 268 merges.

The `dm-notifications-mobile` row records a selection only. Cheese chose direction A on CHE-1811 (comment `01a14243-3674-7796-aaf2-b9354a94065e`). The commit in that row is the `rev-1` revision commit. No approval exists until PR 277 merges.

The `dm-stats-archive-menu` row records a selection only. Cheese chose direction C on CHE-1782 (comment `01a15844-7da8-78f5-880b-7f891dde2142`: "C"). The commit in that row is the `rev-2` revision commit. No approval exists until PR 284 merges.

The `bare-key-chip-stability` row records a selection only. Cheese chose direction B (14 rem, 224 px) in the native quote on CHE-1827, comment `01a15750-f3e6-74f1-9675-771347dc32f2`, item 2. The commit in that row is the scored `rev-1` revision commit, not the commit that records the selection. No approval exists until PR 294 merges.
The `dm-wakeup-chip` row records a selection only. Cheese chose direction A in the native reply "A" on CHE-1874, comment `01a15952-f42e-7b9c-8b91-f8662101ff1a`. The commit in that row is the scored `rev-2` revision commit, not the commit that records the selection. The UX verdict for that commit is comment `01a158ba-682c-71b3-a029-92ea70549b96`. No approval exists until PR 323 merges.

The `dm-decision-quote-cards` row records a selection only. Cheese chose direction B (Receipt card) on CHE-1870, comment `01a160f2-1c1d-702e-b019-083e356949f4` ("I choose B, receive card"), corrected by comment `01a160f2-9379-7720-80c3-8eec261036fe`. B was scored in `rev-4` (comment `01a1603d-6c50-7c36-a806-f5a07d151c79`). The commit in that row is the `rev-8` revision commit. rev-5 added the visible "from the original DM" text to B and was scored (comment `01a16132-4c22-787a-8a97-4eb7fbc869f3`). rev-6 fixed the unmuted disclaimed card and the revision heading, and was scored (comment `01a16162-6394-7a98-b8a5-77dca41b6142`). rev-7 added the two Disclaim 409 states and was scored (comment `01a16191-f63a-7e2a-b551-e1c7940e1eca`). rev-8 replaces the raw colours of the conflict callouts with semantic token references and is not yet scored. The native request `f583b643-7097-43a9-a2f8-087a791f4916` still reads `open`. No approval exists until PR 316 merges.

The `own-comment-background` row records a selection only. Cheese chose direction C (fill + "You" label) on CHE-2133, comment `01a17b8a-b0d9-7b52-8123-83de7ddddd0c` ("C"). C scored 44/60 in the UX verdict, comment `01a177dd-00aa-78d1-8656-0848b58ab034`; the reviewer pick was B. The commit in that row is the scored `rev-1` revision commit, not the commit that records the selection. No approval exists until PR 405 merges.

The `dm-search-web-desktop` row records a selection only. Cheese chose direction A (field above the list) on CHE-1794, native answer `A` in decision-record comment `01a160fd-22c9-75b8-9cb4-ca7421f09464` on ask `01a15fbf-91b0-7e0f-adc6-a56e398b8862`. Cheese then chose A on all four product choices (CHE-1794 comment `01a181fc-3992-777d-8315-c078f2150219`, "I choose A for four decisions", answering packet `01a16163-894a-70d8-a9fe-3915f38d7db4`): Back restores the query and results, keep the three-line result row, inline Unarchive on an archived hit, and bare `/` binds to DM search outside editable controls. The commit in that row is the scored `rev-3` revision commit, not the commit that records the selections. Its static UX verdict (x99-codex-luna) is CHE-1794 comment `01a1615c-86ec-7b9b-a19a-596efb694215`. No approval exists until PR 346 merges.

The `dm-wakeup-event-row` row records a selection only. Cheese chose direction C (thread rail) on CHE-2459, decision record `01a18753-e957-7ba2-a312-4400a43808a1`. The row names the doop frame `-Yr2vfvBHn` at `updatedAt` `2026-10-29T11:07:11.362Z`, locked in `design/dm-wakeup-event-row/approved/`. Its UX verdict (x99-agy-gemini-ux, 55/60) is CHE-2459 comment `01a1875a-c9e1-7656-a1a7-965a9b0d1628`. No approval exists until PR 581 merges.
