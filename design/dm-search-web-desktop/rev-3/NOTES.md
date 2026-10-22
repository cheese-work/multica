## What changed vs rev-2

`git diff --no-index rev-2 rev-3` (exit 1 = differences). rev-3 holds direction A only. rev-1 and rev-2 are unchanged.

- Fixes the defect in the independent UX verdict `01a1613b-7e20-7b93-9194-3be03f44ac12` (x99-codex-luna, OpenAI `gpt-6-luna`, CHANGES REQUESTED): rev-2 put Pin, Archive and Unarchive buttons inside `role="option"` rows. The [WAI-ARIA APG Listbox Pattern](https://www.w3.org/WAI/ARIA/apg/patterns/listbox/) does not allow interactive descendants in options.
- Every result list in frames 4, 4b, 5, 11, 12 and 14 is now `role="list"` with `role="listitem"` rows. Each row has one open link and, as a sibling, a labeled action group. The open chat carries `aria-current="true"` instead of `aria-selected`.
- Row actions are shown on hover or keyboard focus within the row. They stay in the tab order when hidden.
- The seven verified rev-2 corrections stay: title hits first, labeled actions with the time visible, field-first offline frame, tokens, 24 px clear button, 16 px phone input, and actions on popover hits.
- No product choice changed. The four open choices are prepared as a packet below. Nothing was sent to Cheese.

## Directions

- A — Field above the list. The field is the first row under the header, in the page rail and in the floating window's history popover. Results replace the list in place, title hits first, then message hits. Escape or the clear button restores the list.

## The listbox defect

| Frame | rev-2 | rev-3 |
| --- | --- | --- |
| 4, 5, 11, 12 | `listbox` > `option` rows, hover strip inside the row text | `list` > `listitem` rows: `a.open` plus a sibling action group |
| 4b | Unarchive button inside an option | Unarchive button beside the open link |
| 14 | `listbox` with Pin and Archive inside the option | `list` with the same sibling structure |

The reviewer's seven other correction checks passed on rev-2 and are kept. Reviewer correction 6 asked for a labeled list in frame 14. It is now the same labeled list in every result frame.

## Open product choices (prepared, not sent)

rev-3 approves none of these. The recommendations are the reviewer's, from verdict `01a1613b-7e20-7b93-9194-3be03f44ac12`. Cheese owns each choice.

| ID | Question | Option A | Option B | Reviewer recommends |
| --- | --- | --- | --- | --- |
| `dm-search-web-desktop-back` | After Back from an opened result? | Restore the query and results (frame 11). The query then needs a place to live across history entries. | Return to the plain list, query cleared (frame 11b). Matches L2, where the URL holds only the open chat. | A |
| `dm-search-web-desktop-row-height` | Result row height? | Keep the taller three-line row (drawn). Fewer hits per screen. | 56 px rows with a one-line snippet beside the agent. Not drawn. | A |
| `dm-search-web-desktop-archived-action` | Action on an archived hit? | Unarchive beside the open link (frame 4b). | No inline action. Open the Archived view. | A |
| `dm-search-web-desktop-slash` | The `/` shortcut? | Bind bare `/` to DM search outside editable controls. A new shortcut registry action. | Leave it unbound and remove the `/` hint. | B |

Delete stays in the Archived view only, under either archived-hit option (L2). The frames still draw the `/` hint, because the choice is open.

## Part 2 — settled sources

S1 and S2 text in `docs/designs/dm-search-notifications-push.md` (accepted amended L0 design, PR 271 at `e4e262ce4469938b831b470d29fdac27cee67f5b`); published L2/L3 branch (PR 340, head `b6579e107ae0149b57f691416287b94df78a70e8`); `packages/core/shortcuts/definitions.ts` at base `c92ac3ae2b885a9e16875b215b9452f328351bed`.

- Closed for A: the `N in this chat` count. S1 returns no count and A draws none.
- Settled: S1 includes archived hits and marks them. L2 keeps the composer on an archived chat and asks before a send unarchives it, so an opened archived chat is not read-only.
- Settled: S2 requires title, agent, snippet with the match highlighted and time, and a pushed history entry consistent with L3. S2 sets no row height and no query retention.
- Settled at the base: bare `/` is not bound. `Mod+/` toggles the right sidebar and `Mod+K` opens global search.

## Required states

Default, focused, searching, results, show more, no results, failed, timed out, disconnected, query too long, opened at message, message gone. Floating window: default, results, no results, failed. Extra frames: 4b, 11b.

## Accessibility limits

- Roles drawn: `search`, `searchbox` with a name, a polite `status` count, `list` and `listitem` rows, an open link per row, labeled action buttons, `aria-current` on the open chat. The highlight is a text mark plus the surrounding words, not colour alone.
- Keyboard model proposal: Tab moves open link, Pin, Archive per row. Arrow-key movement between rows is a code decision.
- Touch targets at the phone width are proposals (32 to 44 px), not current code.
- There was no screen-reader pass, no keyboard run and no dynamic-type check. Do not read this as an accessibility pass.

## Verification limits

- Static HTML from `packages/ui/styles/tokens.css` values. The app did not render it.
- Current screens: `NOT-CAPTURED`. `visual diff: NOT-RUN`. Native and desktop (Electron) behavior, runtime keyboard behavior and any live search are `NOT-RUN`.
- The sample data is invented. Highlighting is drawn on the whole query.
- The two PNGs are 256-colour palette files, full page, captured on X99 with headless Chrome at 1440 px and 390 px.
