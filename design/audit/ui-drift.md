# UI drift audit: merged S0b result, S1 pattern list, root-cause verdict

Issue: CHE-5451. Parent: CHE-5424 (S0b). Plan: CHE-5420. Date: 13 Nov 2026 (ICT).
Inputs: [`s0b-1.md`](ui-drift/s0b-1.md), [`s0b-2.md`](ui-drift/s0b-2.md), [`s0b-3.md`](ui-drift/s0b-3.md), [`s0b-4.md`](ui-drift/s0b-4.md). The four batch files are unchanged. Spot-checks ran on main at `b29d81efcc`.
Scope: read-only merge of the four batch audits (118 fork-only files in `packages/views/`). No code changed. Evidence is code only (components, tokens, locale keys). No rendered frame was compared.

## Summary

| Question | Answer |
|---|---|
| Files covered | 118 of 118 (83 with drift, 35 pattern-only) |
| Drift items | 126 after one correction (127 as recorded in the batches) |
| Sibling existed | 116 of 126 (92.1%) |
| Reference would have pointed to it | 106 of 126 (84.1%) |
| Root-cause verdict, cause 1 (no pattern reference) | **SUPPORTED** |
| S1 seed list | 28 file patterns plus 4 drift-family entries (list below) |
| S3 seeded copy duplicates | 10 (list below) |
| S4b raw-element files | 35 (list below) |

## Totals

Counts come from the batch tables (`Drift items` and `File map`), parsed by script. Per-item fields are Kind, Sibling existed and Reference would have pointed.

| Batch | Files | Pattern-only | With drift | Items (as recorded) | Sibling yes / no | Reference yes / no |
|---|--:|--:|--:|--:|--:|--:|
| [S0b-1](ui-drift/s0b-1.md) | 15 | 6 | 9 | 13 | 12 / 1 | 11 / 2 |
| [S0b-2](ui-drift/s0b-2.md) | 40 | 9 | 31 | 54 | 49 / 5 | 48 / 6 |
| [S0b-3](ui-drift/s0b-3.md) | 32 | 11 | 21 | 32 | 31 / 1 | 24 / 8 |
| [S0b-4](ui-drift/s0b-4.md) | 31 | 9 | 22 | 28 | 25 / 3 | 23 / 5 |
| **Total as recorded** | **118** | **35** | **83** | **127** | **117 / 10** | **106 / 21** |
| **Total corrected** | 118 | 35 | 83 | **126** | **116 / 10** | **106 / 20** |

Kind, corrected: component 99, layout 19, copy 8 (recorded: 99 / 19 / 9).

Notes:
- `s0b-3.md` says drift items are "in 22 files". Its rows give 21. This file uses the row count.
- The correction drops S0b-2 D3 (see Corrections). Batch 2 has 53 items after it (sibling yes 48 / no 5; reference yes 48 / no 5).
- `s0b-1.md` file paths are relative to `packages/views/issues/`. The other batches are relative to `packages/views/`. The file map below uses full paths relative to `packages/views/`.

## File map (all 118 files)

`Drift` = number of drift items recorded for the file (ids are local to the batch file). `S4b` = raw-element file marked `screenshot: S4b` in its batch file; `raw*` = raw-element file that its batch did not mark S4b (S0b-1: screenshot NOT-CAPTURED). Paths are under `packages/views/`.

