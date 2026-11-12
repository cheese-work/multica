# Fork feature dependencies

Companion to [FORK-FEATURES.md](FORK-FEATURES.md) (CHE-4789). Evidence for each `Depends on` value.
Static read of fork `main` `e8f066f3eaba3a2f3ea2f832b2a962e4356e310a` (2026-11-09 08:44 ICT). No runtime qualification.

## Method

- An edge `A → B` means row A needs row B. It is listed only when code of A imports, calls, queries, routes to or invokes code, tables or verbs that row B owns.
- **hard**: A does not build or run without B. **soft**: A runs without B with reduced behavior.
- The pointer is the first `path:line` where A uses B. The text is the agent-recorded reading of that line.
- Row ownership comes from each row's `Source:` list. Where no row owns a package, the edge is not listed and the note says so.
- A row that depends on a removed row is flagged; its code now has no provider on `main`.

## Totals

| Item | Count |
| --- | --- |
| Rows | 248 |
| Rows with `Depends on: none` | 131 |
| Hard edges | 168 |
| Soft edges | 20 |

## Cycles

Rows in a cycle are ordered as one unit in [UPSTREAM-READINESS.md](UPSTREAM-READINESS.md); none can land before the others unless a hook is split out.

- B01 ↔ B02 ↔ C05
- B03 ↔ C08 ↔ C10 ↔ C25 ↔ C26 ↔ C28 ↔ C29
- C11 ↔ C34
- G04 ↔ G24
- H09 ↔ H10
- O06 ↔ O21
- O15 ↔ O27
- S01 ↔ S02 ↔ S04
- W06 ↔ W08 ↔ W09
- W15 ↔ W16

## Edges

