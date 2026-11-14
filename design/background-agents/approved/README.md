Locked from doop page `mRAnnjvGod:page1` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod), five v2 frames at the `updatedAt` values in `LOCK.json`. Issue [CHE-4122](mention://issue/01a1abac-ec1a-7710-bdfe-21345481b88d), part of CHE-4013 (plan r4 step 25).
Cheese chose "Approve v2 after the two P2 fixes" on a decision card (13 Nov 2026), recorded in CHE-4122 comment `01a1d38a-7f8b-74fb-87b9-00947319496a`. UX PASS (x99-codex-terra, 13 Nov 2026 13:30:48 ICT, comment `01a1d39a-0472-701d-9154-24333e55005c`): Clarity 8, Hierarchy 8, States 8, Consistency 8, Accessibility 7, Density 8. Both P2 notes closed.
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

## What is locked

| File | Frame | Shows |
| --- | --- | --- |
| `a2-workspace-tab.html` | A2 `9L657IkfjZ` | Settings → Workspace → Background agents. Rows DM title, Quick actions, Compact, Routing. Agent and Fallback agent selects. Scope badge. Owner and admin edit. |
| `b2-personal-tab.html` | B2 `XyqcwrCaU8` | Settings → Personal → Background agents. Own picks for DM title, Quick actions, Compact (no Routing). "Using workspace" unset state. Archived personal pick: no fall-through, one explicit button back to the workspace pick. Agent select open. |
| `c2-compact-row.html` | C2 `IZ5zGPF9ZM` | Compact row. Transcript disclosure. "Runs with <agent> · <scope>" line (Personal, Workspace, Off). Archived and narrowed-access warnings. Status row (Normal, Fallback, Waiting, Failed). `/compact` hint above the input. |
| `d2-states.html` | D2 `fLyQE0xYpu` | Both tabs: loading, empty (owner/admin and member), load error 503, offline, member read-only, save conflict 409. |
| `e-agent-page-notice.html` | E `ijhJ5xbn0u` | Agent page. "Background agent" notice with scope badge, link and job key per job. "Who can run this agent" note and pre-save warning. Member view. Banner stack. |

Each file has a 1440 px column and a 390 px phone column. Each file is the frame HTML as doop returned it, unchanged; hashes are in `LOCK.json`. The HTML is a self-contained mockup (system font stack, inline SVG, no external fetch). Agent names are sample data.

## Rules the frames show

- Scope is always visible: a Workspace or Personal badge in every state, errors included. The scope words are `workspace` and `personal`, the same as the CLI.
- Unset is "No agent". Each job says what happens with no agent.
- A personal pick wins over the workspace pick for that member. An unset personal row says "Using workspace: <agent>".
- A failed or archived personal pick runs the no-agent path. The workspace agent and the personal fallback do not take over. One explicit button returns to the workspace pick.
- Fallback agent runs only when the primary agent fails to claim the job. It is disabled until a primary agent is picked.
- Routing is Workspace only. Compact shows only when CHE-4121 ships.
- Members see Workspace picks read-only and set their own on the Personal tab. Nobody sees another member's personal picks, admins included.
- Access narrowing on the agent page warns before save and names the jobs it affects. Saving stays allowed.

## Implementation must add and test

- Real radio semantics on the agent page access options: `role="radiogroup"`, arrow-key movement, `aria-checked`, a roving tabindex. The mockup spans carry `aria-checked` only.
- Accessible select semantics for the Agent and Fallback agent selects: the mockup buttons carry `aria-label`, `aria-expanded` and `aria-haspopup` only, and the open list uses `role="listbox"` and `role="option"` without keyboard behaviour.
- Server check that the picked agent is workspace-eligible (not archived, on a runtime, runnable by everyone) on write and again when the job runs. The client warning is a hint, not the rule.
- Key mapping: `dm_title`, `quick_actions` and `compact` for Workspace and Personal; `routing` for Workspace only. The agent page jobs list shows these keys.
- Personal pick wins over the workspace pick. A failed or archived personal pick runs the no-agent path, with no fall-through.
- Privacy: the agent page shows only workspace uses and the viewer's own personal uses, for every role.
- 409 handling on the Workspace tab (compare, keep theirs, apply mine again) and the "another tab" banner on the Personal tab.
- The `/compact` hint stays above the input and clear of the phone keyboard bar.
- All copy needs every supported translation. Status is an icon and a word, never colour alone.
- Token alignment: the mockup uses raw `oklch` values; the code slice maps them to semantic tokens.

## Not covered by this lock

- The UX PASS is render-scoped (the five PNGs). Frame HTML, accessibility semantics, sloplint, privacy enforcement, interaction and component reuse were NOT-RUN.
- No live keyboard, screen-reader or touch flow was tested (static HTML).
- Light theme is not mocked.
- The v1 frames stay on the canvas as history and are not locked.