| # | Batch | File | Pattern id(s) | Drift | Screenshot |
|--:|:--:|------|---------------|------:|:---:|
| 1 | [1](ui-drift/s0b-1.md) | `issues/compact-issue-chips-context.tsx` | `issue-mention-chip` | 0 |  |
| 2 | [1](ui-drift/s0b-1.md) | `issues/components/attachment-list.tsx` | `attachment-card-list` | 1 |  |
| 3 | [1](ui-drift/s0b-1.md) | `issues/components/attachment-revision-diff.tsx` | `diff-dialog` | 1 |  |
| 4 | [1](ui-drift/s0b-1.md) | `issues/components/batch-workflow-picker.tsx` | `property-picker` | 0 |  |
| 5 | [1](ui-drift/s0b-1.md) | `issues/components/comment-mention-card.tsx` | `issue-mention-chip` | 0 |  |
| 6 | [1](ui-drift/s0b-1.md) | `issues/components/decision-action-panel.tsx` | `decision-action-approval` | 1 |  |
| 7 | [1](ui-drift/s0b-1.md) | `issues/components/decision-quote-card.tsx` | `decision-receipt-card` | 4 |  |
| 8 | [1](ui-drift/s0b-1.md) | `issues/components/description-disclosure.tsx` | `description-disclosure` | 1 |  |
| 9 | [1](ui-drift/s0b-1.md) | `issues/components/issue-decision-banner.tsx` | `inline-retry`, `ict-timestamp` | 2 | raw* |
| 10 | [1](ui-drift/s0b-1.md) | `issues/components/issue-detail-lazy.tsx` | `lazy-detail` | 0 |  |
| 11 | [1](ui-drift/s0b-1.md) | `issues/components/issue-inbox-restore-button.tsx` | `icon-button-action` | 0 |  |
| 12 | [1](ui-drift/s0b-1.md) | `issues/components/run-budget-chip.tsx` | `status-pill` | 1 |  |
| 13 | [1](ui-drift/s0b-1.md) | `issues/components/run-budget-meter.tsx` | `run-budget` | 1 |  |
| 14 | [1](ui-drift/s0b-1.md) | `issues/components/status-history-button.tsx` | `sidebar-action-button` | 0 |  |
| 15 | [1](ui-drift/s0b-1.md) | `issues/components/status-history-dialog.tsx` | `status-history` | 1 |  |
| 16 | [2](ui-drift/s0b-2.md) | `issues/components/status-summary.tsx` | `inline-notice` | 1 |  |
| 17 | [2](ui-drift/s0b-2.md) | `issues/components/supersession-links.tsx` | `inline-link-action` | 0 |  |
| 18 | [2](ui-drift/s0b-2.md) | `issues/components/workflow-pill.tsx` | `status-pill` | 1 |  |
| 19 | [2](ui-drift/s0b-2.md) | `issues/components/hierarchy/hierarchy-dialog.tsx` | `canvas-dialog` | 2 | S4b |
| 20 | [2](ui-drift/s0b-2.md) | `issues/components/hierarchy/hierarchy-minimap.tsx` | `sidebar-card`, `loading-skeleton` | 1 | S4b |
| 21 | [2](ui-drift/s0b-2.md) | `issues/components/hierarchy/hierarchy-parts.tsx` | `status-dot` | 1 |  |
| 22 | [2](ui-drift/s0b-2.md) | `chat/components/scheduled-wakeups-chip.tsx` | `header-chip-popover` | 1 |  |
| 23 | [2](ui-drift/s0b-2.md) | `chat/components/wakeup-event-row.tsx` | `event-rail-row`, `issue-mention-chip` | 3 | S4b |
| 24 | [2](ui-drift/s0b-2.md) | `issues/components/issue-wakeup-defaults.tsx` | `wakeup-rule-form` | 2 |  |
| 25 | [2](ui-drift/s0b-2.md) | `issues/components/wakeup-condition-params.tsx` | `pill-menu` | 1 | S4b |
| 26 | [2](ui-drift/s0b-2.md) | `issues/components/wakeup-fields.tsx` | `pill-menu`, `inline-notice` | 2 | S4b |
| 27 | [2](ui-drift/s0b-2.md) | `issues/components/wakeup-pickers.tsx` | `property-picker`, `pill-menu`, `segmented-control` | 2 | S4b |
| 28 | [2](ui-drift/s0b-2.md) | `issues/components/wakeups-paused-banner.tsx` | `inline-notice` | 1 |  |
| 29 | [2](ui-drift/s0b-2.md) | `projects/components/project-wakeups-section.tsx` | `collapsible-section` | 0 |  |
| 30 | [2](ui-drift/s0b-2.md) | `settings/components/wakeups-tab.tsx` | `settings-section-form`, `inline-notice` | 1 |  |
| 31 | [2](ui-drift/s0b-2.md) | `settings/components/workspace-wakeup-rules.tsx` | `collapsible-section`, `wakeup-rule-form`, `inline-notice` | 2 | S4b |
| 32 | [2](ui-drift/s0b-2.md) | `chat/components/annotatable-answer.tsx` | `annotation-host` | 0 |  |
| 33 | [2](ui-drift/s0b-2.md) | `chat/components/chat-annotations.tsx` | `header-chip-popover` | 1 |  |
| 34 | [2](ui-drift/s0b-2.md) | `chat/components/chat-context-meter.tsx` | `run-budget` | 2 |  |
| 35 | [2](ui-drift/s0b-2.md) | `chat/components/chat-preset-picker.tsx` | `icon-button-action`, `dropdown-radio-menu` | 0 |  |
| 36 | [2](ui-drift/s0b-2.md) | `chat/components/chat-search.tsx` | `search-list`, `inline-retry` | 3 | S4b |
| 37 | [2](ui-drift/s0b-2.md) | `chat/components/chat-session-mention-card.tsx` | `issue-mention-chip` | 0 |  |
| 38 | [2](ui-drift/s0b-2.md) | `chat/components/chat-session-stats-dialog.tsx` | `stats-dialog`, `inline-retry` | 2 |  |
| 39 | [2](ui-drift/s0b-2.md) | `chat/components/chat-supplement-status.tsx` | `delivery-status-line` | 0 |  |
| 40 | [2](ui-drift/s0b-2.md) | `chat/components/config-change-viewer.tsx` | `diff-dialog`, `tabs` | 1 | S4b |
| 41 | [2](ui-drift/s0b-2.md) | `chat/components/continuation-block.tsx` | `collapsible-section` | 1 |  |
| 42 | [2](ui-drift/s0b-2.md) | `chat/components/decision-answer-content.tsx` | `decision-receipt-card` | 1 |  |
| 43 | [2](ui-drift/s0b-2.md) | `chat/components/decision-artifacts.tsx` | `attachment-card-list`, `diff-dialog`, `inline-retry` | 2 | S4b |
| 44 | [2](ui-drift/s0b-2.md) | `chat/components/decision-dm-card.tsx` | `decision-receipt-card` | 4 |  |
| 45 | [2](ui-drift/s0b-2.md) | `chat/components/decision-recent-runs.tsx` | `run-budget`, `ict-timestamp` | 1 |  |
| 46 | [2](ui-drift/s0b-2.md) | `chat/components/decision-request-card.tsx` | `decision-request-card`, `decision-action-approval`, `ict-timestamp` | 5 | S4b |
| 47 | [2](ui-drift/s0b-2.md) | `chat/components/decision-withdrawal-card.tsx` | `decision-receipt-card` | 2 |  |
| 48 | [2](ui-drift/s0b-2.md) | `chat/components/deferred-content-editor.tsx` | `lazy-detail` | 0 |  |
| 49 | [2](ui-drift/s0b-2.md) | `chat/components/diff-hunks.tsx` | `diff-dialog` | 0 |  |
| 50 | [2](ui-drift/s0b-2.md) | `chat/components/interactive-login-banner.tsx` | `inline-notice` | 1 |  |
| 51 | [2](ui-drift/s0b-2.md) | `chat/components/lazy-content-editor.tsx` | `lazy-detail` | 0 |  |
| 52 | [2](ui-drift/s0b-2.md) | `chat/components/needs-you-list.tsx` | `needs-you-list`, `inline-retry` | 4 | S4b |
| 53 | [2](ui-drift/s0b-2.md) | `chat/components/open-decisions-badge.tsx` | `header-chip-popover` | 1 |  |
| 54 | [2](ui-drift/s0b-2.md) | `chat/components/predecessor-dms.tsx` | `collapsible-section` | 1 |  |
| 55 | [2](ui-drift/s0b-2.md) | `chat/components/quick-agent-bar.tsx` | `avatar-strip` | 1 | S4b |
| 56 | [3](ui-drift/s0b-3.md) | `chat/components/use-archived-send.tsx` | `confirm-dialog` | 0 |  |
| 57 | [3](ui-drift/s0b-3.md) | `chat/components/use-chat-annotations.tsx` | `annotation-hook` | 0 |  |
| 58 | [3](ui-drift/s0b-3.md) | `chat/components/use-dm-session-surface.tsx` | `ict-timestamp` | 1 |  |
| 59 | [3](ui-drift/s0b-3.md) | `settings/components/agent-decisions-row.tsx` | `settings-toggle-row` | 0 |  |
| 60 | [3](ui-drift/s0b-3.md) | `settings/components/auto-compact-tokens-row.tsx` | `settings-number-row` | 0 |  |
| 61 | [3](ui-drift/s0b-3.md) | `settings/components/composio-tab.tsx` | `integration-card-grid` | 2 |  |
| 62 | [3](ui-drift/s0b-3.md) | `settings/components/delegated-writes-section.tsx` | `settings-table-section` | 3 | S4b |
| 63 | [3](ui-drift/s0b-3.md) | `settings/components/dingtalk-tab.tsx` | `integration-tab` | 7 | S4b |
| 64 | [3](ui-drift/s0b-3.md) | `settings/components/export-privacy-tab.tsx` | `settings-tab-states` | 0 |  |
| 65 | [3](ui-drift/s0b-3.md) | `settings/components/github-review-mappings.tsx` | `settings-key-value-map-row` | 1 |  |
| 66 | [3](ui-drift/s0b-3.md) | `settings/components/idle-guard-rows.tsx` | `settings-select-row` | 0 |  |
| 67 | [3](ui-drift/s0b-3.md) | `settings/components/lark-tab.tsx` | `integration-tab` | 6 | S4b |
| 68 | [3](ui-drift/s0b-3.md) | `settings/components/passkeys-tab.tsx` | `settings-credential-list` | 1 |  |
| 69 | [3](ui-drift/s0b-3.md) | `settings/components/provider-breakers-tab.tsx` | `settings-table-section` | 2 |  |
| 70 | [3](ui-drift/s0b-3.md) | `settings/components/routing-agent-rows.tsx` | `settings-select-row` | 0 |  |
| 71 | [3](ui-drift/s0b-3.md) | `settings/components/runtime-profile-row.tsx` | `settings-select-row` | 0 |  |
| 72 | [3](ui-drift/s0b-3.md) | `settings/components/slack-tab.tsx` | `integration-tab` | 8 | S4b |
| 73 | [3](ui-drift/s0b-3.md) | `settings/components/telegram-tab.tsx` | `integration-tab` | 7 | S4b |
| 74 | [3](ui-drift/s0b-3.md) | `settings/components/wecom-scan-install-dialog.tsx` | `qr-install-dialog` | 1 |  |
| 75 | [3](ui-drift/s0b-3.md) | `settings/components/wecom-tab.tsx` | `integration-tab` | 6 | S4b |
| 76 | [3](ui-drift/s0b-3.md) | `settings/components/workspace-scheduling-row.tsx` | `settings-action-row` | 0 |  |
| 77 | [3](ui-drift/s0b-3.md) | `agents/components/agent-auto-compact-section.tsx` | `settings-number-row` | 1 |  |
| 78 | [3](ui-drift/s0b-3.md) | `agents/components/agent-scheduling.tsx` | `settings-action-row` | 2 |  |
| 79 | [3](ui-drift/s0b-3.md) | `agents/components/create-agent-dialog.tsx` | `create-dialog` | 2 | S4b |
| 80 | [3](ui-drift/s0b-3.md) | `agents/components/inspector/agent-presets-section.tsx` | `settings-section-card` | 0 |  |
| 81 | [3](ui-drift/s0b-3.md) | `agents/components/inspector/concurrency-picker.tsx` | `property-picker` | 0 | S4b |
| 82 | [3](ui-drift/s0b-3.md) | `agents/components/inspector/runtime-profile-field.tsx` | `settings-select-row` | 1 |  |
| 83 | [3](ui-drift/s0b-3.md) | `agents/components/inspector/skill-attach.tsx` | `chip-add-button` | 1 | S4b |
| 84 | [3](ui-drift/s0b-3.md) | `agents/components/inspector/visibility-picker.tsx` | `property-picker` | 1 | S4b |
| 85 | [3](ui-drift/s0b-3.md) | `agents/components/instructions-editor.tsx` | `description-disclosure` | 1 | S4b |
| 86 | [3](ui-drift/s0b-3.md) | `agents/components/runtime-machine-filter-dropdown.tsx` | `filter-dropdown` | 1 |  |
| 87 | [3](ui-drift/s0b-3.md) | `agents/components/tabs/agent-mcp-tab.tsx` | `integration-card-grid` | 2 |  |
| 88 | [4](ui-drift/s0b-4.md) | `agents/components/tabs/integrations-tab.tsx` | `integration-tab` | 1 |  |
| 89 | [4](ui-drift/s0b-4.md) | `agents/create/ai-create-agent-page.tsx` | `create-shell` | 0 |  |
| 90 | [4](ui-drift/s0b-4.md) | `agents/create/builder-setup-panel.tsx` | `create-shell` | 0 |  |
| 91 | [4](ui-drift/s0b-4.md) | `autopilots/components/add-trigger-dialog.tsx` | `form-dialog` | 2 | S4b |
| 92 | [4](ui-drift/s0b-4.md) | `autopilots/components/autopilot-dialog-lazy.tsx` | `lazy-detail` | 0 |  |
| 93 | [4](ui-drift/s0b-4.md) | `common/decision-action-approval.tsx` | `decision-action-approval` | 0 |  |
| 94 | [4](ui-drift/s0b-4.md) | `common/expandable-description.tsx` | `description-disclosure` | 1 | S4b |
| 95 | [4](ui-drift/s0b-4.md) | `common/task-transcript/run-preset-details.tsx` | `detail-row` | 1 |  |
| 96 | [4](ui-drift/s0b-4.md) | `editor/extensions/preset-arg-suggestion.tsx` | `slash-suggestion` | 0 |  |
| 97 | [4](ui-drift/s0b-4.md) | `editor/html-viewer.tsx` | `icon-button-action` | 1 | S4b |
| 98 | [4](ui-drift/s0b-4.md) | `editor/plan-revision-viewer.tsx` | `diff-dialog`, `ict-timestamp`, `inline-retry` | 4 | S4b |
| 99 | [4](ui-drift/s0b-4.md) | `invite/invite-page.tsx` | `state-card` | 1 |  |
| 100 | [4](ui-drift/s0b-4.md) | `labels/resource-label-picker.tsx` | `property-picker` | 1 | S4b |
| 101 | [4](ui-drift/s0b-4.md) | `layout/machine-pause-banner.tsx` | `pause-banner` | 1 |  |
| 102 | [4](ui-drift/s0b-4.md) | `layout/open-decisions-chip.tsx` | `status-pill` | 1 |  |
| 103 | [4](ui-drift/s0b-4.md) | `layout/workspace-pause-banner.tsx` | `pause-banner` | 1 |  |
| 104 | [4](ui-drift/s0b-4.md) | `media/media-context.tsx` | `icon-button-action`, `menu-popover` | 2 | S4b |
| 105 | [4](ui-drift/s0b-4.md) | `media/media-page.tsx` | `inline-retry`, `empty-state` | 2 | S4b |
| 106 | [4](ui-drift/s0b-4.md) | `projects/components/github-ref-dialog.tsx` | `form-dialog` | 2 |  |
| 107 | [4](ui-drift/s0b-4.md) | `projects/components/local-directory-mode-dialog.tsx` | `form-dialog`, `radio-card` | 3 | S4b |
| 108 | [4](ui-drift/s0b-4.md) | `rich-content/next-self-mention.tsx` | `floating-jump-pill` | 1 | S4b |
| 109 | [4](ui-drift/s0b-4.md) | `runtimes/components/machine-pause-card.tsx` | `pause-banner`, `inline-retry` | 2 |  |
| 110 | [4](ui-drift/s0b-4.md) | `search/delegate/delegate-group.tsx` | `palette-group` | 0 |  |
| 111 | [4](ui-drift/s0b-4.md) | `search/lazy-search-command.tsx` | `lazy-detail` | 0 |  |
| 112 | [4](ui-drift/s0b-4.md) | `workflows/batch-handoff-confirm-dialog.tsx` | `confirm-dialog`, `inline-retry` | 1 |  |
| 113 | [4](ui-drift/s0b-4.md) | `workflows/project-workflow-menu.tsx` | `flow-accordion-row` | 1 | S4b |
| 114 | [4](ui-drift/s0b-4.md) | `workflows/workflow-budget-request-banner.tsx` | `inline-notice` | 1 |  |
| 115 | [4](ui-drift/s0b-4.md) | `workflows/workflow-choice-pill.tsx` | `pill-menu` | 0 |  |
| 116 | [4](ui-drift/s0b-4.md) | `workflows/workflow-edge-editor.tsx` | `form-field` | 2 |  |
| 117 | [4](ui-drift/s0b-4.md) | `workflows/workflow-graph.tsx` | `flow-graph` | 0 |  |
| 118 | [4](ui-drift/s0b-4.md) | `workflows/workflow-override-options.tsx` | `choice-list` | 1 |  |

