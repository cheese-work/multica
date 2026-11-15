# Upstream overlap re-check

Companion to [FORK-FEATURES.md](FORK-FEATURES.md) (CHE-4789). Re-check of every ledger row against current upstream `main`.

- Fork `main`: `e8f066f3eaba3a2f3ea2f832b2a962e4356e310a` (2026-11-09 08:44 ICT).
- Upstream `main` pin: `73975653323750d04c27047ea9c986ef06bdf6a1` (2026-11-08 13:35 ICT). Upstream tag `v0.7.0`: `dbdbb299164347586e2650bb67684a490726a798` (the baseline of the `Changed since v0.7.0` column). Latest upstream tag `v0.8.1`: `b1ea927def6490c43bedff537b6f4ed9a47fa828` (2026-11-06 16:51 ICT); the pin is 15 commits ahead of it.
- Evidence for the 80 new rows was read at the pin. The 168 earlier rows were re-checked at the pin by delta only: 134 upstream commits since the previous pin `3794359792e1e310d89d0488b46efff66e9d2369` (2026-11-02 19:02 ICT) changed 1053 files. Rows whose upstream files changed were re-read; 6 rows (H04, H05, C17, D03, G23, O17) got new evidence text, and no row changed status.
- Method: search upstream `main` for the row's code (`git grep`, `git show upstream/main:<path>`, `git log v0.7.0..upstream/main -- <path>`).
  A title match alone is never `overlaps` or `upstreamed`; the evidence column names the upstream file, symbol or commit.
- **fork-only**: no upstream code covers the behavior. **overlaps**: upstream covers part; the recommendation says what the fork still adds. **upstreamed**: upstream covers all of it.
- **Changed since v0.7.0**: upstream touched the overlapping code after the tag (matters for the reconcile step).
- Confidence `high` = direct file or symbol hit or a directory absent upstream; `medium` = absence search; `low` = one weak signal.

## Totals

| Status | Rows |
| --- | --- |
| fork-only | 213 |
| overlaps | 33 |
| upstreamed | 2 |

## Rows

