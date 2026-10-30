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
	"activity_log":                       workspaceDelete,
	"agent":                              workspaceDelete,
	"agent_builder_draft":                workspaceDelete,
	"agent_invocation_target":            workspaceDelete,
	"agent_preset":                       workspaceDelete,
	"agent_runtime":                      workspaceDelete,
	"agent_skill":                        workspaceDelete,
	"agent_task_queue":                   workspaceDelete,
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
	"chat_draft_restore":                 workspaceDelete,
	"chat_message":                       workspaceDelete,
	"chat_pinned_agent":                  workspaceDelete,
	"chat_session":                       workspaceDelete,
	"chat_wakeup":                        workspaceDelete,
	"client_usage_daily":                 workspaceDeleteDetach,
	"comment":                            workspaceDelete,
	"comment_reaction":                   workspaceDelete,
	"comment_trigger_rate_limit":         workspaceDelete,
	"contact_sales_inquiry":              workspaceDeleteKeep,
	"daemon_connection":                  workspaceDelete,
	"daemon_token":                       workspaceDelete,
	"decision_action":                    workspaceDeleteWorker,
	"decision_answer":                    workspaceDeleteWorker,
	"decision_bound_write":               workspaceDeleteWorker, // no FK; orphan cleanup deletes the row with its request.
	"decision_class_change":              workspaceDeleteWorker,
	"decision_expiry_digest":             workspaceDelete, // workspace_id FK ON DELETE CASCADE removes the day digests with the workspace.
	"decision_expiry_digest_request":     workspaceDelete, // cascades from its digest and from decision_request (worker orphan cleanup).
	"decision_intent":                    workspaceDeleteWorker,
	"decision_intent_target":             workspaceDeleteWorker,
	"decision_request":                   workspaceDeleteWorker,
	"decision_worker_lease":              workspaceDeleteKeep,
	"dingtalk_group_presence":            workspaceDelete,
	"dingtalk_bot_identity":              workspaceDelete,
	"dingtalk_group_route":               workspaceDelete,
	"feedback":                           workspaceDeleteDetach,
	"github_dispatch_halt":               workspaceDelete,
	"github_installation":                workspaceDelete,
	"github_pending_check_suite":         workspaceDelete,
	"github_route_grant":                 workspaceDelete,
	"github_pending_installation":        workspaceDeleteKeep,
	"github_pull_request":                workspaceDelete,
	"github_pull_request_check_run":      workspaceDelete,
	"github_pull_request_check_suite":    workspaceDelete,
	"github_workflow_job":                workspaceDelete, // CHE-2147 webhook-fed run/job mirror; workspace-owned leaf.
	"github_workflow_run":                workspaceDelete, // CHE-2147 webhook-fed run/job mirror; workspace-owned leaf.
	"github_webhook_intake":              workspaceDeleteKeep,
	"github_webhook_dispatch":            workspaceDelete,
	"github_webhook_revision":            workspaceDelete,
	"governed_vote_record":               workspaceDelete, // CHE-1862 vote-gate ledger; workspace-owned leaf.
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
	"issue_wakeup_pr_event":              workspaceDelete,
	"issue_ci_advance":                   workspaceDelete,
	"issue_wakeup_receipt":               workspaceDelete,
	"issue_child_event":                  workspaceDelete,
	"issue_view":                         workspaceDelete,
	"issue_view_preference":              workspaceDelete,
	"issue_dependency":                   workspaceDelete,
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
	"issue_status":                       workspaceDelete,
	"issue_workflow":                     workspaceDelete,
	"issue_subscriber":                   workspaceDelete,
	"issue_to_label":                     workspaceDelete,
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
	"member_delegation_day":              workspaceDeleteKeep, // user-scoped telemetry ledger, no workspace_id
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
	"plugin_invocation":                  workspaceDelete,
	"plugin_storage":                     workspaceDelete,
	"plugin_secret":                      workspaceDelete,
	"plugin_package":                     workspaceDelete,
	"plugin_package_version":             workspaceDelete,
	"plugin_package_file":                workspaceDelete,
	"project":                            workspaceDelete,
	"project_resource":                   workspaceDelete,
	"protocol_lint_run":                  workspaceDeleteKeep, // Cross-workspace protocol-lint audit/reporting data (CHE-552), not workspace-owned.
	"provenance_export_log":              workspaceDelete,     // CHE-755 provenance export audit rows; workspace-owned leaf.
	"provider_failure_recovery":          workspaceDeleteKeep,
	"quick_action":                       workspaceDelete,
	"run_budget_notice":                  workspaceDelete,
	"run_capability_grant":               workspaceDelete,
	"runtime_profile":                    workspaceDelete,
	"schema_migrations":                  workspaceDeleteKeep,
	"search_index_change":                workspaceDelete,
	"search_index_prune_mark":            workspaceDeleteKeep,
	"seat_capacity_outbox":               workspaceDeleteSettle,
	"skill":                              workspaceDelete,
	"skill_file":                         workspaceDelete,
	"skill_to_label":                     workspaceDelete,
	"squad":                              workspaceDelete,
	"squad_member":                       workspaceDelete,
	"stage_completion_wake":              workspaceDelete,
	"stage_generation":                   workspaceDelete,
	"sys_cron_executions":                workspaceDeleteKeep,
	"task_message":                       workspaceDelete,
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
	"user_composio_connection":           workspaceDeleteKeep,
	"user_passkey":                       workspaceDeleteKeep,
	"vcs_commit_status":                  workspaceDelete,
	"vcs_connection":                     workspaceDelete,
	"vcs_pull_request":                   workspaceDelete,
	"verification_code":                  workspaceDeleteKeep,
	"webhook_delivery":                   workspaceDelete,
	"workspace":                          workspaceDelete,
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
	for _, table := range []string{"decision_action", "decision_answer", "decision_bound_write", "decision_class_change", "decision_intent", "decision_intent_target", "decision_request"} {
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
	if strings.Contains(string(queries), "decision_") {
		test.Error("baseline workspace deletion must not run a decision statement")
	}
}