## Proposed S1 pattern list (CHE-5425)

The batches proposed 70 distinct ids. After merging 5 aliases (`integration-provider-card` into `integration-tab`, `lazy-editor` into `lazy-detail`, `pill-trigger` into `pill-menu`, `handoff-confirm-dialog` into `confirm-dialog`, `context-meter` into `run-budget`) and dropping `dialog-description` (see Corrections), 69 remain. Seed rule: an id seeds `design/patterns.yml` when 2 or more files map to it. 28 ids pass. 4 more come from recurring drift families that no single id covers. The 41 other ids have one file and stay out of S1 until a second file appears.

"exists" means the batch found the component in `packages/ui` or `packages/views`. "none exists" means S1 must name the shared component to build. "none recorded" means the batches recorded no locale key for the pattern.

### File patterns (28)

| id | Files | Intent | Components | Copy keys | Surfaces (file names) |
|---|--:|---|---|---|---|
| `inline-retry` | 9 | One error line plus one retry action. | `Button` (exists); one label key | `chat.json` `needs_you.error` + `needs_you.retry`; collapse the other Retry and Try again keys (S3 item 1) | `issue-decision-banner`, `chat-search`, `chat-session-stats-dialog`, `decision-artifacts`, `needs-you-list`, `plan-revision-viewer`, `media-page`, `machine-pause-card`, `batch-handoff-confirm-dialog` |
| `inline-notice` | 7 | Warning, muted, success or destructive notice box. | `Alert` (exists; `default` and `destructive` only). S1 must decide warning, muted and success tones. | none recorded | `status-summary`, `wakeup-fields`, `wakeups-paused-banner`, `wakeups-tab`, `workspace-wakeup-rules`, `interactive-login-banner`, `workflow-budget-request-banner` |
| `integration-tab` | 6 | Settings tab for one chat integration: provider card, installation row, bot row, 3 card states, disconnect dialog. | shared tab (none exists: copied 5 times); `ui/card.tsx`, `AlertDialog` (exist) | `revoked_badge`, `installed_at_label`, `not_enabled_title`, `empty_title`, `loading`, `toast_disconnected`, `agent_bot_connected_label` (S3 items 3 to 8) | `dingtalk-tab`, `lark-tab`, `slack-tab`, `telegram-tab`, `wecom-tab`, `integrations-tab` |
| `diff-dialog` | 5 | Dialog or panel with diff hunks. | `HunksRegion` in `config-change-viewer`/`diff-hunks` (exists); `ui/tabs.tsx`, `ui/collapsible.tsx` (exist) | none recorded | `attachment-revision-diff`, `config-change-viewer`, `decision-artifacts`, `diff-hunks`, `plan-revision-viewer` |
| `property-picker` | 5 | Popover picker for one property. | `PropertyPicker`, `common/pill-button.tsx` (exist) | none recorded | `batch-workflow-picker`, `wakeup-pickers`, `concurrency-picker`, `visibility-picker`, `resource-label-picker` |
| `ict-timestamp` | 5 | One formatter for ICT times with the ICT suffix. | shared formatter (none exists: 6 local copies; 5 use `Asia/Bangkok`, 1 uses `Asia/Ho_Chi_Minh`) | literal " ICT" in `decision-recent-runs.tsx:46` moves to a locale key | `issue-decision-banner`, `decision-recent-runs`, `decision-request-card`, `use-dm-session-surface`, `plan-revision-viewer` |
| `lazy-detail` | 5 | `Suspense` wrapper with a null fallback for a lazy surface. | one wrapper (3 near-copies today) | none | `issue-detail-lazy`, `deferred-content-editor`, `lazy-content-editor`, `autopilot-dialog-lazy`, `lazy-search-command` |
| `issue-mention-chip` | 4 | Chip that links an issue. | `ISSUE_CHIP_CLASS` in `issues/components/issue-chip` (exists) | none recorded | `compact-issue-chips-context`, `comment-mention-card`, `wakeup-event-row`, `chat-session-mention-card` |
| `decision-receipt-card` | 4 | Card that records a decision ask, answer or withdrawal. | shared card frame (none exists: 3 copies of one recipe) | none recorded | `decision-quote-card`, `decision-answer-content`, `decision-dm-card`, `decision-withdrawal-card` |
| `icon-button-action` | 4 | Icon-only action button. | `Button variant="ghost" size="icon-sm"`, `IconButton` (exist) | none recorded | `issue-inbox-restore-button`, `chat-preset-picker`, `html-viewer`, `media-context` |
| `collapsible-section` | 4 | Toggle that folds a section. | `ui/collapsible.tsx` (exists; used in `continuation-block`, `predecessor-dms`) | none recorded | `project-wakeups-section`, `workspace-wakeup-rules`, `continuation-block`, `predecessor-dms` |
| `settings-select-row` | 4 | Settings row with a select. | `SettingsRow` + `Select` (exist); `runtime-profile-field` omits `SettingsRow` | none recorded | `idle-guard-rows`, `routing-agent-rows`, `runtime-profile-row`, `runtime-profile-field` |
| `pill-menu` | 4 | Pill-shaped menu trigger. | `Select`, `PropertyPicker` (exist); `pillClass` in `wakeup-pickers` is the local source | none recorded | `wakeup-condition-params`, `wakeup-fields`, `wakeup-pickers`, `workflow-choice-pill` |
| `decision-action-approval` | 3 | Approve or reject a decision action. | `common/decision-action-approval.tsx` (exists) | none recorded | `decision-action-panel`, `decision-request-card`, `decision-action-approval` |
| `description-disclosure` | 3 | Show more / Show less. | `issues/components/description-disclosure.tsx` (exists, `Button size="xs"`); 4 implementations today | `wakeup.show_instruction`/`hide_instruction`, `needs_you.show_all`/`show_less` | `description-disclosure`, `instructions-editor`, `expandable-description` |
| `status-pill` | 3 | Small status chip. | `ui/badge.tsx` (exists) | none recorded | `run-budget-chip`, `workflow-pill`, `open-decisions-chip` |
| `header-chip-popover` | 3 | Header chip that opens a popover. | `Popover` + `Button variant="outline" size="sm"` (exist) | none recorded | `scheduled-wakeups-chip`, `chat-annotations`, `open-decisions-badge` |
| `form-dialog` | 3 | Dialog with a form, a destructive notice and a mono path box. | `ui/input.tsx`, `ui/field.tsx`, `Alert variant="destructive"` (exist) | none recorded | `add-trigger-dialog`, `github-ref-dialog`, `local-directory-mode-dialog` |
| `pause-banner` | 3 | Warning banner above a page for a paused workspace or machine. | shared banner (none exists: 2 opacity sets) | `machine_pause.*` | `machine-pause-banner`, `workspace-pause-banner`, `machine-pause-card` |
| `run-budget` | 3 | Meter for a budget or a context window. | `ui/progress.tsx`, `issues/components/progress-ring.tsx` (exist); use `role="meter"` | none recorded | `run-budget-meter`, `chat-context-meter`, `decision-recent-runs` |
| `attachment-card-list` | 2 | Attachment cards with a revision badge. | `deliverables/version-badge.tsx` `VersionBadge` (exists) | `deliverables.version_short`; retire `attachment_revision.badge` (S3 item 9) | `attachment-list`, `decision-artifacts` |
| `wakeup-rule-form` | 2 | Instruction, enable switch, 4000-byte limit and 409 message for a wakeup rule. | one form shared by issue and settings (2 copies today) | `instruction_invalid`, `save_error`, `conflict_error` (2 namespaces) | `issue-wakeup-defaults`, `workspace-wakeup-rules` |
| `settings-number-row` | 2 | Number input with inline error. | `auto-compact-tokens-row` (exists, 3 copies today) | none recorded | `auto-compact-tokens-row`, `agent-auto-compact-section` |
| `integration-card-grid` | 2 | Card for a connectable app with a status. | `ui/card.tsx`, `ui/badge.tsx` (exist) | none recorded | `composio-tab`, `agent-mcp-tab` |
| `settings-table-section` | 2 | Settings table. | `ui/table.tsx` (exists; 2 raw tables here, plus `chat-session-stats-dialog`) | none recorded | `delegated-writes-section`, `provider-breakers-tab` |
| `settings-action-row` | 2 | Pause / resume row with a confirm dialog. | one row (2 copies today) | none recorded | `workspace-scheduling-row`, `agent-scheduling` |
| `create-shell` | 2 | Agent create page shell. | `AgentCreateShell`, `AgentCreateChip` (exist per the batch) | none recorded | `ai-create-agent-page`, `builder-setup-panel` |
| `confirm-dialog` | 2 | Confirm dialog for a handoff. | `AlertDialog` (exists) | none recorded | `use-archived-send`, `batch-handoff-confirm-dialog` |

