# S0b batch plan (CHE-5420, written by S0a / CHE-5423)

Scan: `scripts/design/output/fork-ui-scan.md`, ref `de59132eab`, base `250ecacf4a` (upstream/main). 118 fork-only UI files, 35 with raw elements.

## Reference screens

Cheese had named none on CHE-5420 when S0a started (13 Nov 2026, ICT). Default used: **issue detail page, inbox, settings**. S0b compares every file against these three. Cheese can replace them; only the screenshot step of S0b then changes.

## Cost model (estimate, not measured)

Minutes of work per file: 4 for 150 lines or fewer, 8 for 151 to 400, 15 for more than 400. Plus 5 for a file with raw elements (sibling lookup and a screenshot on the doop reference frame).

Per batch: work 80 min or less, plus 10 min setup and 30 min for PR and review overhead = 120 min or less. Total work for 118 files: 906 min.

**Result: S0b needs 13 batches. It is split into S0b-1 to S0b-13 before it starts (parent author).** One PR per batch; each batch writes its own audit rows file, so batches do not conflict. S0b-13 is small; fold it into S0b-12 if calibration shows the estimate is high.

Calibration: run S0b-1 first and record actual minutes per file. If actual is more than 20% off the estimate, the parent author re-sizes the remaining batches before releasing them (same rule as S4b).

| Batch | Surface | Files | Work min | Total min (with overhead) |
| --- | --- | ---: | ---: | ---: |
| S0b-1 | issues | 15 | 77 | 117 |
| S0b-2 | issues, hierarchy, wakeups | 10 | 76 | 116 |
| S0b-3 | wakeups, chat/DM | 10 | 78 | 118 |
| S0b-4 | chat/DM | 10 | 71 | 111 |
| S0b-5 | chat/DM | 10 | 78 | 118 |
| S0b-6 | chat/DM, settings | 9 | 73 | 113 |
| S0b-7 | settings | 8 | 76 | 116 |
| S0b-8 | settings, other | 6 | 64 | 104 |
| S0b-9 | other | 9 | 80 | 120 |
| S0b-10 | other | 10 | 74 | 114 |
| S0b-11 | other | 9 | 68 | 108 |
| S0b-12 | other | 10 | 79 | 119 |
| S0b-13 | other | 2 | 12 | 52 |

## S0b-1 (77 min work, 15 files)

- `issues/compact-issue-chips-context.tsx` (24 lines, 4 min)
- `issues/components/attachment-list.tsx` (104 lines, 4 min)
- `issues/components/attachment-revision-diff.tsx` (96 lines, 4 min)
- `issues/components/batch-workflow-picker.tsx` (104 lines, 4 min)
- `issues/components/comment-mention-card.tsx` (65 lines, 4 min)
- `issues/components/decision-action-panel.tsx` (77 lines, 4 min)
- `issues/components/decision-quote-card.tsx` (371 lines, 8 min)
- `issues/components/description-disclosure.tsx` (196 lines, 8 min)
- `issues/components/issue-decision-banner.tsx` (110 lines, 9 min, raw)
- `issues/components/issue-detail-lazy.tsx` (20 lines, 4 min)
- `issues/components/issue-inbox-restore-button.tsx` (46 lines, 4 min)
- `issues/components/run-budget-chip.tsx` (24 lines, 4 min)
- `issues/components/run-budget-meter.tsx` (113 lines, 4 min)
- `issues/components/status-history-button.tsx` (69 lines, 4 min)
- `issues/components/status-history-dialog.tsx` (187 lines, 8 min)

## S0b-2 (76 min work, 10 files)

- `issues/components/status-summary.tsx` (21 lines, 4 min)
- `issues/components/supersession-links.tsx` (62 lines, 4 min)
- `issues/components/workflow-pill.tsx` (18 lines, 4 min)
- `issues/components/hierarchy/hierarchy-dialog.tsx` (232 lines, 13 min, raw)
- `issues/components/hierarchy/hierarchy-minimap.tsx` (111 lines, 9 min, raw)
- `issues/components/hierarchy/hierarchy-parts.tsx` (87 lines, 4 min)
- `chat/components/scheduled-wakeups-chip.tsx` (104 lines, 4 min)
- `chat/components/wakeup-event-row.tsx` (186 lines, 13 min, raw)
- `issues/components/issue-wakeup-defaults.tsx` (273 lines, 8 min)
- `issues/components/wakeup-condition-params.tsx` (326 lines, 13 min, raw)

