Locked from doop page `G--GFZSHv9:page1` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/G--GFZSHv9), frame `Ocyo3e9IQD`, `updatedAt` `2026-11-08T09:29:28.407Z`. Issue [CHE-4616](mention://issue/01a1b9f7-ac81-7929-8f70-8096c520c34b), part of CHE-4445.
Direction B (read first, edit opens a live preview) selected by Cheese (decision request `d3c47807-0687-4e54-9473-cd76b0f14d6d`, recorded as comment `01a1ba7e-0503-7223-b312-b67987c5089b`: "B: read first, edit opens a live preview"). Cheese edited frame B after the first UX PASS; the cross-lab re-verification found the HTML byte-identical to the scored version. UX PASS on this exact version (`01a1ba8a-9636-7775-83df-7924195c6a9b`, x99-agy-gemini-ux); the first PASS for both directions is `01a1ba08-0c84-7002-bf36-124056f02488`.
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `direction-b.html`: the frame as read from doop, unchanged. Its hash is `screens[0].content_sha256` in `LOCK.json`.
- Shows: a Workspace settings › Governance card "Delegated governed writes". It opens on one sentence ("N agents may write M fields. Each change waits in a decision card.") and an agent-by-field grid (`agent.instructions`, `agent.skills`, `agent.model`). "Edit rule" opens three inputs under the grid: allowed agents (chips plus Add agent), allowed fields (checkboxes), Requires human decision (switch). The grid redraws as inputs change and marks new grants with a "New" tag.
- Six states: 1 owner, saved value after reload; 2 editing with live preview; 3 empty (Set up delegation); 4 member, read-only with a lock note and no Edit button; 5 Requires human decision off (warm summary, "Writes directly" cells, alert, confirm checkbox, Save disabled until ticked); 6 save refused (error names the archived agent and the fix, input kept).

## Implementation must add and test

- Only workspace owners and admins can edit; members see the grid and the lock note.
- The agent list and field list in the mockup are a sample catalog (assumption). Use the real agents and the real governed fields.
- Add `aria-hidden="true"` to decorative SVGs (`sloplint 33`).
- Add explicit `:focus-visible` styles (`sloplint 26a`).
- Disabled buttons need a style other than opacity alone (`sloplint 39a`).
- Chip remove buttons name the agent in `aria-label` (State 2 chips say only "Remove").
- State is an icon and a word, never colour alone.
- The warn text colours are mockup tokens; check contrast against the real tokens.
- The rule that a settings change goes through a decision card with a diff is out of scope here (CHE-2949 phase E).

## Not covered by this lock

- Direction A (`mQBjq6iiMA`) was not selected and is not locked.
- No per-proposal canvas page was created; frames sit on the test canvas `page1`.
- No live capture of the real settings page and no standalone browser render check were taken (NOT-CAPTURED, NOT-RUN).
- Dark theme, phone width and loading states are not mocked.