### Drift-family entries (4)

| id | Intent | Evidence | Components |
|---|---|---|---|
| `section-eyebrow` | Uppercase label above a section. | Recipe `text-micro uppercase tracking-wide` in 27 files of `packages/views` ([S0b-3](ui-drift/s0b-3.md) cross-file table). | none exists |
| `type-role-tokens` | Use `text-caption`, `text-body` and the other role tokens, not `text-xs`, `text-sm` or `text-body-sm`. | [S0b-1](ui-drift/s0b-1.md) D13, [S0b-2](ui-drift/s0b-2.md) D1 and D47, [S0b-3](ui-drift/s0b-3.md) D23. `text-body-sm` is not a token. | tokens in `ui/styles/tokens.css` (exist) |
| `inline-link-action` | Underlined text link. | Recipe `underline underline-offset-2` in 8 files ([S0b-2](ui-drift/s0b-2.md) D12, D31). | none exists |
| `choice-card` | Single-choice card or radio list. | 7 files use `role="radio"` in `packages/views`. [S0b-2](ui-drift/s0b-2.md) D16, D40; [S0b-3](ui-drift/s0b-3.md) D16, D30; [S0b-4](ui-drift/s0b-4.md) D16. `packages/ui` has no `RadioGroup`; `dashboard/components/dashboard-shared.tsx:64` exports `Segmented`. | none exists |

