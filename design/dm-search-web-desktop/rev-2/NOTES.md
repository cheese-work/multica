## What changed vs rev-1

`git diff --no-index rev-1 rev-2` (exit 1 = differences). rev-2 holds direction A only. rev-1 is unchanged.

- Cheese picked A: "A" (decision record, CHE-1794 comment `01a160fd-22c9-75b8-9cb4-ca7421f09464`, 07:22:44 ICT, 22 October 2026). A selection is not a design approval.
- The eight reviewer corrections for A are applied (table below). The reviewer is x99-agy-argon (Google, runtime provenance UNVERIFIED), verdict comment `01a15fb1-e258-769c-8b29-88e9676e6d36`.
- Two frames are added: 4b (archived hit, hover) and 11b (Back, option 2).
- The rev-1 claim "opening an archived chat is read-only" was wrong. L2 keeps the composer; sending asks first. See the reconciliation.
- The per-chat match count belongs to C only. A never drew it.

## Directions

- A — Field above the list. The field is the first row under the header, in the page rail and in the floating window's history popover. Results replace the list in place, title hits first, then message hits. Escape or the clear button restores the list.

## Part 1 — the eight corrections

| # | Reviewer finding for A | rev-2 change |
| --- | --- | --- |
| 1 | A message hit sat above a title hit, against S1 ranking. | Title hits come first in every result frame (4, 4b, 5, 11, 12, 14), then message hits by recency. S1: "Ranked: title matches first, then most recent message match." |
| 2 | Hover actions were unlabeled and hid the timestamp. | Pin and Archive are `button`s with `aria-label` and `title`, grouped as "Chat actions". They sit on the agent line, so the time stays visible. |
| 3 | Frame 9 put the offline notice above the input. | The field stays first. The notice sits under it, as in the error frames 7, 8 and 16. |
| 4 | Popover hits dropped the row actions. | Frame 14 shows Pin and Archive on the hovered row. |
| 5 | Placeholder used `--faint-foreground`, raw `#fff`, no `--find-match`. | Placeholder uses `--muted-foreground` (the repo `Input` uses `placeholder:text-muted-foreground`). `#fff` is now `var(--surface)`. The highlight uses `--find-match` and `--find-match-foreground`, copied from `packages/ui/styles/tokens.css`. |
| 6 | Frame 14 had no `role="listbox"`. | The popover results sit in `role="listbox"` `aria-label="Search results"`. |
| 7 | The desktop clear button was 20 px. | 24 px. The phone width keeps 32 px. |
| 8 | The phone input stayed at 14 px. | 16 px at the phone width, matching the `base.css` rule for narrow or coarse-pointer screens (`font-size: 16px !important`). |

## Part 2 — product questions, reconciled against settled sources

Sources: S1 and S2 text in `docs/designs/dm-search-notifications-push.md` (accepted amended L0 design, PR 271 at `e4e262ce4469938b831b470d29fdac27cee67f5b`); published L2/L3 branch (PR 340, head `b6579e107ae0149b57f691416287b94df78a70e8`); `packages/core/shortcuts/definitions.ts` at base `c92ac3ae2b885a9e16875b215b9452f328351bed`. "Settled" means a source states it. "Open" means no source decides it and Cheese or a product decision owns it. rev-2 approves none of the open items.

| # | Question | Status | Settled constraint, or why it is open |
| --- | --- | --- | --- |
| 1 | Direction | Closed | Cheese picked A. |
| 2 | After Back from an opened result: keep the query, or return to the plain list? | Open | S2: opening "pushes a history entry, consistent with CHE-1755 L3." In L2, the URL owns the open chat and a user selection pushes an entry, so Back walks list, A, B (`chat-page.tsx`). The query is not in the URL or the store, so keeping it needs a new place to hold it. Frame 11 draws "keep" (option 1). Frame 11b draws "plain list" (option 2). Neither is chosen. |
| 3 | Result rows are taller than the fixed 56 px list rows. | Open | L2 rows are `h-14` (56 px) with two lines. S2 requires title, agent, snippet with the match highlighted, and time on each result, but sets no height. A 56 px result would need a one-line snippet beside the agent. rev-2 keeps taller result rows, as drawn in rev-1. |
| 4 | `N in this chat` count. | Closed for A | S1 returns per hit: session id, title, agent, matched message id, snippet, `updated_at`, archived flag. It returns no count. A draws none. |
| 5 | Archived chats in results. | Settled in part | Settled: S1 "Archived DMs are included and marked archived." Settled: L2 keeps the composer on an archived chat; sending asks first, then unarchives and sends (`use-archived-send.tsx`). So opening one is not read-only. Open: the hover action on an archived hit. rev-2 draws Unarchive only (frame 4b), because L2 keeps Delete in the Archived view. That is a proposal. |
| 6 | The `/` shortcut. | Open | At the base, bare `/` is not bound. `Mod+/` toggles the right sidebar, `Mod+K` opens global search, bare `C` creates an issue and bare `E` archives an inbox item. Adding bare `/` is a new registry action, so it is a product choice. The frames draw the `/` hint as a proposal. |

## Required states, in every frame set

Default, focused, searching, results, show more, no results, failed, timed out, disconnected, query too long, opened at message, message gone. Floating window: default, results, no results, failed. Extra frames: 4b, 11b.

## Accessibility limits

- Roles drawn: `search`, `searchbox` with a name, a polite `status` count, `listbox` and `option` rows with `aria-selected`, labeled action buttons. The highlight is a text mark plus the surrounding words, not colour alone.
- Touch targets at the phone width are proposals (32 to 44 px), not current code.
- There was no screen-reader pass, no keyboard run and no dynamic-type check. Do not read this as an accessibility pass.

## Verification limits

- Static HTML from `packages/ui/styles/tokens.css` values. The app did not render it. The font is the browser system font and the icons are simplified.
- Current screens: `NOT-CAPTURED`. `visual diff: NOT-RUN`. Native and desktop (Electron) behavior, runtime keyboard behavior and any live search are `NOT-RUN`.
- The sample data is invented. Highlighting is drawn on the whole query.
- The two PNGs are 256-colour palette files, full page, captured on X99 with headless Chrome at 1440 px and 390 px.
