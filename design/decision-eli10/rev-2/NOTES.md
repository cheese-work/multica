## What changed vs rev-1

- Adds `A.html`, a static replay fixture of the built card (tokens copied from `packages/ui/styles/tokens.css`, `details`/`summary` rules copied from `rich-content.css`, no network). Open it in any browser to replay both Details states.
- Adds `A-1440.png` and `A-390.png`, captured from `A.html` with headless Chrome on X99: Details collapsed on top, Details open below. Rev-1 PNGs came from a Vite page that is no longer in the repo.
- Answers review finding R1 from x99-codex-sol on PR #456: the record lacked replayable source, an admit sheet and a review pack. The sheet is `docs/ux/CHE-2186.yml`.
- Rev-1 is unchanged. Its before/after captures of the real card stay the evidence for the CHE-1755 comparison.

## Directions

- A — the card as built: plain context first, question and stakes before options, working notes folded into Details, investigate link always visible.

## Limits

- The fixture is a hand-copied mirror of the built card, not the app. Drift is possible; the Vitest card tests and rev-1 real-card captures cover the real component.
- The ask text is the CHE-1755 draft shortened for the fixture. Time values are estimates.
- One direction only. No Cheese feedback yet; no design approval or waiver is recorded.
