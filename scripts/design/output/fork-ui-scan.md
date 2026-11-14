# Fork-only UI scan

- ref: `de59132eab6f4bd50e3ddc7d2654ecf3d9d27831`
- base: `250ecacf4ab17a4bdaa7bde3a3cec2ed14ce15bb` (upstream)
- scope: `packages/views`, `.tsx` files, tests excluded
- fork-only UI files: **118** (114 new + 4 git-detected renames of upstream files, marked "renamed from")
- with raw `<button|input|select|textarea>`: **35** (30%)
- with a hex colour literal: **0**
- all of `packages/views`: 214 of 628 files with raw elements (34%)

| Surface | Files | With raw | Raw tags | Hex |
| --- | ---: | ---: | ---: | ---: |
| issues | 18 | 1 | 1 | 0 |
| chat/DM | 27 | 6 | 16 | 0 |
| settings | 18 | 6 | 20 | 0 |
| wakeups | 10 | 5 | 10 | 0 |
| hierarchy | 3 | 2 | 5 | 0 |
| other | 42 | 15 | 22 | 0 |

## issues (18)

| File | Raw | Hex |
| --- | ---: | ---: |
| `packages/views/issues/compact-issue-chips-context.tsx` |  |  |
| `packages/views/issues/components/attachment-list.tsx` |  |  |
| `packages/views/issues/components/attachment-revision-diff.tsx` |  |  |
| `packages/views/issues/components/batch-workflow-picker.tsx` |  |  |
| `packages/views/issues/components/comment-mention-card.tsx` |  |  |
| `packages/views/issues/components/decision-action-panel.tsx` |  |  |
| `packages/views/issues/components/decision-quote-card.tsx` |  |  |
| `packages/views/issues/components/description-disclosure.tsx` |  |  |
| `packages/views/issues/components/issue-decision-banner.tsx` | 1 |  |
| `packages/views/issues/components/issue-detail-lazy.tsx` |  |  |
| `packages/views/issues/components/issue-inbox-restore-button.tsx` (renamed from `packages/views/issues/actions/inbox-restore.tsx`) |  |  |
| `packages/views/issues/components/run-budget-chip.tsx` |  |  |
| `packages/views/issues/components/run-budget-meter.tsx` |  |  |
| `packages/views/issues/components/status-history-button.tsx` |  |  |
| `packages/views/issues/components/status-history-dialog.tsx` |  |  |
| `packages/views/issues/components/status-summary.tsx` |  |  |
| `packages/views/issues/components/supersession-links.tsx` |  |  |
| `packages/views/issues/components/workflow-pill.tsx` |  |  |

## chat/DM (27)

| File | Raw | Hex |
| --- | ---: | ---: |
| `packages/views/chat/components/annotatable-answer.tsx` |  |  |
| `packages/views/chat/components/chat-annotations.tsx` |  |  |
| `packages/views/chat/components/chat-context-meter.tsx` |  |  |
| `packages/views/chat/components/chat-preset-picker.tsx` |  |  |
| `packages/views/chat/components/chat-search.tsx` | 5 |  |
| `packages/views/chat/components/chat-session-mention-card.tsx` |  |  |
| `packages/views/chat/components/chat-session-stats-dialog.tsx` |  |  |
| `packages/views/chat/components/chat-supplement-status.tsx` |  |  |
| `packages/views/chat/components/config-change-viewer.tsx` | 1 |  |
| `packages/views/chat/components/continuation-block.tsx` |  |  |
| `packages/views/chat/components/decision-answer-content.tsx` |  |  |
| `packages/views/chat/components/decision-artifacts.tsx` | 3 |  |
| `packages/views/chat/components/decision-dm-card.tsx` |  |  |
| `packages/views/chat/components/decision-recent-runs.tsx` |  |  |
| `packages/views/chat/components/decision-request-card.tsx` | 2 |  |
| `packages/views/chat/components/decision-withdrawal-card.tsx` |  |  |
| `packages/views/chat/components/deferred-content-editor.tsx` |  |  |
| `packages/views/chat/components/diff-hunks.tsx` |  |  |
| `packages/views/chat/components/interactive-login-banner.tsx` |  |  |
| `packages/views/chat/components/lazy-content-editor.tsx` |  |  |
| `packages/views/chat/components/needs-you-list.tsx` | 3 |  |
| `packages/views/chat/components/open-decisions-badge.tsx` |  |  |
| `packages/views/chat/components/predecessor-dms.tsx` |  |  |
| `packages/views/chat/components/quick-agent-bar.tsx` | 2 |  |
| `packages/views/chat/components/use-archived-send.tsx` |  |  |
| `packages/views/chat/components/use-chat-annotations.tsx` |  |  |
| `packages/views/chat/components/use-dm-session-surface.tsx` |  |  |

## settings (18)

| File | Raw | Hex |
| --- | ---: | ---: |
| `packages/views/settings/components/agent-decisions-row.tsx` |  |  |
| `packages/views/settings/components/auto-compact-tokens-row.tsx` |  |  |
| `packages/views/settings/components/composio-tab.tsx` |  |  |
| `packages/views/settings/components/delegated-writes-section.tsx` | 2 |  |
| `packages/views/settings/components/dingtalk-tab.tsx` | 8 |  |
| `packages/views/settings/components/export-privacy-tab.tsx` |  |  |
| `packages/views/settings/components/github-review-mappings.tsx` |  |  |
| `packages/views/settings/components/idle-guard-rows.tsx` |  |  |
| `packages/views/settings/components/lark-tab.tsx` | 1 |  |
| `packages/views/settings/components/passkeys-tab.tsx` |  |  |
| `packages/views/settings/components/provider-breakers-tab.tsx` |  |  |
| `packages/views/settings/components/routing-agent-rows.tsx` |  |  |
| `packages/views/settings/components/runtime-profile-row.tsx` |  |  |
| `packages/views/settings/components/slack-tab.tsx` | 4 |  |
| `packages/views/settings/components/telegram-tab.tsx` | 3 |  |
| `packages/views/settings/components/wecom-scan-install-dialog.tsx` (renamed from `packages/views/settings/components/wecom-channel-authorization.tsx`) |  |  |
| `packages/views/settings/components/wecom-tab.tsx` | 2 |  |
| `packages/views/settings/components/workspace-scheduling-row.tsx` |  |  |

