# Feature map

One file per user-facing feature of the fork. The final verifier of a change drives every row the change touches and attaches evidence per row id. Add a feature with the verify-app Maintain procedure; do not edit `docs/fork/FORK-FEATURES.md` for it (that ledger is the code and upstream record that `scripts/fork-sync-guard.mjs` reads).

- [decision-cards](decision-cards.md): the card in a DM or on an issue where a person approves or rejects an agent's proposed action.
- [notification-settings](notification-settings.md): per-group mute and per-channel switches for inbox and DM notifications.
- [workflow-status](workflow-status.md): issue statuses, project workflows, step handoffs and entry gates.

Surface evidence rules: web, desktop and CLI rows run on an X99 build (see `../SKILL.md`). Mobile rows run through the Android device squad's procedure, not this skill; a row with no device evidence is "not verified".
