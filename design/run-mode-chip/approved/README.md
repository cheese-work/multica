Locked from doop page `mRAnnjvGod:page1` (file https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod), frame `N4j_S11KaP`, `updatedAt` `2026-11-02T08:23:51.708Z`. Issue [CHE-3066](mention://issue/01a19b39-6ef5-747b-8621-544994a072d4).
Direction C (one summary chip, modes inline in the popover) selected by Cheese (decision request `161b2a5b`, recorded as comment `01a19b78-92b6-73e0-a63d-56ad3461879d`), UX PASS on this exact version (`01a19b65-38f7-75ea-bd5d-3418b783635e`, x99-agy-gemini-ux).
Approved when the lock PR merges. The topic's last implementing PR deletes these reference files.

- `run-mode-chip-c.html`: the frame as read from doop, unchanged. Its hash is `screens[0].content_sha256` in `LOCK.json`.
- Shows: one summary chip for 1, 2, 3 and 5 recipients; desktop 1440 popover with a four-way mode control per agent, a description line, and the safe-triangle area; iPhone 390 anchored non-modal popover; a 48-character agent name beside a blocked-mention chip.

## Conditions from Cheese's pick

Build requirements. The mockup does not show them, and frame C was not edited for them.

- Let the agent-name column flex, or show a tooltip for names over 18 characters. The mockup fixes the column at 150 px and truncates.
- Add a non-colour cue for the mode on the collapsed chip. Today the mode shows by avatar-ring colour only.

## Implementation must add and test

- Hover opens the popover (`openOnHover` with the safe polygon) and a click or tap opens it too. Hover alone never changes a mode.
- Each agent row is a four-way control with keyboard movement, and the focused or hovered mode fills the description line.
- A mode that does not apply (agent not running) is dimmed and not selectable, as today.
- The blocked chip keeps today's look and is not part of the popover.
- The phone targets (40 px), the 150 ms grace period and the short phone labels are proposals in the mockup, not source.