## wakeups (10)

| File | Raw | Hex |
| --- | ---: | ---: |
| `packages/views/chat/components/scheduled-wakeups-chip.tsx` |  |  |
| `packages/views/chat/components/wakeup-event-row.tsx` | 1 |  |
| `packages/views/issues/components/issue-wakeup-defaults.tsx` |  |  |
| `packages/views/issues/components/wakeup-condition-params.tsx` | 2 |  |
| `packages/views/issues/components/wakeup-fields.tsx` | 1 |  |
| `packages/views/issues/components/wakeup-pickers.tsx` | 5 |  |
| `packages/views/issues/components/wakeups-paused-banner.tsx` |  |  |
| `packages/views/projects/components/project-wakeups-section.tsx` |  |  |
| `packages/views/settings/components/wakeups-tab.tsx` |  |  |
| `packages/views/settings/components/workspace-wakeup-rules.tsx` | 1 |  |

## hierarchy (3)

| File | Raw | Hex |
| --- | ---: | ---: |
| `packages/views/issues/components/hierarchy/hierarchy-dialog.tsx` | 3 |  |
| `packages/views/issues/components/hierarchy/hierarchy-minimap.tsx` | 2 |  |
| `packages/views/issues/components/hierarchy/hierarchy-parts.tsx` |  |  |

## other (42)

| File | Raw | Hex |
| --- | ---: | ---: |
| `packages/views/agents/components/agent-auto-compact-section.tsx` |  |  |
| `packages/views/agents/components/agent-scheduling.tsx` |  |  |
| `packages/views/agents/components/create-agent-dialog.tsx` | 3 |  |
| `packages/views/agents/components/inspector/agent-presets-section.tsx` |  |  |
| `packages/views/agents/components/inspector/concurrency-picker.tsx` | 1 |  |
| `packages/views/agents/components/inspector/runtime-profile-field.tsx` |  |  |
| `packages/views/agents/components/inspector/skill-attach.tsx` | 1 |  |
| `packages/views/agents/components/inspector/visibility-picker.tsx` | 1 |  |
| `packages/views/agents/components/instructions-editor.tsx` | 1 |  |
| `packages/views/agents/components/runtime-machine-filter-dropdown.tsx` |  |  |
| `packages/views/agents/components/tabs/agent-mcp-tab.tsx` |  |  |
| `packages/views/agents/components/tabs/integrations-tab.tsx` |  |  |
| `packages/views/agents/create/ai-create-agent-page.tsx` |  |  |
| `packages/views/agents/create/builder-setup-panel.tsx` |  |  |
| `packages/views/autopilots/components/add-trigger-dialog.tsx` | 1 |  |
| `packages/views/autopilots/components/autopilot-dialog-lazy.tsx` |  |  |
| `packages/views/common/decision-action-approval.tsx` |  |  |
| `packages/views/common/expandable-description.tsx` | 1 |  |
| `packages/views/common/task-transcript/run-preset-details.tsx` |  |  |
| `packages/views/editor/extensions/preset-arg-suggestion.tsx` |  |  |
| `packages/views/editor/html-viewer.tsx` | 4 |  |
| `packages/views/editor/plan-revision-viewer.tsx` | 1 |  |
| `packages/views/invite/invite-page.tsx` |  |  |
| `packages/views/labels/resource-label-picker.tsx` | 2 |  |
| `packages/views/layout/machine-pause-banner.tsx` |  |  |
| `packages/views/layout/open-decisions-chip.tsx` |  |  |
| `packages/views/layout/workspace-pause-banner.tsx` |  |  |
| `packages/views/media/media-context.tsx` | 1 |  |
| `packages/views/media/media-page.tsx` | 2 |  |
| `packages/views/projects/components/github-ref-dialog.tsx` |  |  |
| `packages/views/projects/components/local-directory-mode-dialog.tsx` (renamed from `packages/views/projects/components/local-directory-mode-options.tsx`) | 1 |  |
| `packages/views/rich-content/next-self-mention.tsx` | 1 |  |
| `packages/views/runtimes/components/machine-pause-card.tsx` |  |  |
| `packages/views/search/delegate/delegate-group.tsx` |  |  |
| `packages/views/search/lazy-search-command.tsx` |  |  |
| `packages/views/workflows/batch-handoff-confirm-dialog.tsx` |  |  |
| `packages/views/workflows/project-workflow-menu.tsx` (renamed from `packages/views/workflows/project-workflow-sheet.tsx`) | 1 |  |
| `packages/views/workflows/workflow-budget-request-banner.tsx` |  |  |
| `packages/views/workflows/workflow-choice-pill.tsx` |  |  |
| `packages/views/workflows/workflow-edge-editor.tsx` |  |  |
| `packages/views/workflows/workflow-graph.tsx` |  |  |
| `packages/views/workflows/workflow-override-options.tsx` |  |  |