## S0b-3 (78 min work, 10 files)

- `issues/components/wakeup-fields.tsx` (263 lines, 13 min, raw)
- `issues/components/wakeup-pickers.tsx` (336 lines, 13 min, raw)
- `issues/components/wakeups-paused-banner.tsx` (37 lines, 4 min)
- `projects/components/project-wakeups-section.tsx` (25 lines, 4 min)
- `settings/components/wakeups-tab.tsx` (226 lines, 8 min)
- `settings/components/workspace-wakeup-rules.tsx` (413 lines, 20 min, raw)
- `chat/components/annotatable-answer.tsx` (33 lines, 4 min)
- `chat/components/chat-annotations.tsx` (137 lines, 4 min)
- `chat/components/chat-context-meter.tsx` (58 lines, 4 min)
- `chat/components/chat-preset-picker.tsx` (67 lines, 4 min)

## S0b-4 (71 min work, 10 files)

- `chat/components/chat-search.tsx` (362 lines, 13 min, raw)
- `chat/components/chat-session-mention-card.tsx` (42 lines, 4 min)
- `chat/components/chat-session-stats-dialog.tsx` (287 lines, 8 min)
- `chat/components/chat-supplement-status.tsx` (110 lines, 4 min)
- `chat/components/config-change-viewer.tsx` (142 lines, 9 min, raw)
- `chat/components/continuation-block.tsx` (74 lines, 4 min)
- `chat/components/decision-answer-content.tsx` (33 lines, 4 min)
- `chat/components/decision-artifacts.tsx` (324 lines, 13 min, raw)
- `chat/components/decision-dm-card.tsx` (195 lines, 8 min)
- `chat/components/decision-recent-runs.tsx` (52 lines, 4 min)

## S0b-5 (78 min work, 10 files)

- `chat/components/decision-request-card.tsx` (638 lines, 20 min, raw)
- `chat/components/decision-withdrawal-card.tsx` (36 lines, 4 min)
- `chat/components/deferred-content-editor.tsx` (194 lines, 8 min)
- `chat/components/diff-hunks.tsx` (60 lines, 4 min)
- `chat/components/interactive-login-banner.tsx` (21 lines, 4 min)
- `chat/components/lazy-content-editor.tsx` (18 lines, 4 min)
- `chat/components/needs-you-list.tsx` (359 lines, 13 min, raw)
- `chat/components/open-decisions-badge.tsx` (64 lines, 4 min)
- `chat/components/predecessor-dms.tsx` (174 lines, 8 min)
- `chat/components/quick-agent-bar.tsx` (116 lines, 9 min, raw)

## S0b-6 (73 min work, 9 files)

- `chat/components/use-archived-send.tsx` (116 lines, 4 min)
- `chat/components/use-chat-annotations.tsx` (81 lines, 4 min)
- `chat/components/use-dm-session-surface.tsx` (162 lines, 8 min)
- `settings/components/agent-decisions-row.tsx` (111 lines, 4 min)
- `settings/components/auto-compact-tokens-row.tsx` (75 lines, 4 min)
- `settings/components/composio-tab.tsx` (353 lines, 8 min)
- `settings/components/delegated-writes-section.tsx` (301 lines, 13 min, raw)
- `settings/components/dingtalk-tab.tsx` (1159 lines, 20 min, raw)
- `settings/components/export-privacy-tab.tsx` (308 lines, 8 min)

## S0b-7 (76 min work, 8 files)

- `settings/components/github-review-mappings.tsx` (188 lines, 8 min)
- `settings/components/idle-guard-rows.tsx` (131 lines, 4 min)
- `settings/components/lark-tab.tsx` (991 lines, 20 min, raw)
- `settings/components/passkeys-tab.tsx` (279 lines, 8 min)
- `settings/components/provider-breakers-tab.tsx` (162 lines, 8 min)
- `settings/components/routing-agent-rows.tsx` (119 lines, 4 min)
- `settings/components/runtime-profile-row.tsx` (87 lines, 4 min)
- `settings/components/slack-tab.tsx` (602 lines, 20 min, raw)