### Deferred ids (41, one file each)

`annotation-hook`, `annotation-host`, `avatar-strip`, `canvas-dialog`, `chip-add-button`, `choice-list`, `create-dialog`, `decision-request-card`, `delivery-status-line`, `detail-row`, `dropdown-radio-menu`, `empty-state`, `event-rail-row`, `filter-dropdown`, `floating-jump-pill`, `flow-accordion-row`, `flow-graph`, `form-field`, `inline-link-action`, `loading-skeleton`, `menu-popover`, `needs-you-list`, `palette-group`, `qr-install-dialog`, `radio-card`, `search-list`, `segmented-control`, `settings-credential-list`, `settings-key-value-map-row`, `settings-section-card`, `settings-section-form`, `settings-tab-states`, `settings-toggle-row`, `sidebar-action-button`, `sidebar-card`, `slash-suggestion`, `state-card`, `stats-dialog`, `status-dot`, `status-history`, `tabs`

## S3 seeded set: 10 copy duplicates (CHE-5427)

Chosen from the 22 candidates in the batch files. Rule: the duplicate is a real locale or JSX copy, with file and line. Lines are on main at `b29d81efcc` (locales in `packages/views/locales/en/`). Each line was re-read in this issue.

| # | Duplicate | Copies (file:line) | Pattern |
|--:|---|---|---|
| 1 | One action, two words: "Retry" and "Try again". 11 "Retry" keys and 4 "Try again" keys. | Retry: `chat.json:17`, `chat.json:455`, `chat.json:590`, `issues.json:507`, `issues.json:1755`, `settings.json:126`, `settings.json:1677`, `editor.json:32`, `editor.json:42`, `runtimes.json:345`, `runtimes.json:730`. Try again: `chat.json:572`, `issues.json:7`, `issues.json:92`, `issues.json:2007`. | `inline-retry` |
| 2 | Error sentence with "Retry" inside it, used as a button label. Other surfaces keep message and action apart. | `chat.json:399` (`scheduled.load_failed`: "Couldn't load scheduled follow-ups. Retry"), `issues.json:1191` ("Could not load wakeups. Retry"). | `inline-retry` |
| 3 | `revoked_badge` = "revoked", 5 namespaces. | `settings.json:1064`, `:1173`, `:1241`, `:1280`, `:1352`. | `integration-tab` |
| 4 | `installed_at_label` = "Installed {{when}}", 4 namespaces. | `settings.json:1067`, `:1174`, `:1242`, `:1353`. | `integration-tab` |
| 5 | `not_enabled_title` = "<Provider> integration not enabled", 6 namespaces. | `settings.json:1052`, `:1131`, `:1161`, `:1230`, `:1268`, `:1339`. | `integration-tab` |
| 6 | `empty_title` = "No bots connected yet" (5 namespaces) and `loading` = "Loading…" (5 namespaces). | `empty_title`: `settings.json:1060`, `:1169`, `:1237`, `:1276`, `:1348`. `loading`: `settings.json:1059`, `:1168`, `:1236`, `:1275`, `:1346`. | `integration-tab` |
| 7 | `toast_disconnected` = "Disconnected <Provider> bot", 5 namespaces. | `settings.json:1073`, `:1180`, `:1249`, `:1287`, `:1359`. | `integration-tab` |
| 8 | `agent_bot_connected_label`: "Connected to <Provider>" in 4 namespaces, "Connected bot:" in DingTalk. One label, two wordings. | `settings.json:1081`, `:1186`, `:1253`, `:1291`, `:1363`. | `integration-tab` |
| 9 | Same revision badge, two keys. | `issues.json:2119` (`attachment_revision.badge`: "v{{revision}} · {{earlier}} earlier"), `issues.json:1616` (`deliverables.version_short`: "v{{version}}"). | `attachment-card-list` |
| 10 | Not-found copy in two blocks for the same job. | `invite.json:6` (`not_found`), `common.json:131` (`not_found`). | `state-card`, `empty-state` |

