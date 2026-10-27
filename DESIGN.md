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
- Web notification settings (`dm-notifications-web` rev-1 A): Settings → Notifications has Inbox and Direct messages sections, each led by In-app and Push rows, then a This device section for browser permission and push registration. Web Inbox DM rows get a chat glyph and filter chips (All, Unread, Direct messages). Banners carry the sender and "New reply" / "N new replies" / "Decision needed" only.

## Approved revisions

| Topic | Revision | Direction | Design PR | Revision commit SHA |
| --- | --- | --- | --- | --- |
| wakeups-pause | 3 | A | 255 | `de646d2bb8c50bb93653d98395f17f3dc2e0ceee` |
| jev-governance-tab | 4 | C | 268 | `a32ec00a3ee38cbdf9336c3c953f7cf840ace38b` |
| dm-notifications-mobile | 1 | A | 277 | `a768e4532ac3f193ece7b552308a95eabe97619b` |
| dm-notifications-web | 1 | A | 307 | `0b7065192a3d456ef5bd06173aff361ae0924387` |

Each row is written before its merge, as the standard requires. A design is approved when Cheese merges its PR: PR 255 for `wakeups-pause`, PR 268 for `jev-governance-tab`, PR 277 for `dm-notifications-mobile`, PR 307 for `dm-notifications-web`. The merge commit is read from GitHub and is not written here.

The `jev-governance-tab` row records a selection only. Cheese chose direction C at 03:18 ICT on 16 Oct 2026 (CHE-1780 comment `01a14136-ef91-73e1-867f-41c3747e6ad7`: "C  is direction"). The commit in that row is the `rev-4` revision commit. No approval exists until PR 268 merges.

The `dm-notifications-mobile` row records a selection only. Cheese chose direction A on CHE-1811 (comment `01a14243-3674-7796-aaf2-b9354a94065e`). The commit in that row is the `rev-1` revision commit. No approval exists until PR 277 merges.

The `dm-notifications-web` row records a selection only. Cheese chose direction A on CHE-1798 (comment `01a17c84-c39f-7b25-952e-b018529aa34d`: "A"). The commit in that row is the `rev-1` revision commit. No approval exists until PR 307 merges.