## S0b-8 (64 min work, 6 files)

- `settings/components/telegram-tab.tsx` (557 lines, 20 min, raw)
- `settings/components/wecom-scan-install-dialog.tsx` (300 lines, 8 min)
- `settings/components/wecom-tab.tsx` (635 lines, 20 min, raw)
- `settings/components/workspace-scheduling-row.tsx` (96 lines, 4 min)
- `agents/components/agent-auto-compact-section.tsx` (120 lines, 4 min)
- `agents/components/agent-scheduling.tsx` (192 lines, 8 min)

## S0b-9 (80 min work, 9 files)

- `agents/components/create-agent-dialog.tsx` (670 lines, 20 min, raw)
- `agents/components/inspector/agent-presets-section.tsx` (50 lines, 4 min)
- `agents/components/inspector/concurrency-picker.tsx` (94 lines, 9 min, raw)
- `agents/components/inspector/runtime-profile-field.tsx` (67 lines, 4 min)
- `agents/components/inspector/skill-attach.tsx` (58 lines, 9 min, raw)
- `agents/components/inspector/visibility-picker.tsx` (86 lines, 9 min, raw)
- `agents/components/instructions-editor.tsx` (116 lines, 9 min, raw)
- `agents/components/runtime-machine-filter-dropdown.tsx` (194 lines, 8 min)
- `agents/components/tabs/agent-mcp-tab.tsx` (209 lines, 8 min)

## S0b-10 (74 min work, 10 files)

- `agents/components/tabs/integrations-tab.tsx` (443 lines, 15 min)
- `agents/create/ai-create-agent-page.tsx` (92 lines, 4 min)
- `agents/create/builder-setup-panel.tsx` (75 lines, 4 min)
- `autopilots/components/add-trigger-dialog.tsx` (194 lines, 13 min, raw)
- `autopilots/components/autopilot-dialog-lazy.tsx` (18 lines, 4 min)
- `common/decision-action-approval.tsx` (230 lines, 8 min)
- `common/expandable-description.tsx` (81 lines, 9 min, raw)
- `common/task-transcript/run-preset-details.tsx` (40 lines, 4 min)
- `editor/extensions/preset-arg-suggestion.tsx` (81 lines, 4 min)
- `editor/html-viewer.tsx` (30 lines, 9 min, raw)

## S0b-11 (68 min work, 9 files)

- `editor/plan-revision-viewer.tsx` (221 lines, 13 min, raw)
- `invite/invite-page.tsx` (298 lines, 8 min)
- `labels/resource-label-picker.tsx` (124 lines, 9 min, raw)
- `layout/machine-pause-banner.tsx` (156 lines, 8 min)
- `layout/open-decisions-chip.tsx` (35 lines, 4 min)
- `layout/workspace-pause-banner.tsx` (71 lines, 4 min)
- `media/media-context.tsx` (99 lines, 9 min, raw)
- `media/media-page.tsx` (72 lines, 9 min, raw)
- `projects/components/github-ref-dialog.tsx` (105 lines, 4 min)

## S0b-12 (79 min work, 10 files)

- `projects/components/local-directory-mode-dialog.tsx` (239 lines, 13 min, raw)
- `rich-content/next-self-mention.tsx` (179 lines, 13 min, raw)
- `runtimes/components/machine-pause-card.tsx` (341 lines, 8 min)
- `search/delegate/delegate-group.tsx` (123 lines, 4 min)
- `search/lazy-search-command.tsx` (24 lines, 4 min)
- `workflows/batch-handoff-confirm-dialog.tsx` (168 lines, 8 min)
- `workflows/project-workflow-menu.tsx` (247 lines, 13 min, raw)
- `workflows/workflow-budget-request-banner.tsx` (59 lines, 4 min)
- `workflows/workflow-choice-pill.tsx` (60 lines, 4 min)
- `workflows/workflow-edge-editor.tsx` (212 lines, 8 min)

## S0b-13 (12 min work, 2 files)

- `workflows/workflow-graph.tsx` (252 lines, 8 min)
- `workflows/workflow-override-options.tsx` (52 lines, 4 min)