Held back (real, but not in the 10): `wakeup.show_instruction` / `hide_instruction` vs "Show more" vs `needs_you.show_all` (three label sets, S0b-2 item 9); wakeup instruction-form keys in two namespaces (S0b-2 item 8); literal "↑", "↓", "→" and " ICT" in JSX (S0b-1 item 5, S0b-2 item 10, S0b-3 item 8); hard-coded English in `visibility-picker.tsx` (S0b-3 D24).

## Root-cause verdict

Hypothesis (CHE-5420, cause 1): the fork drifted because a sibling surface already solved the job, and a pattern reference that named the sibling would have pointed the author to it.

**Verdict: SUPPORTED** for cause 1. Rule used: supported if 2/3 or more of items had a sibling and a reference would have pointed to it; partly supported from 1/3 to 2/3; not supported below 1/3.

| Measure | Count |
|---|--:|
| Drift items (corrected) | 126 |
| Sibling existed | 116 (92.1%) |
| Sibling existed and reference would have pointed | **106 (84.1%)** |
| Sibling existed, reference would not help | 10 (7.9%) |
| No sibling, so nothing to point to | 10 (7.9%) |

By kind: component 85 of 99 point (85.9%); layout 19 of 19; copy 2 of 8 (25%). Copy is the weak spot: for 6 of 8 copy items there is no sibling, or a pointer would not help.

