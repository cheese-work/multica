## Source

Derived from the repository rules in `AGENTS.md` (UI Copy, Web/Desktop UI Rules) and the tokens in `packages/ui/styles/tokens.css`. It records no decision beyond them. Cheese approves a design by merging its design PR.

## UX principles

- Descriptions are optional. Add one only for a non-obvious choice, constraint, consequence or next step, and state each fact once beside its control.
- Keep permissions, cost, destructive consequences, execution prerequisites and error recovery visible.
- Selected states stay identifiable on hover. Handle overflow, long text and scrolling on purpose.
- Prefer the existing shadcn/Base UI primitives.

## Tokens

Use semantic tokens from `packages/ui/styles/tokens.css`: surface, border, muted-foreground, primary, destructive, warning, and the role-named `--text-*` type scale (caption 12 px, body 14 px, title-sm 16 px, title 18 px). Mockups copy the token values and never invent colors.

## Patterns

- Settings tab: a section title, an optional one-line description, then a card of rows. A row has a label, an optional description and one control on the right.
- Notices inside a tab or panel use the muted notice for information and the warning tone for a state that stops work.
- Admin-only controls stay visible to members, disabled, with a one-line reason.

## Required states

Every control that saves shows: idle, saving, saved, save failed (with recovery), disabled for a member, and the empty or error state of the data it needs.

## Approved revisions

| Topic | Revision | Direction | Design PR | Revision commit |
| --- | --- | --- | --- | --- |
| wakeups-pause | 3 | A | 255 | `de646d2bb8c50bb93653d98395f17f3dc2e0ceee` |

The row is written before the merge, as the standard requires. The design is approved when Cheese merges PR 255. The merge commit is read from GitHub and is not written here.
