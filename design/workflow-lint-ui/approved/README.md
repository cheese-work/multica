Locked from doop page `OXAkiRKOeX:page1` (test canvas https://congvc-c00.taila6fa8a.ts.net:34443/c/OXAkiRKOeX), five frames, each at the `updatedAt` in `LOCK.json`. Issue CHE-3660 (feature CHE-3658, workflow lint).
Direction D (Checks sheet) selected by Cheese (decision ask record `01a1b5d0-57af-7636-a7cc-eab5a9259538`, answer `01a1b684-52be-7c07-87fa-0037df7f2600`). UX PASS on these exact versions (`01a1b692-77fa-7ca1-8cff-6af646b7a3e2`, x99-codex-luna-design, OpenAI; the designer is Anthropic).
Approved when the lock PR merges. The topic's last implementing PR (layer 5 of CHE-3658) deletes these reference files.

- `d-checks-sheet-light.html`, `d-checks-sheet-dark.html`: Settings → Workflows → Delivery with the Checks sheet open (440 px, right side). Header button "Checks" shows error, warning and note counts. Step cards show a badge per level (icon plus number). Sheet: filter tabs All / Errors / Warnings / Notes with counts, findings grouped by step, each with message, "Fix:" hint, rule ID, "Waive…" and "Go to step". A waived finding shows muted with the reason, who and when, and "Undo waive".
- `dialog-save-refused-light.html`, `dialog-save-refused-dark.html`: save refusal. Lists the unwaived errors, then a required reason field (0 / 500) and "Save anyway" (disabled until a reason is typed) beside "Back to editing".
- `states-checks-panel-light.html`: Clean, Running, Read-only member, and Checks failed to run (saving stays allowed; the server checks again on save).
- Files are the frames as read from doop, unchanged. Hashes are in `LOCK.json`.

## Implement and test

- The findings body scrolls under a fixed header and footer.
- Keyboard order, visible focus and screen-reader semantics for the filter tabs (`tablist`), "Go to step", "Waive…" and close.
- Level is an icon and a word or number, never colour alone.
- The Checks button count and the step badges read from the same lint result as the sheet.
- Draft state: the override reason keeps its text across close and reload until a successful save or explicit discard.
- Times in the UI carry the ICT suffix, as in the mockup.

## Not covered by this lock

- The frames sit on the test canvas, not on the project file "Cheese Work Setup", and on `page1`; no proposal page was created.
- No 390 px mobile width.
- The states frame is light only.
- No interaction, keyboard or screen-reader test was run on the mockup.
- Hindsight design-direction recall was not done.
