package handler

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestGovernanceWorkspaceDeletionManifestHasExplicitQueries(t *testing.T) {
	queries, err := os.ReadFile("../../pkg/db/queries/workspace_delete.sql")
	if err != nil {
		t.Fatal(err)
	}
	for table, action := range workspaceDeletionManifest {
		if !strings.HasPrefix(table, "governance_") {
			continue
		}
		if action != workspaceDelete || !strings.Contains(string(queries), "DELETE FROM "+table+" WHERE workspace_id = $1") {
			t.Errorf("governance table %s lacks explicit workspace-scoped teardown", table)
		}
	}
}

func TestProviderBreakerWorkspaceDeletionIsExplicit(t *testing.T) {
	if workspaceDeletionManifest["provider_breaker"] != workspaceDelete {
		t.Fatal("provider_breaker must be classified for explicit workspace deletion")
	}

	queries, err := os.ReadFile("../../pkg/db/queries/workspace_delete.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(queries), "DELETE FROM provider_breaker WHERE workspace_id = $1") {
		t.Fatal("workspace deletion queries must explicitly delete provider_breaker")
	}

	handler, err := os.ReadFile("workspace.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(handler), "DeleteProviderBreakersForWorkspace(ctx, requester.WorkspaceID)") {
		t.Fatal("workspace deletion graph must run the provider_breaker teardown query")
	}
}

// CHE-7545: these workspace-keyed tables have no FK to workspace, so only an
// explicit statement in the leaf teardown removes their rows.
func TestWorkspaceKeyedNoFKTablesHaveExplicitTeardown(t *testing.T) {
	queries, err := os.ReadFile("../../pkg/db/queries/workspace_delete.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"ask_recovery_notice", "ask_repair_root", "browser_gateway_credential",
		"issue_handoff_dispatch", "issue_step_assignee", "issue_step_watchdog_action",
		"merge_receipt_fact", "merge_veto", "merge_veto_deadline", "decision_backing_lane", "guard_decision",
		"weekly_run_guard_allowance", "weekly_run_guard_refusal",
	} {
		if workspaceDeletionManifest[table] != workspaceDelete {
			t.Errorf("%s must be classified workspaceDelete", table)
		}
		if !strings.Contains(string(queries), "DELETE FROM "+table+" WHERE workspace_id = $1") {
			t.Errorf("%s lacks explicit workspace-scoped teardown", table)
		}
	}
}

type workspaceDeleteAction string

const (
	workspaceDelete       workspaceDeleteAction = "delete"
	workspaceDeleteDetach workspaceDeleteAction = "detach"
	workspaceDeleteKeep   workspaceDeleteAction = "keep"
	workspaceDeleteSettle workspaceDeleteAction = "settle"
	workspaceDeleteWorker workspaceDeleteAction = "decision-worker"
)

