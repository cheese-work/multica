# Decision cards

## What it does

An agent proposes an action (`issue.run_guard.allow`, `agent.pause`, `issue.run_budget.raise`, `agent.env.patch` and others). The server stores it, renders a preview of the values the executor will check, and shows a card. A person approves (passkey-gated) or rejects. A card whose type the web client does not preview shows "Details unavailable" and a disabled Approve (fail closed).

Sub-features:
- Weekly run-guard card: a refused run (7-day cap, default 30, `MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS`) opens one card; approving adds 10 runs. Owner: `server/internal/handler/weekly_run_guard.go`, `decision_request_card_preview.go`. Driven by `scripts/decision-card.spec.ts`.
- Preview allowlist: `PREVIEWED_ACTION_TYPES` in `packages/core/issues/decision-action-preview.ts`; the Go side is `actionPreviewFields`. Both lists must match (CHE-6242). Covered by `e2e/decision-dm-card-renders.spec.ts`, which lists every type; this skill drives only the run-guard type.
- Not driven yet: reject path, approve with the passkey, answer-on-card, fold and "needs you" badges.

## How to reach it

- **Web**: `/<workspace-slug>/chat?session=<id>` (the DM with the asking agent) or the issue page's pending-decision banner. Test ids: `decision-request-card`, `decision-request-action-preview`, radio "Approve" and "Reject".
- **Desktop**: same shared view (`packages/views/chat/components/decision-request-card.tsx`) in the Electron chat tab. Not driven by this skill yet.
- **CLI**: `multica decision propose|ask|request|get|list|withdraw|revoke`. Approval has no CLI path; it needs a person's passkey in a browser.
- **Mobile**: no card component found under `apps/mobile`; only chat attention keys mention `decision:` rows. Treat as not verified until the Android device procedure records evidence.

## Settings

- `MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS` (server env; 0 or less turns the guard off).
- Workspace `settings.run_guard.mode`: `off | warn | block` (default `block`; `warn` opens no card).
- Agent decisions on or off: `multica decision agent-setting` (owner only, default off).

## States

Request: `open`, `answered`, `withdrawn`. Action: pending, executed, failed, rejected. Only a pending action carries a preview. The card folds once answered.

## Edge cases

- The preview field needs the server limit above 0; with the guard off the card has no fields.
- A passkey approval needs an exact https origin from `MULTICA_WEBAUTHN_ORIGINS`; the drive serves the page over a routed https origin (`serveOverHttps`) and uses a virtual authenticator.
- The first visit to the chat route compiles under `next dev` (about 2 minutes on a loaded host); warm it before driving.
- The auto-approver (`weekly_run_guard_auto.go`) can settle a refusal before a card opens; the drive seeds the refusal row directly so a card always opens.

## Ledger row

`docs/fork/FORK-FEATURES.md`: C06 (task-bound decision intents), C25 (quote cards, answer-on-card), C29 (approve-to-execute actions), C50 (action proposals, proposal cards with passkey approval), C51 (config-change preview). The run-guard card regression is CHE-6242.
