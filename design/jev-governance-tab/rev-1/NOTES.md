## What changed vs the current live Settings

- New tab "Jev / Governance" in the Workspace group; nothing existing is edited.
- Mode `off | shadow | on` with `on` locked and the reason "needs shadow data: <= 2% confident-wrong over 200+ verified items".
- `jev_min_confidence` field; credential shown as set / not set only, never the value.
- History with rollback (`rollback_to_version`, `expected_version`, confirm dialog).
- Mock data only; static HTML, no network fetches; shared tokens in `shared.css`.

## Directions

- A — Stacked sections: one card per concern, segmented control for mode, history table with a Roll back button per row.
- B — Status first: summary strip, three option cards for mode, history as a timeline with change chips.
- C — Sub-tabs, dense history: Routing / Credential / History sub-tabs, compact list with a detail pane that owns Roll back.
