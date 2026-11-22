# Workflow status

## What it does

An issue has a status key. A project workflow defines the statuses (steps), what each step hands off to (an assignee or agent and its instructions), and entry gates a status write must meet. Moving an issue into a step reassigns it to the step's handler and starts their run. Status history records each change.

Sub-features:
- Status write with gates: `multica issue status <id> <key>`; a refused write names each failed gate (`server/internal/issueworkflow/`).
- Project workflow get and switch: `multica project workflow get|set`; workflow CRUD and handoff: `multica workflow ...` (`server/cmd/multica/cmd_workflow.go`).
- Per-issue workflow override and guarded reassignment, per-step guarded edges and a read-only flowchart (W02, W15).
- Status history dialog (`packages/views/issues/components/status-history-dialog.tsx`).
- Not driven yet: every row.

## How to reach it

- **Web**: issue list and board status pickers, the issue page status control, status history button, workflows pages (`packages/views/workflows/`), workspace settings, Issue Statuses.
- **Desktop**: the same shared views.
- **CLI**: `multica issue status`, `multica workflow list|get|lint|update|handoff|set-instructions`, `multica project workflow get|set`. A step or handoff edit needs a passkey-approved action.
- **Mobile**: status and workflow pickers under `apps/mobile/components/issue/pickers/`, handoff data in `apps/mobile/data/workflow-handoff.ts`. Device evidence required.

## Settings

Workflow steps (status keys, entry gates, handoff), run budget fields on the workflow (`max_runs_per_issue`), custom statuses per workspace (Workspace Settings, Issue Statuses). `--no-start` records a change without starting the handler's run.

## States

Built-in keys: `backlog`, `todo`, `in_progress`, `in_review`, `blocked`, `done`, `cancelled`; a workspace adds custom keys. A status write is accepted, or refused with the failed gate. Cancelling with `--duplicate-of` marks a duplicate; any later non-cancel status clears the mark.

## Edge cases

- The argument is a status key, not its display name; an unknown value errors with the full list.
- A step with a handler starts a run on entry; a run refused by the weekly run guard or run budget leaves a card (see decision-cards).
- The issue must keep a status that fits its workflow when its project or workflow changes.
- Moving a status as a person's own login may pass a gate an agent token cannot; drive gate rows with the right actor.

## Ledger row

`docs/fork/FORK-FEATURES.md`: W01 (CLI read/edit of project workflows), W02 (per-issue workflow override), W15 (guarded edges, flowchart view), W19 (step entry gates), C65 (status history).
