## What changed vs the current screens

rev-1 is the first revision, so it compares against today's `main` (`d4978e0c0`), read from code. Live baseline screenshots are NOT-CAPTURED: no running web build of this checkout on X99 in this run.

- Today the ping (`message_kind` `decision_request`) and the note (`decision_reference`) render as plain assistant text. The note shows raw lines such as `Disclaim: /api/decision-answers/<id>/disclaim` (`server/internal/handler/decision_record.go:297`). There is no Disclaim button and no raise-class control.
- Today the veto note (`decision_veto`) is plain text with the reason (`decision_veto_notice.go:118`).
- Today the issue quote is an ordinary agent comment with a Markdown blockquote. The frontend schema knows `decision_comment_kind` (`ask`, `quote`, `disclaim`, `delivery`, `cancel`, `veto`) but renders nothing for it (`packages/core/api/schemas.ts`). No struck state, no permalink.
- Today Inbox has no tabs and no list of open requests. `GET /api/decisions` exists (layer 1) with no UI.
- All three directions add the same states: pending, on hold, in effect, disclaimed, vetoed, cancelled, unprompted record, archived chat, original removed, other viewer, loading, empty, error.

## Directions

- A — Cards in place: a ping card and a note card in the transcript, a Decision header on the issue comment, an Inbox tab with a detail pane.
- B — Quiet transcript, pinned asks: the ask is an agent message with an eyebrow, open asks pin above the composer (with the class control), notes are system rows, the issue quote carries a progress line, Waiting on you is a full-width list that opens the DM.
- C — Decisions panel: a per-DM ledger (bottom sheet on phone) lists open, recorded and closed requests with every action; the transcript keeps compact chips; the issue quote is a structured record with answer history; Waiting on you is a dense table.

## Limits

- Mock data and static HTML. No live behaviour, no screen reader pass, no real keyboard run.
- Raise class follows the server rule: only from Reversible to one of the five hold classes (`server/internal/handler/decision_class.go`, 409 otherwise). A held class shows a lock and no menu.
- The permalink opens only for the DM's own person; everyone else sees the recorded copy (design Flow 8).
- PNGs are full-page captures (Playwright 1.58.0, viewports 1440×900 and 390×844), palette-quantized to 128 colours without dithering to stay under the 300 KB CI image budget.
- C hides the panel below 640 px; frame 2b shows its bottom-sheet form. A hides the Inbox detail pane on phone.