// workspaceDeletionManifest is the schema coverage contract for workspace
// teardown. Adding a table requires an explicit ownership decision here; the
// handler deletion graph must then implement that decision before CI passes.
var workspaceDeletionManifest = map[string]workspaceDeleteAction{
	"cloud_agent_connection":             workspaceDelete,
	"merge_queue_intake":                 workspaceDelete,
	"merge_queue_epoch":                  workspaceDelete,
	"merge_queue_association":            workspaceDelete,
	"merge_queue_wait":                   workspaceDelete,
	"merge_queue_pr_close":               workspaceDelete,
	"merge_queue_enqueue_event":          workspaceDelete,
	"merge_queue_state":                  workspaceDelete,
	"merge_queue_evidence":               workspaceDelete,
	"daemon_pause":                       workspaceDelete,
	"activity_log":                       workspaceDelete,
	"workspace_audit_event":              workspaceDelete,
	"agent":                              workspaceDelete,
	"agent_builder_draft":                workspaceDelete,
	"agent_invocation_target":            workspaceDelete,
	"agent_preset":                       workspaceDelete,
	"agent_runtime":                      workspaceDelete,
	"agent_skill":                        workspaceDelete,
	"agent_task_event":                   workspaceDelete,
	"agent_task_queue":                   workspaceDelete,
	"ops_snapshot":                       workspaceDelete,
	"ops_snapshot_5m":                    workspaceDelete,
	"agent_to_label":                     workspaceDelete,
	"attachment":                         workspaceDelete,
	"autopilot":                          workspaceDelete,
	"autopilot_collaborator":             workspaceDelete,
	"autopilot_quota_period":             workspaceDelete,
	"autopilot_quota_reservation":        workspaceDelete,
	"autopilot_rule_version":             workspaceDelete,
	"autopilot_run":                      workspaceDelete,
	"autopilot_subscriber":               workspaceDelete,
	"autopilot_trigger":                  workspaceDelete,
	"channel_binding_token":              workspaceDelete,
	"channel_chat_context_generation":    workspaceDelete,
	"channel_chat_session_binding":       workspaceDelete,
	"channel_inbound_audit":              workspaceDelete,
	"channel_inbound_message_dedup":      workspaceDelete,
	"channel_installation":               workspaceDelete,
	"channel_media_pending_object":       workspaceDeleteSettle,
	"channel_outbound_card_message":      workspaceDelete,
	"channel_outbound_message":           workspaceDelete,
	"channel_reply_delivery":             workspaceDelete,
	"channel_task_delivery":              workspaceDelete,
	"channel_typing_reaction":            workspaceDeleteSettle, // Source deletion makes retained anchors eligible for the cleanup worker.
	"channel_user_binding":               workspaceDelete,
	"chat_attention_item":                workspaceDelete, // no FK; removed by workspace_id in DeleteWorkspaceCommunicationRoots.
	"chat_attention_mark":                workspaceDelete, // no workspace_id; keyed by chain root session, removed with the sessions.
	"chat_draft_restore":                 workspaceDelete,
	"chat_message":                       workspaceDelete,
	"chat_pinned_agent":                  workspaceDelete,
	"chat_session":                       workspaceDelete,
	"chat_submission_receipt":            workspaceDelete,
	"chat_wakeup":                        workspaceDelete,
	"chat_wakeup_fork_conversion":        workspaceDeleteKeep, // CHE-10195 audit record of migration 99zzz00, no workspace_id.
	"chat_wakeup_fork_saved":             workspaceDeleteKeep, // CHE-10195 recovery copy of fork rows, emptied by the 99zzz00 down; has workspace_id, no FK.
	"chat_wakeup_fork_saved_input":       workspaceDeleteKeep, // CHE-10195 prior message_kind of wakeup input rows, no workspace_id.
	"client_usage_daily":                 workspaceDeleteDetach,
	"comment":                            workspaceDelete,
	"comment_idempotency_receipt":        workspaceDelete,
	"comment_reaction":                   workspaceDelete,
	"comment_search_text":                workspaceDelete, // CHE-2943 derived; the comment delete trigger removes it, no FK.
	"comment_supersession":               workspaceDelete,
	"comment_trigger_rate_limit":         workspaceDelete,
	"contact_sales_inquiry":              workspaceDeleteKeep,
	"daemon_connection":                  workspaceDelete,
	"daemon_token":                       workspaceDelete,
	"dashboard_range_view":               workspaceDelete,
	"dingtalk_group_presence":            workspaceDelete,
	"dingtalk_bot_identity":              workspaceDelete,
	"dingtalk_group_route":               workspaceDelete,
	"doc":                                workspaceDelete,
	"doc_collab_state":                   workspaceDelete,
	"doc_comment":                        workspaceDelete,
	"doc_folder":                         workspaceDelete,
	"doc_revision":                       workspaceDelete,
	"doc_version":                        workspaceDelete,
	"entitlement_feature_attempt":        workspaceDelete,
	"entitlement_limit_overage":          workspaceDelete,
	"feedback":                           workspaceDeleteDetach,
	"github_dispatch_halt":               workspaceDelete,
	"github_dispatch_halt_audit":         workspaceDelete,
	"github_installation":                workspaceDelete,
	"github_pending_check_suite":         workspaceDelete,
	"github_route_grant":                 workspaceDelete,
	"github_pending_installation":        workspaceDeleteKeep,
	"github_pr_closed_replay":            workspaceDeleteKeep, // CHE-3402 retry marker keyed by installation address (shared across workspaces); no workspace_id. A replay with no routed workspace is a no-op that clears it.
	"github_pull_request":                workspaceDelete,
	"github_pull_request_check_run":      workspaceDelete,
	"github_pull_request_check_suite":    workspaceDelete,
	"github_workflow_job":                workspaceDelete, // CHE-2147 webhook-fed run/job mirror; workspace-owned leaf.
	"github_workflow_run":                workspaceDelete, // CHE-2147 webhook-fed run/job mirror; workspace-owned leaf.
	"github_webhook_intake":              workspaceDeleteKeep,
	"github_snapshot_approval":           workspaceDelete,
	"read_snapshot_approval":             workspaceDelete, // CHE-2509 elevated-read approvals; workspace-owned leaf.
	"github_webhook_dispatch":            workspaceDelete,
	"github_webhook_revision":            workspaceDelete,
	"governed_vote_record":               workspaceDelete, // CHE-1862 vote-gate ledger; workspace-owned leaf.
	"governed_pass_receipt":              workspaceDelete, // CHE-3490 single-use PASS receipts; workspace-owned leaf.
	"run_gate_event":                     workspaceDelete, // CHE-1918 run-gate shadow ledger; workspace-owned leaf.
	"governance_receipt":                 workspaceDelete, // CHE-685 Jev governance routing receipts; workspace-owned, same shape as issue_checkpoint.
	"governance_budget_window":           workspaceDelete,
	"governance_budget_root":             workspaceDelete,
	"governance_budget_reservation":      workspaceDelete,
	"governance_budget_journal":          workspaceDelete,
	"governance_budget_outbox":           workspaceDelete,
	"governance_concurrency_guard":       workspaceDelete,
	"governance_concurrency_hold":        workspaceDelete,
	"governance_jev_credential":          workspaceDelete, // CHE-714 write-only encrypted Jev provider credential; explicit transactional teardown.
	"governance_workspace_config":        workspaceDelete, // CHE-707 versioned workspace governance gate and control epoch.
	"governance_workspace_config_audit":  workspaceDelete, // CHE-707 idempotency, rollback, and operating-limit audit history.
	"governance_case":                    workspaceDelete, // CHE-704 C01 lifecycle authority; no FK/cascade teardown.
	"governance_case_transition":         workspaceDelete, // CHE-704 immutable CAS/audit trail.
	"governance_attempt":                 workspaceDelete, // CHE-704 case-bound Jev/agent attempt evidence.
	"governance_evaluation":              workspaceDelete, // CHE-704 immutable snapshot/vector/candidate/citation map.
	"governance_evaluation_source":       workspaceDelete, // CHE-704 copied-context source index/redaction lookup.
	"inbox_item":                         workspaceDelete,
	"instance_telemetry_state":           workspaceDeleteKeep,
	"issue":                              workspaceDelete,
	"issue_checkpoint":                   workspaceDelete,
	"issue_wakeup":                       workspaceDelete,
	"issue_wakeup_definition":            workspaceDelete,
	"dm_progress_delivery":               workspaceDelete, // CHE-3405 sent progress cards; no FK, swept by DeleteWorkspaceIssueRoots.
	"dm_progress_follow":                 workspaceDelete, // CHE-3369 C1 follow state; no FK, swept by DeleteWorkspaceIssueRoots.
	"dm_progress_data_incident":          workspaceDelete, // CHE-3546 L0 data incident; no FK, swept by DeleteWorkspaceIssueRoots.
	"dm_progress_lane":                   workspaceDelete, // CHE-3369 C1 lane row; no FK, swept by DeleteWorkspaceIssueRoots.
	"dm_progress_lane_event":             workspaceDelete, // CHE-3548 moved-under lane event; no FK, swept by DeleteWorkspaceIssueRoots.
	"dm_progress_pending":                workspaceDelete, // CHE-3404 unsent progress events; no FK, swept by DeleteWorkspaceIssueRoots.
	"issue_wakeup_pr_event":              workspaceDelete,
	"issue_ci_advance":                   workspaceDelete,
	"issue_workflow_switch_pending":      workspaceDelete, // CHE-7669 deferred workflow switch; no FK, swept by DeleteWorkspaceIssueRoots and the issue delete.
	"stage_queue_waiting":                workspaceDelete, // CHE-3611 refused-PR ledger; issue_id and agent_id FKs ON DELETE CASCADE remove its rows with the workspace.
	"issue_decision_park":                workspaceDelete, // CHE-3946 decision park; issue_id FK ON DELETE CASCADE removes its rows with the workspace.
	"issue_wakeup_receipt":               workspaceDelete,
	"issue_child_event":                  workspaceDelete,
	"issue_view":                         workspaceDelete,
	"issue_view_preference":              workspaceDelete,
	"issue_blocker":                      workspaceDelete,
	"issue_dependency":                   workspaceDelete,
	"issue_description_collab_state":     workspaceDelete,
	"issue_label":                        workspaceDelete,
	"issue_property":                     workspaceDelete,
	"issue_pr_automation":                workspaceDelete,
	"issue_pull_request":                 workspaceDelete,
	"issue_pull_request_exclusion":       workspaceDelete,
	"issue_pr_event":                     workspaceDelete, // CHE-2298 a1 PR event store; no FKs, swept by DeleteWorkspace.
	"issue_review_admission":             workspaceDelete,
	"issue_run_budget_entry":             workspaceDelete,
	"issue_reaction":                     workspaceDelete,
	"issue_source_context":               workspaceDelete,
	"issue_source_context_object_intent": workspaceDeleteSettle,
	"issue_search_text":                  workspaceDelete, // CHE-2943 derived; the issue delete trigger removes it, no FK.
	"issue_status":                       workspaceDelete,
	"issue_workflow":                     workspaceDelete,
	"issue_workflow_lint_override":       workspaceDelete,
	"issue_subscriber":                   workspaceDelete,
	"issue_to_label":                     workspaceDelete,
	"issue_unread":                       workspaceDelete,
	"issue_vcs_pull_request":             workspaceDelete,
	"lark_binding_token":                 workspaceDelete,
	"lark_chat_session_binding":          workspaceDelete,
	"lark_inbound_audit":                 workspaceDelete,
	"lark_inbound_message_dedup":         workspaceDelete,
	"lark_installation":                  workspaceDelete,
	"lark_outbound_card_message":         workspaceDelete,
	"lark_user_binding":                  workspaceDelete,
	"maintenance_job":                    workspaceDeleteKeep, // Global maintenance audit history, not workspace-owned.
	"member":                             workspaceDelete,
	"member_search_access":               workspaceDelete,
	"member_delegation_day":              workspaceDeleteKeep, // user-scoped telemetry ledger, no workspace_id
	"member_setting":                     workspaceDelete,
	"browser_instance":                   workspaceDelete,
	"browser_lease":                      workspaceDelete,
	"browser_profile":                    workspaceDelete,
	"browser_profile_agent":              workspaceDelete,
	"browser_profile_member":             workspaceDelete,
	"review_admission_deferral":          workspaceDeleteKeep, // no workspace_id; rows expire after 24 h (CHE-4650)
	"host_sample":                        workspaceDeleteKeep, // host telemetry keyed by daemon_id, no workspace_id; outlives workspaces by design
	"host_sample_hourly":                 workspaceDeleteKeep, // hourly rollup of host_sample; same reason
	"agent_mcp_server":                   workspaceDelete,
	"wakeup_aggregate_budget":            workspaceDelete,
	"wakeup_aggregate_reservation":       workspaceDelete,
	"wakeup_scoped_event":                workspaceDelete,
	"workspace_mcp_server":               workspaceDelete,
	"notification_channel_preference":    workspaceDelete,
	"notification_preference":            workspaceDelete,
	"personal_access_token":              workspaceDeleteKeep,
	"passkey_assertion_challenge":        workspaceDeleteKeep,
	"passkey_enroll_ceremony":            workspaceDeleteKeep,
	"passkey_enroll_limit":               workspaceDeleteKeep,
	"passkey_event":                      workspaceDeleteKeep,
	"pinned_item":                        workspaceDelete,
	"plugin_installation":                workspaceDelete,
	"plugin_hook_schedule":               workspaceDelete,
	"plugin_guard_metadata_key":          workspaceDelete,
	"plugin_invocation":                  workspaceDelete,
	"plugin_storage":                     workspaceDelete,
	"plugin_secret":                      workspaceDelete,
	"plugin_package":                     workspaceDelete,
	"plugin_package_version":             workspaceDelete,
	"plugin_package_file":                workspaceDelete,
	"project":                            workspaceDelete,
	"project_folder":                     workspaceDelete,
	"project_member":                     workspaceDelete,
	"project_subscriber":                 workspaceDelete,
	"project_resource":                   workspaceDelete,
	"protocol_lint_run":                  workspaceDeleteKeep, // Cross-workspace protocol-lint audit/reporting data (CHE-552), not workspace-owned.
	"provenance_export_log":              workspaceDelete,     // CHE-755 provenance export audit rows; workspace-owned leaf.
	"provider_breaker":                   workspaceDelete,     // CHE-1735 provider state is removed with its workspace.
	"provider_breaker_audit":             workspaceDelete,     // CHE-4381 breaker transition audit; removed with DeleteProviderBreakersForWorkspace.
	"provider_failure_recovery":          workspaceDeleteKeep,
	"recovery_claim":                     workspaceDelete,     // CHE-1126 gateway-recovery claim ledger; workspace_id FK ON DELETE CASCADE plus explicit teardown.
	"push_delivery":                      workspaceDeleteKeep, // Outbox rows of inbox items; the push worker drops the ones whose item is gone (CHE-1795).
	"push_session_end":                   workspaceDeleteKeep, // Ended login sessions, account-level, expire with the session lifetime (CHE-1795).
	"push_subscription":                  workspaceDeleteKeep, // A user's device across all workspaces, not workspace-owned (CHE-1795).
	"quick_action":                       workspaceDelete,
	"run_budget_notice":                  workspaceDelete,
	"run_budget_refusal":                 workspaceDelete,
	"run_capability_grant":               workspaceDelete,
	"protected_path_policy":              workspaceDelete,
	"task_elevation":                     workspaceDelete,
	"elevated_op":                        workspaceDelete,
	"issue_taint":                        workspaceDelete,
	"issue_taint_clear":                  workspaceDelete,
	"issue_run_taint":                    workspaceDelete,
	"runtime_profile":                    workspaceDelete,
	"schema_contracts":                   workspaceDeleteKeep, // CHE-7486 N5r runner-created fence row per contract migration, no workspace_id.
	"schema_migrations":                  workspaceDeleteKeep,
	"search_index_change":                workspaceDelete,
	"search_index_prune_mark":            workspaceDeleteKeep,
	"seat_capacity_outbox":               workspaceDeleteSettle,
	"site_counter":                       workspaceDeleteKeep,
	"skill":                              workspaceDelete,
	"skill_file":                         workspaceDelete,
	"skill_to_label":                     workspaceDelete,
	"skill_version":                      workspaceDelete,
	"squad":                              workspaceDelete,
	"squad_member":                       workspaceDelete,
	"stage_completion_wake":              workspaceDelete,
	"stage_generation":                   workspaceDelete,
	"sys_cron_executions":                workspaceDeleteKeep,
	"theme_store_download":               workspaceDeleteKeep, // per registry theme, not workspace-owned
	"task_context_receipt":               workspaceDelete,
	"runtime_rate_limit":                 workspaceDelete,
	"task_message":                       workspaceDelete,
	"chat_task_supplement":               workspaceDelete,
	"chat_task_supplement_fork_875":      workspaceDelete,
	"task_supplement":                    workspaceDelete,
	"task_supplement_capability":         workspaceDelete,
	"task_pr_event_capability":           workspaceDelete, // CHE-2298 a2
	"task_pr_event_delivery":             workspaceDelete, // CHE-2298 a2
	"task_token":                         workspaceDelete,
	"task_usage":                         workspaceDelete,
	"task_usage_hourly":                  workspaceDelete,
	"task_usage_hourly_dirty":            workspaceDelete,
	"task_usage_hourly_rollup_state":     workspaceDeleteKeep,
	"user":                               workspaceDeleteKeep,
	"user_composio_connection":           workspaceDelete, // personal accounts made in the workspace; older rows have no workspace and stay
	"vcs_commit_status":                  workspaceDelete,
	"vcs_connection":                     workspaceDelete,
	"vcs_pull_request":                   workspaceDelete,
	"verification_code":                  workspaceDeleteKeep,
	"webhook_delivery":                   workspaceDelete,
	"workflow_webhook_endpoint":          workspaceDelete, // CHE-2031 inbound webhook guard; no FK.
	"workflow_webhook_event":             workspaceDelete, // CHE-2031
	"workflow_webhook_nonce":             workspaceDelete, // CHE-2031 no workspace_id; deleted through its endpoint.
	"wecom_install_session":              workspaceDelete,
	"weixin_login_session":               workspaceDelete,
	"weixin_account_state":               workspaceDelete,
	"workspace_composio_connection":      workspaceDelete,
	"remote_app_connection":              workspaceDelete, // team rows (owner_id = workspace) and personal rows made in the workspace
	"remote_app_oauth_client":            workspaceDeleteKeep,
	"agent_app":                          workspaceDelete,
	"app_action_rule":                    workspaceDelete,
	"app_action_request":                 workspaceDelete,
	"app_tool_call":                      workspaceDelete,
	"app_tool_call_quota_period":         workspaceDelete,
	"issue_environment":                  workspaceDelete,
	"issue_environment_command":          workspaceDelete,
	"issue_environment_config":           workspaceDelete,
	"workspace_app_setting":              workspaceDelete,
	"app_tool_class_override":            workspaceDelete,
	"workspace":                          workspaceDelete,
	"workspace_config_sync":              workspaceDelete,
	"ask_guard_event":                    workspaceDelete,
	"system1_observation":                workspaceDelete,     // CHE-3628 no FK.
	"system1_label":                      workspaceDelete,     // CHE-4470 no FK.
	"system1_prediction":                 workspaceDelete,     // CHE-4471 no FK.
	"system1_label_sample":               workspaceDelete,     // CHE-4473 no FK.
	"ci_watch_config":                    workspaceDelete,     // CHE-4273 CI watcher settings; no FK.
	"ci_watch_config_audit":              workspaceDelete,     // CHE-4273
	"ci_failure_event":                   workspaceDelete,     // CHE-4273
	"ci_watch_action":                    workspaceDelete,     // CHE-4273
	"ci_signature_owner":                 workspaceDelete,     // CHE-4273
	"ci_flake_increment":                 workspaceDelete,     // CHE-4280 flake counter; no FK.
	"ci_watch_event":                     workspaceDelete,     // CHE-4279 storm-breaker event log; no FK.
	"gh_installation_budget":             workspaceDeleteKeep, // CHE-4273 per GitHub App installation, shared across workspaces; no workspace_id.
	"gh_budget_reservation":              workspaceDeleteKeep, // CHE-4273 same; no workspace_id.
	"ask_recovery_notice":                workspaceDelete,     // CHE-7545 no FK; DeleteWorkspaceLeafData.
	"ask_repair_root":                    workspaceDelete,     // CHE-7545 no FK; DeleteWorkspaceLeafData.
	"browser_gateway_credential":         workspaceDelete,     // CHE-7545 no FK; DeleteWorkspaceLeafData.
	"ci_broker_audit":                    workspaceDeleteKeep, // CHE-7545 global GitHub-repository audit; no workspace_id.
	"comment_content_edit":               workspaceDeleteKeep, // CHE-7545 keyed by comment_id, no FK, no workspace_id; orphan edit marks survive comment deletion (tiny; unsure).
	"daemon_update_cycle":                workspaceDeleteKeep, // CHE-7545 keyed by machine owner and daemon; no workspace_id.
	"daemon_update_schedule":             workspaceDeleteKeep, // CHE-7545 keyed by machine owner and daemon; no workspace_id.
	"dm_progress_epic_episode":           workspaceDelete,     // CHE-7545 no FK; DeleteDMProgressByWorkspace.
	"github_release_deploy":              workspaceDelete,     // CHE-7545 FK workspace ON DELETE CASCADE.
	"github_release_intake":              workspaceDeleteKeep, // CHE-7545 per GitHub App installation; no workspace_id.
	"github_release_pending":             workspaceDelete,     // CHE-7545 FK workspace ON DELETE CASCADE.
	"issue_description_revision":         workspaceDelete,     // CHE-7545 FK issue ON DELETE CASCADE; no workspace_id.
	"issue_handoff_dispatch":             workspaceDelete,     // CHE-7545 no FK; DeleteWorkspaceLeafData.
	"issue_handoff_operation":            workspaceDelete,     // CHE-5491 per-request retry receipt, no FK; DeleteWorkspaceLeafData.
	"machine_banner_dismissal":           workspaceDelete,     // CHE-9190 no FK; DeleteWorkspaceIssueRoots.
	"issue_step_assignee":                workspaceDelete,     // CHE-7545 no FK; DeleteWorkspaceLeafData.
	"issue_step_watchdog_action":         workspaceDelete,     // CHE-7545 no FK; DeleteWorkspaceLeafData.
	"merge_pause_state":                  workspaceDeleteKeep, // CHE-7545 per repository variable cache; no workspace_id.
	"merge_receipt_fact":                 workspaceDelete,     // CHE-7545 no FK; DeleteWorkspaceLeafData.
	"merge_veto":                         workspaceDelete,     // CHE-7545 no FK; DeleteWorkspaceLeafData.
	"merge_veto_deadline":                workspaceDelete,     // CHE-7545 no FK; DeleteWorkspaceLeafData.
	"run_budget_window_cutover":          workspaceDeleteKeep, // CHE-7545 global singleton; no workspace_id.
	"server_deploy":                      workspaceDeleteKeep, // CHE-7545 global deploy log; no workspace_id.
	"system1_epoch":                      workspaceDelete,     // CHE-7545 no FK; DeleteWorkspaceLeafData.
	"weekly_run_guard_allowance":         workspaceDelete,     // CHE-7545 no FK; DeleteWorkspaceLeafData.
	"weekly_run_guard_refusal":           workspaceDelete,     // CHE-7545 no FK; DeleteWorkspaceLeafData.
	"claim_poll_stats":                   workspaceDeleteKeep, // CHE-7545 global per-minute claim poll histogram; no workspace_id.
	"decision_backing_lane":              workspaceDelete,     // CHE-7545 workspace_id column, no FK; explicit teardown in DeleteWorkspaceAdministration.
	"guard_decision":                     workspaceDelete,     // CHE-7545 workspace_id column, no FK (migration 9999940); explicit teardown in DeleteWorkspaceAdministration.
	"decision_pick":                      workspaceDeleteKeep, // CHE-7545 UNSURE: keyed by decision_request_id and member_id, no FK, no workspace_id; rows orphan after request cleanup. Decision-worker owner should add cleanup.
	"decision_snooze":                    workspaceDeleteKeep, // CHE-7545 UNSURE: same as decision_pick.
	"workspace_invitation":               workspaceDelete,
	"workspace_share_link":               workspaceDelete,
}