| Row | Needs | Kind | Evidence |
| --- | --- | --- | --- |
| C05 | B01 | hard | server/internal/handler/actor_guards.go:86 uses errRecoveryCaused defined in issue_run_budget.go:411 |
| C07 | C06 | hard | server/internal/handler/decision_disclaim.go:65 uses decisionIntentRefusal defined in decision_intent.go:17 |
| C07 | C08 | hard | server/internal/handler/decision_disclaim.go:131 calls handler.DecisionWorker.Notify() |
| C07 | C25 | hard | server/internal/handler/decision_record_api.go:46 calls createAskedDecisionRecord defined in decision_record.go |
| C08 | B03 | hard | server/internal/handler/decision_delivery.go:374 calls isServerDecision defined in run_budget_decision.go:27 |
| C08 | C06 | hard | server/internal/handler/decision_delivery.go:286 uses decisionIntentRefusal (decision_intent.go:17) |
| C08 | C26 | hard | server/internal/handler/decision_worker.go:197 calls deliverDecisionVetoNotice defined in decision_veto_notice.go:18 |
| C08 | C28 | hard | server/cmd/multica/cmd_decision.go:228 calls runDecisionListOpenAsks defined in cmd_decision_open_asks.go:35 |
| C08 | C29 | hard | server/internal/handler/decision_worker.go:219 calls completeDecisionActionPostCommit defined in decision_action_approve.go:319 |
| C08 | C54 | soft | server/internal/handler/decision_delivery.go:83 calls GetBindingDecisionWait, reading decision_wait episodes (reverse edge) |
| C09 | C05 | hard | server/internal/handler/decision_request_api.go:238 calls isMachineCredentialActor (actor_guards.go:60) |
| C09 | C10 | hard | server/internal/handler/decision_request_api.go:111 calls createDecisionRequest defined in decision_request.go:69 |
| C09 | C24 | hard | server/internal/handler/decision_request_api.go:49 calls decisiontx.DecodeAsk (decisiontx/ask.go) |
| C09 | C28 | hard | server/cmd/multica/cmd_decision.go:228 calls runDecisionListOpenAsks (cmd_decision_open_asks.go:35) |
| C09 | C29 | hard | server/internal/handler/decision_request_api.go:98 uses decisionActionDraft from decision_action_request.go |
| C10 | C06 | hard | server/internal/handler/decision_request.go:586 calls authorizeDecisionTask defined in decision_intent.go:43 |
| C10 | C08 | hard | server/internal/handler/decision_request.go:393 calls handler.DecisionWorker.Notify() |
| C10 | C29 | hard | server/internal/handler/decision_request.go:488 calls createDecisionAction (decision_action_request.go) |
| C11 | C34 | hard | server/internal/handler/chat_continue.go:76 calls compactChatInPlace defined in chat_compact.go:86 |
| C12 | C34 | soft | packages/views/chat/components/chat-session-header.tsx:85 'Compact now' item hidden when onCompact omitted; compaction itself is chat_compact.go in C34 |
| C25 | C06 | hard | server/internal/handler/decision_record.go:74 decisionIntentRefusal (decision_intent.go:17) and :239 authorizeDecisionTask (decision_intent.go:43) |
| C25 | C08 | hard | server/internal/handler/decision_record.go:438 calls handler.DecisionWorker.Notify() |
| C25 | C24 | hard | server/internal/handler/chat_decision_block.go:136 uses decisiontx.AskOption defined in decisiontx/ask.go:15 |
| C26 | C06 | hard | server/internal/handler/decision_card_answer.go:94 uses decisionIntentRefusal (decision_intent.go) |
| C26 | C08 | hard | server/internal/handler/decision_card_answer.go:210 DecisionWorker (decision_worker.go) |
| C26 | C19 | hard | packages/views/chat/components/needs-you-list.tsx:10 imports formatMessageDateTime from ./message-time |
| C26 | C24 | hard | server/internal/handler/decision_card_answer.go:221 decisiontx.AskOption; packages/views/issues/components/issue-decision-banner.tsx:10 imports decision-request-card |
| C26 | C25 | hard | server/internal/handler/decision_card_answer.go:76 DecisionCommentFields, :79 decisionAnswerState (decision_cards.go) |
| C26 | G24 | hard | server/internal/handler/decision_card_answer.go:242 answerIsApproval (decision_bound_write.go) |
| C27 | C06 | hard | server/internal/handler/decision_request_artifacts.go:27 uses decisionIntentRefusal (decision_intent.go) |
| C27 | C24 | hard | server/internal/decisiontx/ask.go:37 Artifacts field, :110 decodeArtifacts, :262 validateArtifacts; artifacts.go:84 method on Ask |
| C28 | B03 | hard | server/internal/handler/decision_authority.go:34 isServerDecision (run_budget_decision.go) |
| C28 | C08 | hard | server/internal/handler/chat_decision_reply.go:147 DecisionWorker (decision_worker.go) |
| C28 | C12 | hard | server/internal/handler/chat_decision_reply.go:23 service.IsCompactCommand (service/auto_compact.go:81) |
| C28 | C61 | soft | server/internal/handler/chat_decision_reply.go:130 calls ResolveChatAttentionOnHumanSend, so a human reply settles DM attention (reverse edge) |
| C29 | B03 | hard | server/internal/handler/decision_action_approve.go:192 isServerDecision (run_budget_decision.go) |
| C29 | C06 | hard | server/internal/handler/decision_action_approve.go:74 decisionIntentRefusal (decision_intent.go) |
| C29 | C28 | hard | server/internal/handler/decision_action_approve.go:79 decisionClassNeedsOwner, :197 requireDecisionAuthorityIn (decision_authority.go) |
| C30 | G13 (removed) | hard | server/internal/middleware/session_generation.go:42 writeAuthenticationError (middleware/task_token_scope.go, absent upstream) |
| C31 | C12 | hard | server/internal/service/chat_wakeup.go:231 service.ErrChatCompacting (defined service/auto_compact.go:35) |
| C31 | C19 | hard | packages/views/chat/components/scheduled-wakeups-chip.tsx:11 and wakeup-event-row.tsx:12 import time formatters from ./message-time |
| C32 | C11 | hard | packages/views/chat/lib/predecessor-chain.ts:14 reads continued_from_session_id (migration 593_chat_continuation) |
| C32 | C20 | hard | packages/views/chat/components/predecessor-dms.tsx:21 imports useChatAnnotationScope |
| C32 | C25 | hard | packages/views/chat/components/predecessor-dms.tsx:20 imports AnnotatableAnswer |
| C34 | C11 | hard | server/internal/handler/chat_continue.go:76 calls h.compactChatInPlace; ContinueChatSession is the C11 handler |
| C34 | C12 | hard | server/internal/service/auto_compact.go:145 compares task.ChatCompactionEpoch with session.CompactionEpoch |
| C35 | C12 | hard | server/internal/handler/chat_supplement.go:148 service.ErrChatCompacting (service/auto_compact.go:35) |
| C36 | C12 | hard | apps/mobile/data/api.ts:1172 GET /api/chat/sessions/${id}/usage (router.go:2630 h.GetChatSessionUsage, chat_usage.go from #266) |
| C36 | C15 | hard | packages/views/chat/components/chat-search.tsx:165 chatSearchInfiniteOptions -> GET /api/chat/search (handler/chat_search.go) |
| C37 | C22 | soft | apps/web/app/[workspaceSlug]/opensearch.xml/route.ts:32,45 advertises the suggestions URL; layout.tsx:73 links the descriptor (C22 #219) |
| C41 | C25 | hard | server/internal/handler/comment_supersede.go:160,167 h.withDecisionFields (defined decision_cards.go:247) |
| C45 | C44 | hard | packages/views/issues/components/hierarchy/use-hierarchy.ts:8 imports issueRelationsOptions from @multica/core/issues/relations; :34 useQuery on it |
| C45 | C67 | hard | packages/views/issues/components/hierarchy/hierarchy-layout.ts:1 imports Box from ../canvas/canvas-math, the split engine file (reverse edge) |
| C48 | C11 | soft | server/internal/handler/chat_continue.go:93 returns WebURL via weblink.Build in the continue response |
| C48 | C13 | soft | server/internal/handler/chat_fork.go:254,318 returns WebURL via weblink.Build in the fork response |
| C49 | C16 | hard | server/pkg/db/queries/push.sql:119 inbox_item_visible() (migration 650_inbox_item_dm_reply, #276) and :112-116 notification_channel_preference (#272) |
| C50 | C25 | soft | packages/views/common/decision-request-card.tsx:225 renders DecisionActionApproval for a proposal on the quote card |
| C50 | C29 | hard | server/internal/handler/decision_action_proposal.go:64 calls decisionaction.Canonical to canonicalise the proposed action before it is stored |
| C51 | C29 | hard | server/internal/handler/decision_action_approve.go:156 looks up configChanges[preview.ActionType] to run a registered config change |
| C51 | G24 | soft | server/internal/handler/decision_action_config_change.go:189 calls delegatedWriteAllowed (workspace delegated-write setting, workspace_delegated_writes.go:27) |
| C52 | C51 | hard | server/internal/handler/decision_action_agent_instructions.go:24 calls registerConfigChange to register the action with the C51 core |
| C54 | C08 | hard | server/internal/handler/decision_wait_delivery.go:46 takes decisionWakeSnapshot (decision_delivery.go:22) to deliver the episode |
| C54 | C53 | hard | server/internal/handler/decision_wait_entry.go:129 calls resolveHumanInCharge to pick the waiting person |
| C55 | C09 | hard | server/internal/handler/agent_decision.go:535 inserts the agent decision into decision_request |
| C57 | C24 | hard | server/internal/handler/system1_shadow.go:26 takes the decisiontx ask package to record the shadow ask |
| C58 | C30 | hard | server/internal/handler/decision_card_answer_gate.go:12 imports server/internal/passkey and uses passkey.GateBlock (:29) |
| C59 | C30 | hard | packages/views/settings/components/passkeys-tab.tsx:8 imports the passkey schema (packages/core/api/passkey-schema.ts, C30 source) |
| C62 | C61 | hard | packages/views/chat/components/needs-you-list.tsx:46 reads attention_since from the session-list projection |
| C71 | C72 | hard | server/migrations/99955_dm_progress_capture.up.sql:17 capture function reads the dm_progress_follow table owned by C72 |
| C72 | C05 | hard | server/pkg/db/queries/dm_progress.sql:20 |
| C72 | C11 | hard | server/pkg/db/queries/dm_progress.sql:7 |
| C74 | C09 | hard | server/pkg/db/queries/decision_wake_receipt.sql:33 |
| C75 | C09 | hard | server/internal/service/issue_wakeup_ask_guard.go:30 |
| C76 | C24 | hard | server/internal/decisiontx/ask_waiver.go:75 |
| H02 | C03 | hard | server/internal/handler/pr_auto_complete.go:304 h.childStatusResolver / isTerminalChildStatus defined in handler/issue_child_done.go:704 |
| H05 | W05 | soft | server/internal/handler/github.go:1403 fires SystemRulePRAttention; recheck still refreshes without the wakeup |
| H08 | C05 | hard | server/internal/handler/auth.go:762 requires X-Actor-Source interactive_jwt, set from JWT amr claim at server/internal/middleware/auth.go:331 |
| H09 | H10 | hard | server/internal/handler/github_review.go:115 calls GitHubDispatchHalted (queries/github_dispatch_halt.sql, CHE-2220) |
| H10 | H09 | hard | server/internal/handler/github_intake_worker.go:308 calls TaskService.PrepareTaskForMention / EnqueuePreparedTaskForMentionInTx defined in service/task.go:1769 by #389 |
| H11 | D08 | hard | server/internal/daemon/duplicate_delivery.go:94 startTaskWithCurrentClaim (defined by #358, return type changed by #535) |
| H11 | W05 | soft | server/pkg/db/queries/task_pr_event.sql:204 ListPROnlyRunIDs matches wakeup receipt types written by service/issue_wakeup_system.go:163 |
| H12 | H05 | hard | server/internal/handler/github_allowlist.go:55 |
| H13 | H10 | hard | server/internal/handler/github_intake_worker.go:314 |
| H14 | H10 | soft | server/internal/handler/github_intake.go:107 asks allowedGitHubInstallations before the signed delivery is stored |
| W04 | W05 | soft | server/internal/handler/github.go:1432 advanceIssueOnCIGreen runs inside the PR-checks block that W05 also drives; falls back to server/internal/service/issue_wakeup_system.go:44 pr_checks_passed wakeup |
| W04 | W15 | hard | server/internal/handler/issue_workflow_ci_advance.go:50 issueworkflow.FirstMatch over def.EdgesOf(status); server/internal/issueworkflow/edges.go:212 FirstMatch is the fork edge engine |
| W05 | W06 | hard | server/internal/service/issue_wakeup_system.go:253 loadBuiltinWakeup(ctx,q,issue,in.Rule,legacy) reads the W06 definition cascade |
| W05 | W08 | hard | server/internal/service/issue_wakeup_system.go:1489 config.aggregateCaps() + admitAggregateStart in the system-wakeup start path |
| W05 | W09 | hard | server/internal/service/issue_wakeup_system.go:1392 config.defersActiveRun(); :1455 deferForActiveRun |
| W05 | W11 | hard | server/internal/service/issue_wakeup_system.go:1003 q.WorkspaceWakeupsPausedShared; :1038 settlePaused (issue_wakeup_pause.go:33) |
| W06 | W08 | hard | server/internal/service/wakeup_definition_write.go:226 withRootAggregateDefault; :229 checkWakeupAggregateScope validate aggregate fields in the definition model |
| W06 | W09 | hard | server/internal/service/wakeup_definition_validate.go:162 validWakeupActiveRun (wakeup_active_run.go:61) |
| W07 | W06 | hard | server/internal/service/wakeup_builtin_exec.go:50 builtinWakeup / Eff config; wakeup_default_capacity.go:29 ApplyDefaultWakeupInstance is called from custom wakeup paths (wakeup_custom_instance.go:139) and reads the resolved definition |
| W08 | W06 | hard | server/internal/service/wakeup_aggregate.go:191 func (b *builtinWakeup) aggregateCaps(); server/internal/service/wakeup_builtin_exec.go:50 builtinWakeup type is defined in W06 code |
| W09 | W06 | hard | server/internal/service/wakeup_active_run.go:66 func (b *builtinWakeup) defersActiveRun(); wakeup_builtin_exec.go:109 reads c.ActiveRun from definition |
| W10 | W06 | hard | server/internal/service/issue_wakeup_system.go:1297 resumeRateLimited(..., config.rateLimit()); wakeup_builtin_exec.go:179 rateLimit() is the W06 config accessor |
| W12 | W05 | soft | server/internal/service/issue_wakeup_activity.go:28 wakeupActivitySkipped = wakeup_skipped is emitted by system-wakeup paths; activity UI works without them |
| W15 | W16 | hard | server/internal/handler/issue_workflow_edges.go:71 h.spawnSubWorkflowChild(...) called from the edge engine (defined in issue_workflow_subworkflow.go:132) |
| W16 | W15 | hard | server/internal/handler/issue_workflow_edges.go:71 spawn happens inside the edge-advance loop; stage join reads edge Facts (server/internal/issueworkflow/edges.go:212) |
| W17 | W15 | hard | server/internal/handler/issue_workflow_edges.go:197 facts.Webhooks = h.guardedWebhooks(...); server/internal/issueworkflow/edges.go:35 GuardWebhook guard consumes verified events |
| W19 | W15 | hard | server/internal/handler/issue_workflow_entry_gate.go:84 h.loadEdgeFacts(...); :206 issueworkflow.Edge; server/internal/issueworkflow/entry.go:16 MaxEntryGatesPerStep = MaxEdgesPerStep, :63 ParseGuard |
| W20 | H07 | hard | server/internal/handler/github_workflow.go:62 `githubEnabledForWorkspace(ws)`; the helper is defined at server/internal/handler/github.go:2679 (added by fork commit cec25ddc8b, row H07) and is absent from upstream `7397565332` (upstream has the `github_enabled` setting, read in server/internal/daemon/daemon.go:3300, but no handler helper). The H07 writer was removed (149e45f53e); the 17-line helper stayed. W20 needs it bundled |
| W21 | W06 | hard | server/internal/service/wakeup_custom_exec.go:47 b := &builtinWakeup{Eff: r.eff, ...} |
| W21 | W07 | hard | server/internal/service/wakeup_custom_instance.go:139 ApplyDefaultWakeupInstance; wakeup_custom_sweep.go:312 same call |
| W21 | W08 | hard | server/internal/service/wakeup_custom_exec.go:262 admitAggregateStart(...) |
| W21 | W09 | hard | server/internal/service/wakeup_custom_exec.go:237 deferForActiveRun(...) |
| W23 | W06 | hard | packages/core/workspace/wakeup-definitions.ts:29 |
| W24 | W06 | hard | server/internal/handler/wakeup_definition.go:199 |
| W25 | H05 | soft | server/internal/integrations/ghsnapshot/checks_enrich.go:34 |
| W25 | W15 | hard | server/internal/handler/issue_workflow_edges.go:47 |
| W26 | W15 | hard | server/internal/workflowlint/workflowlint.go:38 |
| W28 | W05 | soft | server/internal/service/issue_wakeup_system.go:1496 |
| W31 | W02 | hard | packages/core/api/client.ts:1701 PUTs /api/issues/{id}/workflow, the W02 override endpoint (router.go:2210 SetIssueWorkflow) |
| W32 | W15 | hard | server/internal/workflowlint/env.go:11 imports internal/issueworkflow and lints its Definition and Step types |
| W33 | H05 | hard | server/internal/integrations/ghsnapshot/ratelimit.go:14 calls client.restGet on the ghsnapshot Client that the snapshot refresh owns |
| G02 | G01 | hard | server/internal/handler/governed_instruction_owner_cas.go:158 returns hermesExceptionSwapResult (governed_instruction_hermes_exception.go:217) |
| G02 | G03 | hard | server/cmd/multica/cmd_workspace.go:568 mirrors resolveSquadInstructions (cmd_squad.go:291); squad CLI path calls it at cmd_squad.go:274 |
| G04 | G01 | hard | server/internal/handler/governed_instruction_vote.go:395 returns hermesExceptionSwapResult (governed_instruction_hermes_exception.go:217) |
| G04 | G02 | hard | server/internal/handler/governed_instruction_vote.go:395 takes ownerCASColumn (governed_instruction_owner_cas.go:67) |
| G04 | G24 | hard | server/internal/handler/governed_instruction_vote.go:402 calls decisionApply (governed_instruction_decision.go:41) |
| G05 | G01 | hard | server/internal/handler/required_source_link.go:150 calls isGovernedFieldMachineActor (governed_instruction_fields.go:28) |
| G14 | G13 (removed) | hard | 0fc8eaa80856:server/internal/governance/caselifecycle/service.go:366 reads governance_workspace_config (pre-removal tree) |
| G15 | G16 | hard | server/internal/handler/provenance.go:138 h.Queries.GetWorkspaceExportPrivacy (export privacy table/query) |
| G15 | G16 | hard | server/internal/handler/provenance.go:78 featureflags.ExportPrivacyControlsEnabled gate |
| G17 | G13 (removed) | hard | 0fc8eaa80856:server/internal/governance/caselifecycle/service.go:20 ErrStaleControlEpoch against governance_workspace_config control epoch (pre-removal tree) |
| G17 | G14 (removed) | hard | 0fc8eaa80856:server/internal/governance/admission.go:19 lifecycle *caselifecycle.Service (pre-removal tree) |
| G18 | G10 (removed) | hard | 0fc8eaa80856:server/internal/governance/receipt/receipt.go:688 jev.Request/jev.Response in Evaluate (pre-removal tree) |
| G19 | G10 (removed) | hard | 0fc8eaa80856:server/internal/handler/squad_jev_routing.go:27 imports server/pkg/jev (pre-removal tree) |
| G19 | G13 (removed) | hard | 0fc8eaa80856:server/internal/handler/squad_jev_routing.go:55 governance.SquadRoutingOff from governance/control.go:13 (pre-removal tree) |
| G20 | G13 (removed) | hard | 0fc8eaa80856:packages/core/api/client.ts:3277 GET /api/workspaces/{id}/governance/config served by governance_config.go (pre-removal tree) |
| G24 | C06 | hard | server/internal/handler/decision_bound_write.go:46 LockDecisionRequest (task-bound decision intents) |
| G24 | C24 | hard | server/internal/handler/decision_bound_write.go:116 decisiontx.AskOption (structured decision ask) |
| G24 | G01 | hard | server/internal/handler/governed_instruction_decision.go:41 governed instruction write path |
| G24 | G02 | hard | server/cmd/multica/governed_cas.go:179 owner-only digest CAS |
| G24 | G04 | hard | server/cmd/multica/governed_instruction_vote.go:65 decisionIDKey vote receipts |
| G26 | C29 | hard | server/internal/handler/decision_action_workflow_step_instructions.go:21 |
| G27 | G04 | hard | server/internal/handler/governed_instruction_receipt.go:26 |
| G30 | G02 | hard | server/internal/handler/config_sync.go:30 |
| D09 | G04 | hard | server/internal/handler/agent.go:434 ClaimedModel reads claimed_model column added by migrations/668_governed_vote_task_identity.up.sql:9 |
| D14 | D06 | hard | server/internal/service/provider_failure_guard.go:297 uses RecordProviderFailureRecoveryParams reservation (D06 layer) |
| D15 | D21 | hard | server/internal/handler/silence_alert.go:118 calls redact.Prose (server/pkg/redact/redact.go:227) |
| D19 | W04 | hard | server/internal/handler/idle_started_guard_act.go:173 calls tryMoveIssueOnEdge defined at server/internal/handler/issue_workflow_ci_advance.go:104 |
| D20 | G04 | hard | server/internal/handler/daemon_preset.go:80 writes ClaimedModel (claimed_model column, migrations/668_governed_vote_task_identity.up.sql:9) |
| D22 | D02 | soft | server/internal/daemon/daemon.go:5072 builds the update progress reporter inside the daemon update flow; without D02 there is no update flow to report on |
| D23 | D06 | hard | server/internal/service/task.go:6156 trips the breaker when a failed task carries ReasonAgentProviderServerError (classification D06 owns) |
| D23 | G09 | hard | server/internal/service/provider_breaker.go:95 resolves the agent model to a provider with ResolveModelProvider (provider_hold.go:83) |
| B01 | B02 | hard | server/internal/service/run_budget_ledger.go:78 openRunBudget calls loadRunBudgetUsage defined in run_budget_snapshot.go:112 |
| B01 | C05 | hard | server/internal/handler/issue_run_budget.go:152 calls isMachineCredentialActor defined in actor_guards.go:60 |
| B02 | B01 | hard | server/internal/service/run_budget_snapshot.go:145 calls openRunBudget (run_budget_ledger.go:67); :177 calls activationEpoch (run_budget_tokens.go:78) |
| B03 | B01 | hard | server/internal/handler/issue_wakeup.go:199 uses errRecoveryCaused defined in issue_run_budget.go:411 |
| B03 | C08 | hard | server/internal/handler/run_budget_decision.go:253 calls h.DecisionWorker.Notify() (decision_worker.go; field at handler.go:249) |
| B03 | C10 | hard | server/internal/handler/run_budget_decision.go:172 builds decisionRequestInput and :201 calls createDecisionDM (decision_request.go:24,440) |
| B04 | B01 | hard | server/internal/handler/decision_action_run_budget.go:29 calls service.LockRunBudgetWindow to lock the ledger window before raising or topping up |
| B04 | C29 | hard | server/internal/handler/decision_action_approve.go:73 maps the new action types into the approve-to-execute executor table |
| B05 | B04 | hard | server/internal/handler/issue_run_budget_request.go:93 looks up the open refusal and tests service.ErrRunBudgetRefusalNotFound, both owned by the refusal record |
| S01 | S02 | hard | server/internal/daemon/sandbox/args.go:77-78 uses egressDir/bridgeMount defined at bridge.go:29-30 |
| S01 | S04 | hard | server/internal/handler/run_profile.go:74 GetIssueTaintIDs from issue_taint.sql |
| S02 | S01 | hard | sandbox/bridge.go:54 func (l *Launcher) bridgeExe; Spec.EgressSocket in sandbox/args.go |
| S03 | S01 | hard | server/internal/daemon/health.go:561 refuses elevated checkout when broker.PinnedSource(...)==""; pins from handler/run_profile.go:120 grantPins, daemon.go:2912 |
| S03 | S04 | hard | server/internal/handler/github_revision_check.go:82 uses task.TaintIds and te.State |
| S04 | S01 | hard | server/internal/handler/task_elevation.go:60 calls validGrantScope defined at run_capability_grant.go:37 |
| S06 | S05 | hard | server/internal/daemon/broker/broker.go:331 calls b.translate inside the push handler of the broker |
| S07 | S05 | hard | server/internal/daemon/broker/broker.go:137 registers POST /readtoken on the broker mux |
| S08 | S03 | hard | server/internal/daemon/readproxy/proxy.go:246 mounts p.externalRoutes on the read proxy mux |
| S09 | S04 | hard | server/internal/handler/elevated_read_filter.go:20 stubs bodies marked external_read or carrying taint, fields S04 owns |
| S10 | S01 | hard | server/internal/daemon/types.go:386 EffectiveProfile reads the profile the server set; DisableProviderWebTools (line 395) builds on it |
| O03 | O05 | hard | deploy/cd/deploy.sh:169 exits if deploy-lib.sh is missing, then sources it (added by 126a8612c3, #66) |
| O04 | O05 | soft | deploy/cd/cutover.sh:140 compose_files includes docker-compose.ab.yml; :474/:694/:810/:928 call router.sh (quiescence.mjs also runs standalone via migrate-supervised.sh) |
| O04 | O08 | hard | deploy/cd/cutover.sh:553 runs release-packet.mjs verify |
| O05 | O04 | hard | deploy/cd/cutover.sh:616 runs quiescence.mjs preflight; :727 final-gate |
| O05 | O08 | hard | deploy/cd/cutover.sh:553 runs release-packet.mjs verify |
| O06 | O03 | hard | deploy/cd/candidate-preflight.sh:19 runs admission.mjs checks; check-deploy-queue.py:24 queries cd-deploy runs |
| O06 | O21 | hard | deploy/cd/candidate-preflight.sh:12 runs require-current.sh |
| O07 | O05 | hard | deploy/cd/cutover.sh:626-639 pull retry loop (3 attempts, sleep pull_attempt*5) lives inside the cutover controller |
| O09 | O05 | hard | deploy/cd/cutover.sh:196-223 jev_overlay_prepare layers docker-compose.jev.yml onto docker-compose.ab.yml services (:222) |
| O10 | O01 | soft | CI sets CI: "true" at .github/workflows/ci.yml:527,783; server/internal/testutil/nodb.go ExitWithoutDatabase fails only when CI is set |
| O15 | O27 | hard | apps/web/features/landing/components/changelog-page-client.tsx:22 imports ./fork-changelog (O27 file) |
| O16 | D06 | hard | server/internal/service/provider_failure_guard.go (defined in D06's source) holds the probe/guard; service/task.go:94 ProviderFailureProbe field and handler.go:517-521 wiring need it |
| O21 | O03 | hard | cd-deploy.yml schedule (4 h) resolving the newest 'CD qualification build' success; :372 admission.mjs verify |
| O21 | O04 | hard | cd-deploy.yml:564-567 render-cutover-remote-script.sh runs cutover.sh |
| O21 | O05 | hard | deploy/cd/cutover.sh:474 router.sh |
| O21 | O06 | hard | cd-deploy.yml:153 runs candidate-preflight.sh |
| O21 | O14 | hard | deploy/cd/require-current.sh:27 reads server_commit; server/internal/config/config.go:110 |
| O23 | O27 | hard | scripts/fork-sync-guard.mjs:89 reads docs/fork/FORK-FEATURES.md via git show <base>:; sourcePaths throws 'No Source paths found' if absent |
| O25 | O20 | hard | ci.yml:20 needs changes; :186 runs ci-scope.mjs decide |
| O25 | O22 | hard | ci.yml:20 needs migration-prefix-check (job at :48) |
| O27 | O15 | hard | scripts/fork-changelog-fetch.mjs:5 imports from ./fork-changelog.mjs |
| O28 | O24 | soft | .github/workflows/security-weekly.yml:43 scans the whole tree with the security-gate rule packs; without O24 the scan loses its rule packs |
| O33 | O26 | soft | playwright.perf.config.ts:17 matches the chip-stability, chip-retention and compact-thread-reply specs in the perf project |

## Notes recorded while tracing

- **C01**: Shared issue-detail.tsx imports other rows' components (not counted as dependencies). Overlap re-confirmed from old refs, not re-diffed line by line.
- **C04**: Reuses upstream github.go projection; no row dependency.
- **C05**: Mutual with B01 (B01 calls isMachineCredentialActor at issue_run_budget.go:152). actor_guards.go is shared with decision rows (C09 calls it at decision_request_api.go:238).
- **C07**: Cycle C07<->C25 (decision_record.go in C25 calls requireDecisionAuthority, shared decision_authority.go).
- **C08**: main.go in source wires many unrelated workers; not counted.
- **C10**: decision_request.go is the main file of the decision-request core; B03 and C09 call into it.
- **C11**: Shared chat-page.tsx imports of C12/C13/C14/C20 not counted.
- **C12**: Index lists service/auto_compact.go under C34; handler/auto_compact.go:9 consumes service.AutoCompactMinPct/MaxPct (same file family).
- **C13**: chat_fork.go uses upstream chat.go types; no feature-specific cross-row call found.
- **C15**: chat.go (shared C05/C18 file) calls listChatMessagesAround from chat_search.go:220 (reverse edge). Reuses upstream escapeLike (issue.go:749).
- **C16**: Reverse edges: chat.go:666 calls publishDMRepliesRead (inbox.go) and service/task.go:5425 calls dmnotify.Reply.
- **C19**: message-time.ts also listed under C31.
- **C22**: The Cmd+K part of the title partly exists upstream; the site-search URLs and OpenSearch discovery do not.
- **C23**: Tests-only row; presence means the guard tests exist.
- **C24**: Dropped dmnotify.go and notification-preferences files (they belong to C16). decisiontx needs decision_request tables (migration 597), which no ledger row owns.
- **C25**: Cycle C07<->C25 (decision_record_api.go:46 calls createAskedDecisionRecord). Dropped decision-disclaim.ts (belongs to C07).
- **C26**: Cycle: decision_worker.go:197 (C08) calls deliverDecisionVetoNotice from this row.
- **C27**: Reverse: decision_request_api.go:77 (C06/C09) calls verifyAskArtifacts from this row.
- **C28**: B03 and the decision cluster call each other (isServerDecision vs decision_delivery.go:374).
- **C29**: server/internal/passkey/enroll.go:101 and revoke_code.go:35 call gate.go symbols; row ownership of the passkey server package not resolved.
- **C30**: Shared files dropped from source (activity_listeners.go, auth/session.go exist upstream).
- **C33**: Dropped chat-message-list.tsx and chat.tsx (shared with other rows).
- **C34**: Other listed sources (auto_compact.go, cmd_chat.go, chat_continue.go, messages.go) are shared with C11/C12; only chat_compact.go is wholly this row.
- **C36**: Overlap is only the archived-session state; stats and pin-session are fork-only.
- **C39**: Rich wakeup prompt part (#642) edits wakeups-section.tsx, an upstream file.
- **C42**: Listed sources were shared desktop shell files; media-specific files are the fix list.
- **C43**: admission.go and dispatch/reason.go are upstream files (created in MUL-4302 4fac8d772f), so dropped from sources.
- **C44**: issue_relations.go SQL reads issue_wakeup and github_pull_request, both upstream tables.
- **C45**: Dropped packages/ui dialog.tsx (shared).
- **C47**: issue_workflow_write.go (W19) is touched by #383 but has no project-admission reference at HEAD; no dependency recorded.
- **C49**: Dropped auth/store.ts, use-logout.ts, shutdown.go (shared).
- **C50**: also uses C58 passkey approval component (shared file decision-action-approval.tsx, listed in both Sources); no separate edge.
- **C51**: workspace_delegated_writes.go is in both C51 and G24 territory; G24 edge is soft because config-change runs when the setting is off.
- **C54**: C54 is hard on C08 delivery; C08 reads episodes back through a soft edge, so there is no hard cycle. server/internal/issueworkflow/issueworkflow.go:52 HandlerHumanInCharge is used at decision_wait_entry.go:77; no ledger row owns that constant (W15/W18 nearest), so no edge.
- **C56**: runs without C09 in the code read; no call line found, edge is none.
- **C57**: shadow only; issue.go:4755 recordSystem1Routing is a shared-file hook.
- **C60**: session_generation.go handler uses the C30 session-generation lock (LockUserSessionGeneration at :58) but C30 Source names other files; no edge written.
- **C61**: C28 file chat_decision_reply.go calls into attention (reverse edge above). Shared files with C34 and C35 (task.go): applyHumanSendAttention at service/task.go:3191.
- **C63**: no draft consumer call line verified; edge none.
- **C65**: extends the status-history core named in C30 title (#773). C30 Source does not list status_history.go, so ownership is unresolved and no edge is written.
- **C66**: TimelineRow type mentions status_changed rows only in a comment (issue_actual_timeline.go:24); no code edge to C65.
- **C67**: C45 now imports canvas-math from the split (reverse edge above).
- **C68, C69, C70**: no row-to-row edge verified. C70 shares chat_compact.go with C34 (same file, no call line).
- **H01**: Source list had two commit hashes (ffa2f9711, 2b558f361), not paths; dropped router.go as shared.
- **H03**: Upstream 6388272a8a (#9598) touched the same file (skip move when project workflow lacks target) but is not the custom-state behavior.
- **H05**: BaseSHA/BaseRef in snapshot.go come from H11, not H05.
- **H07**: Only migration bookkeeping references remain.
- **H08**: #206 also bundles unrelated chat decision-request changes.
- **H09**: handlePullRequestReviewEvent has no production caller since H10 intake took over; mutual dependency with H10.
- **H10**: Dependency cycle with H09.
- **H11**: task_supplement.go is upstream code (MUL-7565) extended by H11.
- **W01**: Fork cmd_workflow.go builds on the upstream file; fork-only parts depend on W15-W19 server schema only at API level.
- **W02**: Fork issue-flow-card.tsx is a +118/-16 extension of the upstream read-only card.
- **W03**: Builds on upstream workflow substrate; no fork row dependency found.
- **W04**: Wake and advance share one call site (github.go:1432-1436).
- **W05**: issue_wakeup_system.go is shared by W04-W10; extracting W05 alone drags W06/W08/W09/W11.
- **W06**: Cycle W06 <-> W08 <-> W09: builtinWakeup struct (wakeup_builtin_exec.go:50) carries all three; split them together.
- **W07**: Pool table is 583_wakeup_default_capacity.up.sql.
- **W10**: Pause exists upstream; only recovery is fork code.
- **W11**: service/issue_wakeup_pause.go (the settle/drop-late logic) is not in the row's source list; consider adding it. W05 depends on this row.
- **W12**: Soft dep unverified beyond the constant; no hard imports of other rows in these three files.
- **W13**: Old ledger said fork-only; upstream comment coalescing is partial overlap (medium confidence).
- **W15**: Cycle W15 <-> W16: edges engine calls sub-workflow spawn; sub-workflow step is an edge target.
- **W16**: Cycle with W15.
- **W17**: Verifier and nonce tables stand alone; only the guard that reads events needs W15.
- **W18**: Source list trimmed; no hard cross-row symbol found.
- **W19**: Entry gates reuse the edge/guard engine.
- **W20**: Edge to H07 is the surviving helper only (a second caller is github.go:2054). Not independent: the W20 PR must carry `githubEnabledForWorkspace` (about 17 lines), so W20 sits in Close gaps first until it does. Earlier revision of this file listed the symbol here but omitted the edge.
- **W21**: Source list had 5 files; wakeup_schedule.go added as the schedule home.
- **W22**: Source list trimmed of mobile inbox files.
- **G04**: G04 and G24 share governed_cas.go; G24 side of the cycle not checked in this batch.
- **G09**: No hard dependency verified; shares admission.go with G08 typed reasons (not confirmed as a code need).
- **G10**: Row candidate for deletion from ledger.
- **G11**: comment.go survives but only keeps a jev_route type check at line 2176 and evidence redaction at 4455.
- **G13**: Table governance_workspace_config may still exist in migrations.
- **G20**: Tab and endpoints are gone; only the generic settings page remains.
- **G25**: Row describes the removal itself (CHE-2760/2761).
- **D01**: 191c782d60 is an ancestor of v0.7.0; no upstream change to the lock since.
- **D02**: router.go is a shared file in sources; no fix list change made.
- **D04**: Partial adjacency: upstream d5297e2f8d (MUL-8400) disables Codex agents for catalog-v2 models.
- **D06**: Overlap carried from old ledger refs; not re-diffed line by line.
- **D09**: Old overlap ref 482ec69c1 does not cover claimed-model readback; reclassified fork-only.
- **D11**: Pricing was already in v0.7.0 per git log; recheck fork utils.ts residue before dropping the row.
- **D18**: No production caller of recovery_claim.go found at HEAD; scaffold only.
- **B01**: Mutual hard cycle with B02 (snapshot calls openRunBudget/activationEpoch); github_intake_worker.go has 0 budget references so dropped from source.
- **B02**: Dropped shared inbox/create.go (2 budget mentions in a 74-line generic helper).
- **B03**: C08 decision_delivery.go:374 also calls isServerDecision from run_budget_decision.go (reverse edge, cycle B03<->C08). Dropped runtime_sweeper.go (one call at :179) and issue_wakeup.go (one guard at :199) as shared files; top-up handler lives in issue_run_budget.go.
- **S01**: S01/S02 and S01/S04 cycles; wired at daemon.go:35,797 and config.go:20.
- **S02**: egress package has no non-test importers at HEAD; bridge wired at cmd/multica/main.go:122.
- **S03**: readproxy.ShimMain wired at cmd/multica/main.go:125-126; host proxy has no non-test importers.
- **S04**: Route router.go:2166 /taint/clear; agent_access.go, file.go:472 and issue_trigger.go:129-190 are shared with upstream.
- **S05**: Source list trimmed to 6; no cross-row imports found.
- **O01**: ci.yml now runs on ubuntu-24.04; release and cd-qualification still use cheese-x99.
- **O04**: O04/O05 cycle: both live in cutover.sh.
- **O05**: O04/O05 cycle.
- **O08**: No code deps found; consumed by O04/O05 (cutover.sh:553).
- **O09**: Deploy-side overlay (docker-compose.jev.yml, cutover.sh jev_overlay_prepare, build-cutover-bundle.sh:42, test-jev-overlay.sh) is still present; the server consumer is gone.
- **O12**: Fork origin 954a32d3d2 (#183); nothing left to carry.
- **O14**: O21 depends on this (require-current.sh:27 reads server_commit).
- **O15**: O15/O27 cycle.
- **O16**: D06 and D14 also edit provider_failure_guard.go and task.go.
- **O17**: Both source files are shared with upstream edits, so syncs conflict here.
- **O21**: cd-deploy.yml:89-91 concurrency cd-deploy-c00-approval; :168-170 queue max.
- **O23**: pr-guards.yml (fork-sync-guard job) runs the script.
- **O24**: trusted-authors.json read at security_gate.py:214 (same row).
- **O25**: warning-budget.mjs:162,168 baselines come from main push runs and throw if none; push jobs are skipped when merge_group_green (ci.yml:780).
- **O26**: Only the web page-load baseline survives; the governance teardown flake and stale-verdict race tests were removed with governance.
- **O27**: help-launcher.tsx shared with O14; issues.md shared with O17.
- **X06 to X14**: design-only, none.