| Row | Status | Upstream refs | Changed since v0.7.0 | Confidence | Evidence |
| --- | --- | --- | --- | --- | --- |
| C01 | overlaps | f896cc483; f1560daff; MUL-8074 | no | medium | Prior overlap on issue-detail long-thread/description handling kept from old ledger; fork files description-disclosure.tsx and thread-disclosure.ts exist at HEAD; upstream gained unrelated issue-detail changes since v0.7.0 (7c186016a5, 045fcdf19a). |
| C02 | overlaps | 4b8873df3 | no | medium | Old overlap ref kept; fork comment.go/cmd_issue.go changes are in shared files; upstream still has the resolve path. |
| C03 | overlaps | 5ed09342a; MUL-7680 | no | high | Upstream has the child_done system rule (server/internal/service/issue_wakeup_system.go, scheduler/jobs_child_done.go, issue_child_done_test.go); fork's issue_child_done.go plus migration 509_stage_completion_wake adds generation receipts and dedupe. |
| C04 | fork-only | - | no | high | No upstream status/ETA/CI/PR-head answer path; upstream github.go issuePullRequestRowToResponse (github.go:232) is only reused by fork comment_status_query.go. |
| C05 | fork-only | - | no | high | No upstream verified DM auth provenance (docs/chat-auth-provenance.md, auth/session.go, migrations 594/595 are fork-only). |
| C06 | fork-only | - | no | high | No upstream decision_intent/decision_request code; only comment_decision_test.go and task_claim_decision* (unrelated claim logic). |
| C07 | fork-only | - | no | high | No upstream decision records, disclaim, veto or class code. |
| C08 | fork-only | - | no | high | No upstream decision delivery worker or cmd_decision CLI. |
| C09 | fork-only | - | no | high | No upstream decision request API or intent reuse. |
| C10 | fork-only | - | no | high | No upstream decision ping unread state; use-realtime-sync.ts exists upstream but has no decision ping handling. |
| C11 | fork-only | - | no | high | No upstream chat_continue/chat_compact_prompt/slash command; upstream chat-input has no /continue. |
| C12 | fork-only | - | no | medium | No upstream auto_compact, chat_usage or chat-context-meter files; upstream 'compact' hits are the responsive useIsCompact hook only. |
| C13 | fork-only | - | no | high | No upstream chat_fork.go or use-chat-fork.ts. |
| C14 | overlaps | 51aa92412; 4389b0f075 | no | medium | Upstream has session-rename-input.tsx (v0.7.0) and chat.go title rename input cap (chatSessionTitleMaxLen); fork adds chat_rename.go and use-chat-rename.ts so rename is not an agent turn. |
| C15 | fork-only | - | no | high | No upstream chat_search.go; upstream chat.go has ListChatMessagesPage but no ?around= window or search endpoint. |
| C16 | fork-only | - | no | medium | Upstream notification_preference.go has only group switches plus a system_notifications toggle; no notification_channel table/handler and no DM reply coalescing. |
| C17 | fork-only | - | no | medium | Upstream chat-page.tsx navigates with replace() (lines 126, 232, 238) and has no history-entry back stack or navIntent; fork chat-page.tsx:136,563 follows real history. |
| C18 | overlaps | a51ab4d551; MUL-4171 | no | high | Upstream has SetChatSessionPinned (queries/chat.sql:258) and archive flow from Chat V2; it has no archived-pin guard and no migration clearing pinned_at on archived rows. |
| C19 | fork-only | - | no | medium | Upstream formats locale time only in chat-thread-list.tsx:49-54 (list rows); no message-time.ts or bubble timestamps. |
| C20 | fork-only | - | no | medium | Upstream has reply annotations for issue comments (core/drafts/reply-annotation.ts, comment-annotation-selection.ts) which the fork reuses; no DM chat annotation components upstream. |
| C21 | fork-only | - | no | medium | Upstream issue-chip.tsx has no refetchOnWindowFocus; fork issue-chip.tsx:70 sets it. |
| C22 | fork-only | - | no | medium | Upstream has no opensearch.xml route or site-search-tab; only the openSearch shortcut id (core/shortcuts/definitions.ts:9,80) and the Cmd+K command bar. Cmd+K itself exists upstream (fb49f55924 reworked search-command.tsx). |
| C23 | fork-only | - | no | medium | Source files are fork tests asserting lane summaries stay removed; upstream has no lane_summary code. |
| C24 | fork-only | - | no | high | No upstream decisiontx or dmnotify packages; upstream only has comment_decision_test.go and task_claim_decision*. |
| C25 | fork-only | - | no | high | No upstream decision cards, answer receipts or chat_decision_block; upstream chat has no decision handling. |
| C26 | fork-only | - | no | high | No decision_request/decisiontx/needs-you code anywhere in upstream/main server, apps or packages (git grep empty). |
| C27 | fork-only | - | no | high | No decisiontx package or artifact hash check upstream (git grep decisiontx empty). |
| C28 | fork-only | - | no | high | No decision topic/feed/open-asks CLI or handlers upstream (git grep decision_request empty). |
| C29 | fork-only | - | no | high | No decisionaction package or executable decision actions upstream. |
| C30 | fork-only | - | no | medium | No webauthn/passkey code or session generation upstream (git grep webauthn/passkey empty in server and packages). |
| C31 | fork-only | - | no | high | No chat wakeup handler/service/CLI upstream (git grep chat_wakeup/ChatWakeup empty). |
| C32 | fork-only | - | no | high | No predecessor DM chain upstream (git grep predecessor/continued_from_session in chat and server empty). |
| C33 | fork-only | - | no | high | No reading-anchor or jump-to-latest code upstream. |
| C34 | fork-only | - | no | medium | Upstream has no chat_compact handler, compaction epoch or compact CLI (only codex compaction tests and chat_pinned_agent.go compact wording). |
| C35 | overlaps | `packages/views/chat/components/chat-queue.tsx` | no | medium | upstream/main packages/views/chat/components/chat-queue.tsx:60,144 has a queue steer action (queue.steer), present at v0.7.0; no chat_supplement handler/service, chat-steer.ts or supplement status upstream. |
| C36 | overlaps | `apps/mobile/app/(app)/[workspace]/(tabs)/chat.tsx` | no | low | upstream/main mobile chat.tsx:224,491 handles status "archived" (composer disabled) and server chat_archive_cancel_test.go archives sessions; no statistics dialog, chat usage summary or session pin commands upstream (upstream pins agents: chat_pinned_agent.go). |
| C37 | fork-only | - | no | high | No opensearch descriptor or /api/search/suggestions proxy upstream (git grep opensearch in apps/web empty); upstream search commit fb49f55924 is the Cmd+K command bar, a different surface. |
| C38 | fork-only | - | no | medium | Upstream rich-content.tsx has no viewer-mention or own-comment highlight and no next-mention jump; the only upstream rich-content change since v0.7.0 is icon pass f0c361ebca. |
| C39 | fork-only | - | no | high | No comment_preview endpoint or comment-ID chips upstream; upstream timeline-list.tsx:52 highlightCommentId is an inbox deep-link highlight, not a chip. |
| C40 | fork-only | - | no | medium | Upstream has no status_summary or TriggerThreadContext (git grep empty); reply_instructions.go only offers a thread read command (BuildNewCommentsHint, MUL-7344), not an injected transcript. |
| C41 | fork-only | - | no | high | No plan-revision compare or supersession code upstream (git grep supersession/plan_revision empty); upstream "revision" hits are issue cache revisions. |
| C42 | fork-only | - | no | medium | No media page route or media-page.tsx upstream (git grep media-viewer//media in apps/desktop and apps/web/app empty); upstream attachment-preview-modal is a modal, not a page. |
| C43 | overlaps | MUL-4525 | no | medium | upstream/main handler/admission.go (MUL-4525) and comment.go:85,101 TriggerOutcomes plus self-trigger suppression (comment.go:2329) already gate agent comment wakes; the fork humanask CLI refusal was removed (CHE-3366). Fork admission.go differs by 26+/16- lines. |
| C44 | fork-only | - | no | high | No issue relations endpoint, CLI verb or graph upstream (git grep relations in router and cmd/multica empty). |
| C45 | fork-only | - | no | high | No hierarchy minimap upstream (git grep hierarchy in packages/views/issues finds only unrelated header/source-context files). |
| C46 | overlaps | `packages/views/editor/hooks/use-zoom-canvas.ts` | no | medium | upstream/main has packages/views/editor/mermaid-viewer.tsx and hooks/use-zoom-canvas.ts (wheel/pinch zoom), both present at v0.7.0; no mermaid-orientation, gesture-session or mermaid-theme upstream. |
| C47 | overlaps | `server/internal/handler/project_resource.go` | no | medium | upstream/main project_resource.go:480 already accepts http/https/ssh/git URLs (present at v0.7.0); no project_admission.go or ReasonProjectInactive block upstream. |
| C48 | fork-only | - | no | high | No web_url or weblink package in upstream server/CLI (git grep web_url in server finds only MULTICA_APP_URL handling in cmd_auth.go). |
| C49 | fork-only | - | no | high | No web push, VAPID or push outbox upstream (git grep webpush/vapid empty). |
| C50 | fork-only | - | no | high | No decision_action_proposal.go or decision_proposal_guard.go upstream (ls-tree upstream/main 7397565). |
| C51 | fork-only | - | no | high | server/internal/decisionaction absent upstream/main; no config-change handler or card preview upstream. |
| C52 | fork-only | - | no | high | Action files absent upstream/main (decisionaction package missing). |
| C53 | fork-only | - | no | high | human_in_charge.go and cmd_issue_human_in_charge.go absent upstream/main. |
| C54 | fork-only | - | no | high | No decision_wait_*.go or decision_wake_receipt.go upstream/main. |
| C55 | fork-only | - | no | high | agent_decision*.go and agent-decisions-row.tsx absent upstream/main. |
| C56 | fork-only | - | no | high | decision_ask_mcp.go and decision_ask_api.go absent upstream/main. |
| C57 | fork-only | - | no | high | server/internal/system1 absent upstream/main. |
| C58 | fork-only | - | no | high | No server/internal/passkey upstream/main; no passkey code in packages. |
| C59 | fork-only | - | no | high | passkeys-tab.tsx and passkey_list.go absent upstream/main. |
| C60 | fork-only | - | no | medium | Upstream has server/cmd/maintenance/main.go as a generic job driver; no passkey-recovery path or session_generation.go handler upstream. |
| C61 | fork-only | - | no | high | chat_attention.go and chat_outcome.go absent upstream/main (service and handler). |
| C62 | fork-only | - | no | high | No needs-you-list.tsx or chat attention commands upstream/main. |
| C63 | fork-only | - | no | medium | Upstream mobile chat-drafts-store.ts says v1 does not persist; no packages/core/drafts or draft-persistence.ts upstream/main. |
| C64 | fork-only | - | no | medium | recent-agents.ts absent upstream/main; new-chat-button.tsx exists there with no Recent section; upstream recent-context-store.ts is a different feature. |
| C65 | fork-only | - | no | high | status_history.go, issue_status_history.go and status-history-button.tsx absent upstream/main. |
| C66 | fork-only | - | no | high | issue_actual_timeline.go absent upstream/main (service and handler). |
| C67 | fork-only | - | no | high | graph-core.ts, swimlane-layout.ts and canvas/ absent upstream/main. |
| C68 | fork-only | - | no | medium | search_fuzzy.go and search_fold.go absent upstream/main; absence search only, no commit-level check of upstream search changes. |
| C69 | fork-only | - | no | medium | cmd_chat_unseen.go absent upstream/main; chat.go is shared and exists upstream. |
| C70 | fork-only | - | no | medium | chat_task_authority.go and chat_read_access.go absent upstream/main; chat_history.go is shared. |
| C71 | fork-only | - | no | high | upstream/main has no dm_progress file or migration (0 matches in git ls-tree). |
| C72 | fork-only | - | no | high | dm_progress.go, cmd_issue_follow.go and dm_progress.sql absent from upstream main 7397565 |
| C73 | overlaps | 4fac8d772f (commentBlockedTargetOutcomes preview, upstream comment.go:2282) | no | medium | upstream previews blocked targets without rejecting; gate and CLI files absent upstream; symbol unchanged since v0.7.0 |
| C74 | fork-only | - | no | high | decision_wake_receipt.go, decision_worker.go and the 943-945 tables absent from upstream main |
| C75 | fork-only | - | no | high | askguard package and ask_guard*.go absent from upstream main |
| C76 | fork-only | - | no | high | system1*.go, ask_waiver.go and the 9995x migrations absent from upstream main |
| C77 | fork-only | - | no | high | issue_effective_due.go absent from upstream main; no effective_due symbol in upstream server code |
| H01 | overlaps | caeb146ba | no | medium | Upstream GitHub App integration (v0.7.0) has multi-install support and an installation picker, but github.go has no verifyGitHubInstallationOwnership. |
| H02 | overlaps | 0da0b46bb; MUL-7672 | no | high | Upstream has closingIdentifierRe/extractClosingIdentifiers but no code-span stripping, and maybeAutoCompleteIssue has no open-child check. |
| H03 | overlaps | fc4a61570; MUL-7726 | yes | high | Upstream auto-complete resolves custom statuses via resolver.Effective; fork checks literal Done/Cancelled in pr_auto_complete.go so custom done-category states still advance to merged. |
| H04 | overlaps | 0da0b46bb; MUL-7672 | no | medium | Upstream has HTTP DELETE /pull-requests/{prId} (router.go:2166) and exclusion, but cmd_issue.go only lists PRs (no unlink verb). New e7592c57ed "feat(cli): control per-issue PR auto-complete (#10057)" adds --pr-auto-complete on/off and PUT /pr-auto-complete (router.go:2167); per-issue toggle, not unlink or close-link repair. |
| H05 | overlaps | e720fe221; MUL-8135; ecce58986; MUL-5265; 82492f94d6; MUL-8510 | no | medium | Upstream ghsnapshot/refresh.go rechecks decided PRs only when a conflict-wakeup rule watches them (defaultWatchTTL 5m, :45); no hourly sweep. snapshot.go requests check-runs?filter=latest (:124) and, since 82492f94d6, drops cancelled workflow runs replaced by a newer run (fetchSupersededSuites :224) but only with the optional Actions read permission; no per-(name, app) started_at dedupe as in the fork. |
| H06 | fork-only | - | no | high | Upstream has no pr-close-target workflow or script. |
| H07 | fork-only | - | no | high | Upstream has no github_merge_announcement table or merge-announcement comment code. |
| H08 | overlaps | da278c7d4; MUL-8154 | no | medium | Upstream already has POST /api/cli-token with RequireHumanActor, but its login-page.tsx keeps a localStorage-token shortcut. |
| H09 | fork-only | - | no | high | Upstream has no pull_request_review handling, github_review.go or ghsnapshot/reviews.go. |
| H10 | fork-only | - | no | high | Upstream has only the autopilot webhook_delivery, no GitHub signed-delivery store, intake worker, route grants or dispatch halt. |
| H11 | fork-only | - | no | high | Upstream has no pr_event.go, pr_lifecycle.go, cancel_ack.go or task_pr_event.go. |
| H12 | fork-only | - | no | high | github_allowlist*.go and migration 921 absent from upstream main |
| H13 | fork-only | - | no | high | github_dispatch_halt.go and migration 892 absent from upstream main |
| H14 | fork-only | - | no | high | upstream/main has no github_allowlist.go; its handler allowlist hits are the unrelated Composio toolkit allowlist. |
| W01 | overlaps | da1671ca2 / MUL-7420; 0beaeddc0 / MUL-8120; 354243b459 / MUL-8386 | yes | high | upstream/main has server/cmd/multica/cmd_workflow.go with project workflow get/edit verbs on the same workflows API as the fork. |
| W02 | overlaps | da1671ca2 / MUL-7420 | no | medium | Upstream has packages/views/workflows/issue-flow-card.tsx as a read-only card; no issue_workflow_override.go and no --workflow flag in upstream cmd_issue.go (git grep upstream/main). |
| W03 | overlaps | da1671ca2 / MUL-7420 | no | medium | Upstream issue_workflow_entry.go / cmd_issue.go share the status-change workflow handoff; the --no-start preservation is not in upstream. |
| W04 | fork-only | e720fe221 / MUL-8135 | no | medium | upstream/main issue_wakeup_system.go defines only SystemRuleChildDone (no pr_checks_passed) and has no advanceIssueOnCIGreen; its --until-pr condition is a user-created wait, a different mechanism. Old overlap claim not confirmed by code. |
| W05 | fork-only | e720fe221 / MUL-8135 | no | medium | git grep upstream/main finds no pr_merged/pr_checks_*/PRMerged system rules in server/internal; SystemRuleChildDone is the only upstream system rule. Upstream --until-pr (MUL-8135) is a user-created wait condition, not an automatic merge/failure wakeup. |
| W06 | fork-only | 5ed09342a / MUL-7680 | no | medium | Upstream wakeup v2 has per-issue wakeups but no wakeup_definition scope cascade, loadBuiltinWakeup or definition CRUD/preview handlers (git ls-tree upstream/main). |
| W07 | fork-only | - | no | high | No default/local capacity pool code upstream; migration 583_wakeup_default_capacity has no upstream counterpart. |
| W08 | fork-only | - | no | high | No wakeup_aggregate.go or aggregate-cap admission upstream (git ls-tree upstream/main). |
| W09 | fork-only | - | no | medium | No wakeup_active_run.go / deferForActiveRun upstream; upstream wakeup v2 fires without an active-run defer mode. |
| W10 | overlaps | 5ed09342a / MUL-7680 | no | medium | upstream issue_wakeup_system.go:645 pauses a child_done wakeup with wakeupPausedRate; upstream has no resumeRateLimited / ResumeRateLimitedSystemWakeup recovery (no 'resume' in that file). |
| W11 | fork-only | - | no | high | No workspace_wakeups_pause handler, WorkspaceWakeupsPausedShared query or wakeups-paused-banner upstream. |
| W12 | overlaps | 5ed09342a / MUL-7680 | no | medium | upstream has server/internal/service/issue_wakeup_activity.go (recordWakeupActivity) and packages/views/issues/components/wakeup-activity.tsx; fork adds identification fields. Upstream touched these after v0.7.0 only for UI/copy (f0c361ebca, 25086ae73b). |
| W13 | overlaps | 16f0d307dd (#9702); MUL-4525 | no | medium | upstream handler/comment.go has DispatchStatus queued/coalesced/deferred/blocked (line 104) and coalesced_comment_ids; coalescing existed at v0.7.0 (20 'coalesced' hits). comment_self_mention.go is not in upstream. |
| W14 | fork-only | - | no | high | No squad_leader_failure_marker migration or idle-leader failure marker in upstream task.go. |
| W15 | fork-only | - | no | high | Upstream issueworkflow has no edges.go / guards (ParseGuard, FirstMatch) or graph-edit editor; git ls-tree upstream/main shows no such files. |
| W16 | fork-only | - | no | high | No issue_workflow_subworkflow.go or sub-workflow step type upstream. |
| W17 | fork-only | - | no | high | No webhookguard package, workflow webhook endpoint or nonce sweeper upstream. |
| W18 | fork-only | - | no | high | No issue_requested_by handler or migration 689 upstream; handoff-form handler-by-role UI absent. |
| W19 | fork-only | - | no | high | No entry.go / entry-gate code upstream (git ls-tree upstream/main). |
| W20 | fork-only | e720fe221 / MUL-8135 | no | high | Upstream --until-pr conditions exist (issue_wakeup_condition.go) but upstream has no integrations/ghrun package, workflow run/job tables or --until-gh-run flag. |
| W21 | overlaps | 5ed09342a / MUL-7680 | no | medium | Upstream wakeup v2 has at/every/cron per-issue wakeups and scoped events in issue_wakeup.go; fork adds outbox, custom event/condition rules and instances. |
| W22 | fork-only | - | no | high | No daemon_pause handler or scheduling-pause columns upstream. |
| W23 | fork-only | - | no | medium | listed UI files and wakeup_activation.go absent from upstream main; upstream wakeups-tab.tsx exists but carries no definitions editor (absence search, not a full read) |
| W24 | fork-only | - | no | high | wakeup_definition*.go and migration 946 absent from upstream main |
| W25 | fork-only | - | no | high | edges.go, review_loop_guard.go, checks_enrich.go, issue_workflow_edge_tx.go and task_handoff_tx.go absent from upstream main |
| W26 | fork-only | - | no | high | workflowlint package and issue_workflow_lint.go absent from upstream main |
| W27 | fork-only | - | no | medium | no start_status_key column in upstream issue_workflow.sql; migration 891 absent (absence search) |
| W28 | fork-only | - | no | high | wakeup_skip_unchanged.go absent from upstream main |
| W29 | fork-only | - | no | high | agent_decision_followup.go and migrations 932/933 absent from upstream main |
| W30 | fork-only | - | no | high | stagequeue package, stage_queue.go and migration 935 absent from upstream main |
| W31 | overlaps | da1671ca2 / MUL-7420 | no | medium | Upstream project workflows exist; batch picker, workflow pill and mobile override not found upstream (absence search, no filter or pill match in upstream/main). |
| W32 | fork-only | - | no | high | upstream/main has server/internal/issueworkflow but no server/internal/workflowlint package. |
| W33 | fork-only | - | no | high | upstream/main has no server/internal/ghbudget, no ci_watch migration and no ghsnapshot/stacks.go or ratelimit.go; the ghsnapshot package itself exists upstream (client.go, refresh.go). |
| G01 | fork-only | - | no | high | No governed_instruction_* handlers upstream. |
| G02 | fork-only | - | no | high | No owner CAS digest columns/verbs upstream. |
| G03 | fork-only | - | no | medium | Upstream cmd_squad.go has no byte-exact file/stdin resolveSquadInstructions. |
| G04 | fork-only | - | no | high | No vote-receipt governed write upstream. |
| G05 | fork-only | - | no | high | No required_source_link.go upstream. |
| G06 | fork-only | - | no | high | No protocollint package upstream. |
| G07 | fork-only | - | no | high | No server/internal/checkpoint package upstream. |
| G08 | overlaps | MUL-4525 | no | medium | Upstream v0.7.0 and main already have ErrDuplicatePendingTask in service/task.go and handling in handler/comment.go. |
| G09 | fork-only | - | no | high | git grep -i ProviderHold upstream/main -- server returns nothing. |
| G10 | fork-only | - | no | high | Upstream has no Jev/governance packages; all listed sources are deleted at HEAD. |
| G11 | fork-only | - | no | high | Upstream has no governance receipt code. |
| G12 | fork-only | - | no | high | Upstream has no governance/Jev code (no server/internal/governance, no pkg/jev, no governance_workspace_config table). |
| G13 | fork-only | - | no | high | Upstream has no governance/Jev code (no server/internal/governance, no pkg/jev, no governance_workspace_config table). |
| G14 | fork-only | - | no | high | Upstream has no governance/Jev code (no server/internal/governance, no pkg/jev, no governance_workspace_config table). |
| G15 | fork-only | - | no | high | Upstream has no governance/Jev code (no server/internal/governance, no pkg/jev, no governance_workspace_config table). |
| G16 | fork-only | - | no | high | Upstream has no workspace_export_privacy handler or export-privacy settings tab. |
| G17 | fork-only | - | no | high | Upstream has no governance/Jev code (no server/internal/governance, no pkg/jev, no governance_workspace_config table). |
| G18 | fork-only | - | no | high | Upstream has no governance/Jev code (no server/internal/governance, no pkg/jev, no governance_workspace_config table). |
| G19 | fork-only | - | no | high | Upstream has no governance/Jev code (no server/internal/governance, no pkg/jev, no governance_workspace_config table). |
| G20 | fork-only | - | no | high | Upstream has no governance/Jev code (no server/internal/governance, no pkg/jev, no governance_workspace_config table). |
| G21 | fork-only | - | no | high | Upstream has no daemon maintenance command, agent env patch handler or maintenance.go. |
| G22 | fork-only | - | no | high | Upstream has no pkg/credentialexec boundary. |
| G23 | fork-only | - | no | medium | Upstream has no squad_briefing handler (git grep squad_briefing at the pin is empty). runtime_config_sections.go does exist upstream (also at the old pin) but carries different child-completion text (see O17). |
| G24 | fork-only | - | no | medium | Upstream has no decisiontx package or governed CAS CLI path. |
| G25 | fork-only | - | no | high | Removal record row; documents deleted fork-only Jev/governance code, nothing to compare upstream. |
| G26 | fork-only | - | no | high | decisionaction package and decision_action_*.go absent from upstream main; the locking change edits upstream skill.go but adds no upstream feature |
| G27 | fork-only | - | no | high | governed_instruction_receipt.go and migration 925 absent from upstream main |
| G28 | fork-only | - | no | high | audit.go, workspace_audit_event.sql and migration 900 absent from upstream main |
| G29 | fork-only | - | no | high | task_token_write_scope*.go absent from upstream main |
| G30 | fork-only | - | no | high | configsync package, config_sync.go and migration 897 absent from upstream main |
| G31 | fork-only | - | no | high | routing_agent.go and workspace_routing_agent.go absent from upstream main |
| D01 | overlaps | 191c782d60; MUL-7988 | no | medium | upstream/main server/cmd/multica/cmd_daemon_profile_lock.go acquireDaemonProfileLock (flock, daemon.lock) from 191c782d60 MUL-7988; fork also has daemon/instance_lock.go (acquireDaemonInstanceLock, WaitForInstanceRelease) not upstream. |
| D02 | overlaps | fcd13aece9; MUL-2100 | no | medium | upstream/main daemon/auto_update.go (fcd13aece9 MUL-2100, in v0.7.0) auto-updates the CLI when idle; no handler/daemon_update.go or scripts/build-daemon-binaries.sh upstream. |
| D03 | fork-only | - | no | medium | Upstream hermes_home.go already strips the Hermes memory provider (disableHermesMemoryProvider :805, call :694) and hermes_memory.go has mountHermesMemories, but there is no per-agent opt-in to keep external memory (fork HermesExternalMemoryEnv=inherit not found upstream). Hermes commit 50aea8fae5 "MUL-8548: fix(hermes): preserve dependency environments across turns (#9866)" does not touch memory. |
| D04 | fork-only | - | no | medium | Upstream codex.go has only catalog refresh retry backoff (codexCatalogRetryBackoff, line 119); no state-runtime SQLite-init retry, codex_model_catalog.go or codex_resume_multi_agent.go upstream (ls-tree). Upstream codex_multi_agent.go config is a different mechanism. |
| D05 | fork-only | - | no | high | No antigravity_mcp.go / poisoned-executor retirement or taskfailure.AGYReplayedNotFound upstream (git grep upstream/main). |
| D06 | overlaps | 53c0088f1; ebc7c4696; MUL-7847 | no | medium | Upstream has provider-failure classification/retry (old refs); no RecordProviderFailureRecovery reservation, ReasonDispatchBlocked or retry-pending coordination in upstream service/task.go. |
| D07 | fork-only | - | no | high | No daemon_leader_session.go or oversized-leader-session fresh start upstream. |
| D08 | fork-only | - | no | high | No duplicate_delivery.go or repeat-delivery guard in upstream daemon. |
| D09 | fork-only | - | no | medium | Upstream has no claimed_model column or ClaimedModel field (git grep upstream/main -- server empty). Old ref 482ec69c1 (MUL-7969) is claim-decision metrics in task_claim_decision.go, a different feature. |
| D10 | fork-only | - | no | high | No cmd_agent_runtime_skills.go / runtime-target setting verb upstream. |
| D11 | upstreamed | ba30324e10 | yes | medium | Upstream v0.7.0 metrics/pricing.go and packages/core/runtimes/pricing.ts already price GPT-6 Sol/Luna Hermes/Codex models (ba30324e10). |
| D12 | fork-only | - | no | high | No bounded inbox-acceptance wait in upstream integrations/wecom/relay_outbound.go. |
| D13 | fork-only | - | no | high | No daemon_stale_session.go / idle-past-cache-TTL resume skip upstream. |
| D14 | fork-only | - | no | high | No provider_failure_guard.go or reservation schema upstream. |
| D15 | fork-only | - | no | high | No silence_watchdog.go / silence-alert endpoint upstream. |
| D16 | overlaps | - | no | medium | Upstream cmd_runtime.go has a version-based `runtime update --target-version` flow; no update_freeze.go or commit-based runtime update status. |
| D17 | fork-only | - | no | high | No hostsample.go or ops snapshot sampler upstream. |
| D18 | fork-only | - | no | high | No recovery_claim.go upstream. |
| D19 | fork-only | - | no | high | No idle_started_guard files upstream. |
| D20 | fork-only | - | no | high | No agentpreset package or preset API upstream. |
| D21 | fork-only | - | no | high | No server/pkg/redact shared rules upstream. |
| D22 | fork-only | - | no | medium | Upstream has scripts/install.sh, scripts/install.ps1 and server/internal/cli/update.go but no progress reporting in them, and no server/internal/cli/progress.go, server/internal/daemon/update_progress.go or runtime_update.go (git grep and ls-tree upstream/main). |
| D23 | fork-only | - | no | high | upstream/main has no provider_breaker files in server/internal/service or server/internal/handler and no provider-breaker migration. |
| D24 | fork-only | - | no | high | upstream/main has no harness_profile.go in service, handler or daemon/execenv, and no managedHarnessProfile. |
| D25 | fork-only | - | no | high | upstream/main has no server/internal/daemon/initctx package. |
| D26 | fork-only | - | no | high | bgagents package, background_task.go and migration 947 absent from upstream main |
| B01 | fork-only | - | no | high | upstream/main has no run_budget_* service/handler files and no run-budget migration; only unrelated image/media budget tests. |
| B02 | fork-only | - | no | high | upstream/main has no run_budget_snapshot/raise/notice files or CLI 'issue budget' verb. |
| B03 | fork-only | - | no | high | upstream/main has no budget recovery, top-up or server-origin decision code (no dispatch/recovery.go budget caller marker, no run_budget_decision.go). |
| B04 | fork-only | - | no | high | upstream/main has no server/internal/decisionaction package, run_budget_refusal.go or issue.run_budget action types. |
| B05 | fork-only | - | no | high | upstream/main has no issue_run_budget_request.go, cmd_issue_budget.go or cmd_decision_propose.go. |
| S01 | overlaps | - | no | medium | Upstream has IsolatedGitMetadata (repocache/cache.go:752-758, unchanged since v0.7.0) but no daemon/sandbox, restricted_home.go, run_capability_grant, bwrap or netns. |
| S02 | fork-only | - | no | high | No egress package, bridge or netns code upstream. |
| S03 | fork-only | - | no | high | No broker, readproxy or elevated checkout upstream. |
| S04 | fork-only | - | no | high | Upstream has no taint code (no issue_taint queries, no taint tables). |
| S05 | fork-only | - | no | high | upstream/main has no server/internal/daemon/broker package and no `multica git` push/pr verbs (git ls-tree upstream/main); nothing comparable. |
| S06 | fork-only | - | no | high | upstream/main has no server/internal/daemon/broker package (git ls-tree upstream/main). |
| S07 | fork-only | - | no | high | upstream/main has no server/internal/daemon/broker package, so no read-token mint. |
| S08 | fork-only | - | no | high | upstream/main has no server/internal/daemon/readproxy package. |
| S09 | fork-only | - | no | high | upstream/main has no elevated_read_filter.go in server/internal/handler. |
| S10 | fork-only | - | no | high | upstream/main claude.go passes --disallowedTools AskUserQuestion only (no WebSearch or WebFetch); upstream has no daemon/gatesuite or sandbox/gateprobe package. |
| O01 | fork-only | - | no | high | Upstream CI has no turbo concurrency cap and test-go.sh has no -p 1 / shared-Postgres serialization. |
| O02 | fork-only | - | no | high | Upstream apps/web/next.config.ts lacks experimental.cpus, webpackMemoryOptimizations, webpackBuildWorker and the memory cache. |
| O03 | fork-only | - | no | high | Upstream has no cd-*.yml workflows and no deploy/cd; only deploy/helm. |
| O04 | fork-only | - | no | high | Upstream has no deploy/cd directory and no cd-*.yml workflows. No quiescence.mjs upstream. |
| O05 | fork-only | - | no | high | Upstream has no deploy/cd directory and no cd-*.yml workflows. No docker-compose.ab.yml or router.sh upstream. |
| O06 | fork-only | - | no | high | Upstream has no deploy/cd directory and no cd-*.yml workflows. |
| O07 | fork-only | - | no | high | Upstream has no deploy/cd directory and no cd-*.yml workflows. |
| O08 | fork-only | - | no | high | deploy/cd/release-packet.mjs:86-109 KNOWN_LEDGER_RENAMES maps fork migrations 614/622/630/653; no such file upstream. |
| O09 | fork-only | - | no | high | Upstream has no deploy/cd directory and no cd-*.yml workflows. |
| O10 | fork-only | b90696903d | yes | high | server/internal/testutil/nodb.go ExitWithoutDatabase fails when CI=true/1 and skips locally (used at integration_test.go:54,58, handler_test.go:58,62, commentguard/lock_test.go:33,37). Upstream TestMain always os.Exit(0) on no DB. Upstream gained testutil/isolated_db.go (b90696903d), unrelated. |
| O11 | fork-only | 7c186016a5; fb49f55924 | yes | medium | description-disclosure.tsx is fork-only; e2e/issue-content-disclosure.spec.ts:167 is the fork test. Upstream rewrote quick-actions.spec.ts, local-search.spec.ts and search-command.test.tsx in 7c186016a5/fb49f55924, so the fork's selector fixes will conflict on sync. |
| O12 | upstreamed | ca593b3103; 3bacc41f84; ce5a2ba472 | yes | high | v0.7.0 go.mod already had go 1.26.9 and x/net v0.60.0; upstream main has x/net v0.62.0 and CI toolchain 1.27.3 (ce5a2ba472, #9420). Fork HEAD also has x/net v0.62.0. |
| O13 | fork-only | - | no | high | scripts/paired-replay.mjs imports only node builtins; no equivalent upstream script. |
| O14 | fork-only | - | no | high | Upstream only has server_version (server/internal/config/config.go:103-108); no server_build/server_commit field and no server/pkg/buildinfo. Fork adds config.go:105-110,149-154, buildinfo/version.go and release-stamp.sh (used by cd-qualification.yml:173,200 and release.yml:86). |
| O15 | fork-only | - | no | high | Upstream changelog page is static; PULL_REQUEST_TEMPLATE.md only has 'Closes #'. No fork-changelog generation upstream. |
| O16 | fork-only | - | no | medium | provider_failure_guard.go does not exist upstream; upstream has only task.go cooldown for a blip (different). Queries FindLatestScheduledAutopilotTaskForTrigger and FindLatestRecurringWakeupTask are fork-only. |
| O17 | overlaps | 90a88519df; a008d893dd | yes | medium | Upstream has the multica-platform builtin skill and issues.md and runtime_config_sections.go; the fork's 'A sub-issue closes itself' status rule and issues.md child-completion text are not upstream (upstream runtime_config_sections.go:642 says delivering an issue assigned to you always lands in_review; done stays human). 939f1a8ece "MUL-8441: default to one issue, one PR; point sub-issue runs at their parent" adds brief text (:688) but leaves this conflict unchanged. |
| O18 | fork-only | - | no | high | Doc-only .planning/che-332-daemon-sandbox/D1-FEASIBILITY.md; not present upstream, no upstream change since v0.7.0. |
| O19 | fork-only | - | no | high | Fork scripts/dev-env.sh adds --gc (gc_requested) so startup only reports stale environments; upstream runs cmd_gc --auto on startup. File differs by 19 insertions/5 deletions against upstream/main. |
| O20 | fork-only | - | no | high | merge_group in ci.yml:8,151,167,172,173,780,827 and scripts/ci-scope.mjs:20-36 (hasGreenMergeGroup, merge_group_green); upstream ci-scope.mjs only handles push/pull_request. |
| O21 | fork-only | - | no | high | Upstream has no deploy/cd directory and no cd-*.yml workflows. |
| O22 | fork-only | - | no | high | ci.yml:48-67 migration-prefix-check job and scripts/migration-prefix-check.mjs; no upstream equivalent. |
| O23 | fork-only | - | no | high | No fork-sync-guard script or workflow upstream. |
| O24 | fork-only | - | no | high | Upstream only has the 'Closes #' PR template line; no security-gate or pr-guards. |
| O25 | fork-only | - | no | high | warning-budget job (ci.yml:19-45) and scripts/warning-budget.mjs have no upstream equivalent. |
| O26 | fork-only | 6af2268bd9 | no | medium | Upstream has playwright.perf.config.ts, a typing perf scenario (6af2268bd9, MUL-7227) and ui-performance.yml but no page-load baseline; fork adds desktop/phone projects and page-load-report. |
| O27 | overlaps | f0c361ebca; 7c186016a5; 354243b459; bb3747186e | yes | medium | Upstream has the multica-platform skill (8 references) and a static changelog page with hardcoded CHANGELOG_URL in help-links.ts; no FORK-FEATURES.md ledger. |
| O28 | fork-only | - | no | high | upstream/main .github has no secret-scan, security-weekly or CODEOWNERS file and no gitleaks entry. |
| O29 | fork-only | - | no | medium | upstream/main has ci.yml with its own actions/cache steps but no cache-maintenance.yml, main-build.yml, pr-guards.yml or RUNNER-ROUTING.md. |
| O30 | fork-only | - | no | high | upstream/main has no mirror-upstream-main workflow, upstream-pr-watch script or deploy/upstream-watch. |
| O31 | fork-only | - | no | high | upstream/main workflows are ci, desktop-smoke, mobile-verify, openclaw-config-smoke, release and ui-performance; no queue-watch or dequeue-report workflow. |
| O32 | fork-only | - | no | high | upstream/main has no cd-freshness-watch, cd-live-revision or cd-evidence workflow and no deploy/cd directory content for them. |
| O33 | fork-only | - | no | medium | Upstream has e2e/perf and playwright.perf.config.ts (shared); chip-stability, scroll-stability and scroll-anchor files absent upstream/main. |
| O34 | fork-only | - | no | high | pr-main-sync and tier2-merge-card scripts absent from upstream main |
| O35 | fork-only | - | no | high | brief-replay and ux-explorer-negative-authz scripts absent from upstream main |
| O36 | fork-only | - | no | medium | Dockerfile.docs, docs-image.yml and docs-url.ts absent from upstream main; upstream has apps/docs content but no instance /docs serving (absence search) |
| X01 | fork-only | - | no | high | Design/docs-only files (design/dm-decision-quote-cards); path absent on upstream/main. |
| X02 | fork-only | - | no | high | Design/docs-only files (design/dm-stats-archive-menu); path absent on upstream/main. |
| X03 | fork-only | - | no | high | Design/docs-only files (docs/designs/dm-search-notifications-push.md); path absent on upstream/main. |
| X04 | fork-only | - | no | high | Design/docs-only files (design/bare-key-chip-stability); path absent on upstream/main. |
| X05 | fork-only | - | no | high | Design/docs-only files (design/dm-wakeup-chip); path absent on upstream/main. |
| X06 | fork-only | - | no | high | Design/docs-only files (design/run-mode-chip); path absent on upstream/main. |
| X07 | fork-only | - | no | high | Design/docs-only files (design/dm-progress); path absent on upstream/main. |
| X08 | fork-only | - | no | high | Design/docs-only files (design/issue-sidebar-compact); path absent on upstream/main. |
| X09 | fork-only | - | no | high | Design/docs-only files (design/timeline-views); path absent on upstream/main. |
| X10 | fork-only | - | no | high | Design/docs-only files (design/decision-card-fold); path absent on upstream/main. |
| X11 | fork-only | - | no | high | Design/docs-only files (design/workflow-lint-ui); path absent on upstream/main. |
| X12 | fork-only | - | no | high | Design/docs-only files (design/delegated-governed-writes); path absent on upstream/main. |
| X13 | fork-only | - | no | high | Design/docs-only files (docs/designs); path absent on upstream/main. |
| X14 | fork-only | - | no | high | Design/docs-only files (docs/designs); path absent on upstream/main. |
| X15 | fork-only | - | no | high | plan files exist only under fork .plans/ |
| X16 | fork-only | - | no | high | design/dm-attention/approved absent from upstream main |