The 20 items where a reference would not have pointed:
- 10 have no sibling: the recipe had no home yet (for example raw link style, literal glyphs, floating jump pill, radio list, mono path box, guard keys shown raw).
- 10 have a sibling, but a pointer would not help. 7 are copies of an unextracted copy: the 5 integration tabs (S0b-3 D7, D10, D13, D14), the auto-compact rows (D20), the runtime-profile select (D21) and the scheduling dialogs (D22). 2 have no shared helper or canonical to point to: the ICT formatter (S0b-4 D8) and the inline retry (S0b-4 D11). 1 is the sr-only description (S0b-1 D3), whose sibling repeats the same drift. A reference helps only after S1 names a shared component to build.

Caveats:
1. Code-only evidence. No rendered frame was compared. A visual drift that code cannot show is not counted.
2. 15 files were read by grep only (8 in batch 2, 7 in batch 3). Items there rest on grep hits.
3. Hindsight bias. The auditors saw the sibling before they judged "would have pointed". The yes answers may be generous.
4. Counts are recurrence-dominated. Many items repeat one recipe across files (5 integration tabs, 5 ICT formatters, 4 or more retry styles), so independent cases are fewer than 126.
5. One audit claim was refuted and one count differed (see Corrections). 8 of 10 spot-checked claims held.
6. **Cause 2** (the gates compare the wrong pair, CHE-1485 Layer A review pack) and **cause 3** (a lost decision, CHE-1594 to CHE-2085 doop) are not tested by this data. S0 owns cause 2.
7. S0b-1 found no doop tool in its run, so `issues/components/issue-decision-banner.tsx` was NOT-CAPTURED there. CHE-5534 captured it on the reference frame (see S4b raw-element files).

## Corrections to the batch files

The batch files stay unchanged. Totals above apply these two corrections.