func TestWorkspaceDeletionManifestCoversPublicSchema(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}

	rows, err := testPool.Query(context.Background(), `
SELECT tablename
FROM pg_tables
WHERE schemaname = 'public'
ORDER BY tablename
`)
	if err != nil {
		t.Fatalf("list public tables: %v", err)
	}
	defer rows.Close()

	actual := make(map[string]struct{}, len(workspaceDeletionManifest))
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan public table: %v", err)
		}
		actual[table] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate public tables: %v", err)
	}

	var unclassified, missing []string
	for table := range actual {
		if _, ok := workspaceDeletionManifest[table]; !ok {
			unclassified = append(unclassified, table)
		}
	}
	for table := range workspaceDeletionManifest {
		if _, ok := actual[table]; !ok {
			missing = append(missing, table)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(missing)
	if len(unclassified) > 0 || len(missing) > 0 {
		t.Fatalf("workspace deletion manifest drift: unclassified=%v missing=%v", unclassified, missing)
	}

	workspaceColumns, err := testPool.Query(context.Background(), `
SELECT table_name
FROM information_schema.columns
WHERE table_schema = 'public'
  AND column_name = 'workspace_id'
`)
	if err != nil {
		t.Fatalf("list workspace_id columns: %v", err)
	}
	defer workspaceColumns.Close()

	withWorkspaceID := make(map[string]struct{})
	for workspaceColumns.Next() {
		var table string
		if err := workspaceColumns.Scan(&table); err != nil {
			t.Fatalf("scan workspace_id table: %v", err)
		}
		withWorkspaceID[table] = struct{}{}
	}
	if err := workspaceColumns.Err(); err != nil {
		t.Fatalf("iterate workspace_id tables: %v", err)
	}

	for table, action := range workspaceDeletionManifest {
		_, hasWorkspaceID := withWorkspaceID[table]
		switch action {
		case workspaceDeleteKeep:
			if hasWorkspaceID {
				t.Errorf("KEEP table %s gained workspace_id; classify its teardown behavior", table)
			}
		case workspaceDeleteDetach, workspaceDeleteSettle:
			if !hasWorkspaceID {
				t.Errorf("%s table %s lost workspace_id; update its teardown selector", action, table)
			}
		}
	}
}

func TestDecisionWorkspaceDeletionRemainsWorkerOwned(test *testing.T) {
	for _, table := range []string{"decision_action", "decision_agent_scope", "decision_answer", "decision_bound_write", "decision_class_change", "decision_intent", "decision_intent_target", "decision_request", "decision_wake_event", "decision_wake_receipt"} {
		if workspaceDeletionManifest[table] != workspaceDeleteWorker {
			test.Errorf("%s must leave orphan cleanup to the decision worker", table)
		}
	}
	if workspaceDeletionManifest["decision_worker_lease"] != workspaceDeleteKeep {
		test.Error("the global worker lease is not workspace-owned")
	}
	queries, err := os.ReadFile("../../pkg/db/queries/workspace_delete.sql")
	if err != nil {
		test.Fatal(err)
	}
	// decision_wait (L5b) and decision_backing_lane (CHE-7545) are workspace-scoped with no FK, so baseline deletion owns them.
	baseline := strings.NewReplacer("decision_wait", "", "decision_backing_lane", "").Replace(string(queries))
	if strings.Contains(baseline, "decision_") {
		test.Error("baseline workspace deletion must not run a decision statement")
	}
}
