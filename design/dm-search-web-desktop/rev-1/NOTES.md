## What changed vs rev-0

rev-0 is the current Chat page and floating window. They were not captured: they need a signed-in session, which this task does not have, so the baseline is `NOT-CAPTURED`. The mockups follow the source of the published CHE-1755 L2 branch (PR #340 at head `b6579e107ae0149b57f691416287b94df78a70e8`, files `chat-thread-list.tsx`, `chat-page.tsx`, `chat-window.tsx`) and the approved S2 text. Compared with rev-0:

- A search field appears in the DM list (page rail and the popover). While a query is active, results replace the list. Escape or the clear button restores it.
- A result is one row per chat: agent avatar, title, agent, a one-to-two line snippet with the match highlighted, time and an Archived badge. A title-only hit has no snippet and says "Title match". Pin, archive and the hover actions stay on result rows.
- Selecting a result opens the chat at the matching message, scrolls to it and highlights it. A history entry is pushed. If the message is gone, the chat opens at the latest messages with a notice.
- Every direction shows: default, focused, searching, results, show more (cursor), no results, failed, timed out, disconnected, query too long, opened at message, message gone, and the floating window in four states.

Facts used from the server (S1, `ChatSearchHit`): `title`, `agent_id`, `message_id` (null for a title-only hit), a `snippet` of about 160 characters that starts 40 before the match, `updated_at` and `archived`. Only readable chats are returned. The default page is 20, the maximum is 50, and a query is at most 200 characters. A 503 means the search timed out.

## Directions

- A — Field above the list. The field is the first row under the header. Results use one flat list.
- B — Search icon that expands, results in two groups. The 260 px rail keeps its header. Results split into Titles and Messages.
- C — Keyboard-first field with a live selection. The field takes the header row and New chat moves beside it. Up, Down and Enter drive a visible selection, a footer lists the keys, and a row shows how many matches the chat has.

## Decisions for Cheese

1. Direction: A, B or C.
2. After Back from an opened result: keep the query and results (drawn), or return to the plain list.
3. Result rows are taller than the fixed 56 px list rows (a snippet needs two lines). The mockups accept that for results only.
4. "N in this chat" (C) needs a count the API does not return. Dropping it keeps C without any server change; adding it is a server change.
5. Archived chats appear in results with a badge. Opening one is read-only, as today.
6. The search field takes the `/` shortcut when no input has focus (drawn as a hint). Drop it if it clashes with an existing shortcut.

## Required states, in every direction

Default, focused, searching, results, show more, no results, failed, timed out, disconnected, query too long, opened at message, message gone. The floating window shows default, results, no results and failed. The compact (phone-width) layout reuses the same frames in one column; there the opened chat replaces the list.

## Accessibility limits

- The field is a search landmark with an accessible name. The result count is a polite live region. Results are a listbox with `aria-selected`. The highlight is a text mark plus the surrounding words, not colour alone.
- Touch targets in the phone frames are 40 to 44 px as a proposal, not current code.
- There was no screen-reader pass, no keyboard run and no dynamic-type check. Do not read this as an accessibility pass.

## Verification limits

- Static HTML from `packages/ui/styles/tokens.css` values, copied through the `jev-governance-tab` rev-4 stylesheet. The app did not render it. The font is the browser system font and the icons are simplified.
- Current screens: `NOT-CAPTURED`. `visual diff: NOT-RUN`. Native and desktop (Electron) behavior, runtime keyboard behavior, and any live search are `NOT-RUN`.
- The sample data is invented. Highlighting is drawn on the whole query; the real client may also highlight partial words, which is a code decision.
- The six PNGs are 256-colour palette files, full page, captured on X99 with headless Chrome at 1440 px and 390 px. Each is under 300 KB.