1. **Refuted:** [S0b-2](ui-drift/s0b-2.md) D3 and the cross-file row in [S0b-1](ui-drift/s0b-1.md) say `hierarchy/hierarchy-dialog.tsx:85` has an sr-only description that repeats the dialog title. On main it renders `t(($) => $.hierarchy.dialog_description)`, which is "Pan and zoom the hierarchy. Open an issue to go to it." (`issues.json:26`). That is not the title. Effect: item count 127 to 126; copy 9 to 8; reference-no 21 to 20; `dialog-description` is dropped from S1. `decision-action-panel.tsx:53` (`<DialogDescription className="sr-only">{title}</DialogDescription>`) is the one confirmed instance.
2. **Count differs:** S0b-1 D4 says the recipe `rounded-xs border px-1.5 text-micro` is in 6 files. `grep` on main finds it in 3: `decision-request-card.tsx`, `decision-dm-card.tsx`, `decision-quote-card.tsx`. The 6-file figure does not hold for the exact string. The `status-pill` entry does not rely on it.

## S4b raw-element files (CHE-5430)

Reference frame (doop, Multica project file, page `patterns`): [S0b raw-element reference](https://congvc-c00.taila6fa8a.ts.net:34443/c/mRAnnjvGod?frame=tGd4_CW9vU). One labelled 1440 and 390 capture per file. `captured` = on the frame. `captured locally` = PNGs and HTML made by `run.sh`, not yet on the frame. Captured by `scripts/design/pattern-harness/run.sh` on X99.

1. `issues/components/issue-decision-banner.tsx` (batch 1, raw*, not marked S4b) — captured (CHE-5534)
2. `issues/components/hierarchy/hierarchy-dialog.tsx` (batch 2, S4b) — captured (CHE-5534)
3. `issues/components/hierarchy/hierarchy-minimap.tsx` (batch 2, S4b) — captured (CHE-5534)
4. `chat/components/wakeup-event-row.tsx` (batch 2, S4b) — captured (CHE-5534)
5. `issues/components/wakeup-condition-params.tsx` (batch 2, S4b) — captured (CHE-5534)
6. `issues/components/wakeup-fields.tsx` (batch 2, S4b) — captured (CHE-5535)
7. `issues/components/wakeup-pickers.tsx` (batch 2, S4b) — captured (CHE-5535)
8. `settings/components/workspace-wakeup-rules.tsx` (batch 2, S4b) — captured (CHE-5535)
9. `chat/components/chat-search.tsx` (batch 2, S4b) — captured (CHE-5535)
10. `chat/components/config-change-viewer.tsx` (batch 2, S4b) — captured (CHE-5535)
11. `chat/components/decision-artifacts.tsx` (batch 2, S4b) — captured locally (CHE-5536); frame write NOT-WRITTEN: no doop access
12. `chat/components/decision-request-card.tsx` (batch 2, S4b) — captured locally (CHE-5536); frame write NOT-WRITTEN: no doop access
13. `chat/components/needs-you-list.tsx` (batch 2, S4b) — captured locally (CHE-5536); frame write NOT-WRITTEN: no doop access
14. `chat/components/quick-agent-bar.tsx` (batch 2, S4b) — captured locally (CHE-5536); frame write NOT-WRITTEN: no doop access
15. `settings/components/delegated-writes-section.tsx` (batch 3, S4b) — captured locally (CHE-5536); frame write NOT-WRITTEN: no doop access
16. `settings/components/dingtalk-tab.tsx` (batch 3, S4b)
17. `settings/components/lark-tab.tsx` (batch 3, S4b)
18. `settings/components/slack-tab.tsx` (batch 3, S4b)
19. `settings/components/telegram-tab.tsx` (batch 3, S4b)
20. `settings/components/wecom-tab.tsx` (batch 3, S4b)
21. `agents/components/create-agent-dialog.tsx` (batch 3, S4b)
22. `agents/components/inspector/concurrency-picker.tsx` (batch 3, S4b)
23. `agents/components/inspector/skill-attach.tsx` (batch 3, S4b)
24. `agents/components/inspector/visibility-picker.tsx` (batch 3, S4b)
25. `agents/components/instructions-editor.tsx` (batch 3, S4b)
26. `autopilots/components/add-trigger-dialog.tsx` (batch 4, S4b)
27. `common/expandable-description.tsx` (batch 4, S4b)
28. `editor/html-viewer.tsx` (batch 4, S4b)
29. `editor/plan-revision-viewer.tsx` (batch 4, S4b)
30. `labels/resource-label-picker.tsx` (batch 4, S4b)
31. `media/media-context.tsx` (batch 4, S4b)
32. `media/media-page.tsx` (batch 4, S4b)
33. `projects/components/local-directory-mode-dialog.tsx` (batch 4, S4b)
34. `rich-content/next-self-mention.tsx` (batch 4, S4b)
35. `workflows/project-workflow-menu.tsx` (batch 4, S4b)

Count: 35 raw-element files from the batch plan (`scripts/design/output/s0b-batch-plan.md`). 34 are marked `screenshot: S4b` in their batch (S0b-2: 13, S0b-3: 11, S0b-4: 10). 1 is in S0b-1, marked NOT-CAPTURED (no doop tool in that run): `issues/components/issue-decision-banner.tsx`. No frame was captured in S0b-1 to S0b-4. CHE-5534 (S4b-SS1) captured 5 of the 35: items 1 to 5. CHE-5535 (S4b-SS2) captured 5 more: items 6 to 10. CHE-5536 (S4b-SS3) made local captures of 5 more: items 11 to 15. The doop frame write for them is NOT-WRITTEN: this run had no doop MCP access (`POST /mcp` returned `Unauthorized: this MCP server requires OAuth`). 10 of 35 captured on the frame; 15 of 35 captured locally.
