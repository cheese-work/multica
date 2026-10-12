package migrations

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestMigrationNumericPrefixesAreUnique(t *testing.T) {
	files := migrationFilesForLint(t, "*.up.sql")

	const firstUniqueMigrationNumber = 129
	allowedForkCollisions := map[string]string{
		"500_protocol_lint_run":                                 "500_task_message_call_id",
		"501_protocol_lint_run_checked_at_idx":                  "501_runtime_profile_runtime_type",
		"502_channel_reply_delivery":                            "502_issue_checkpoint",
		"503_channel_reply_delivery_turn_index":                 "503_issue_checkpoint_owner_uidx",
		"504_channel_reply_delivery_installation_index":         "504_github_merge_announcement",
		"505_channel_reply_delivery_binding_index":              "505_github_merge_announcement_identity_uidx",
		"506_channel_reply_delivery_attempt_depth":              "506_github_merge_announcement_pending_idx",
		"509_issue_wakeup":                                      "509_stage_completion_wake",
		"510_stage_completion_wake_unique":                      "510_wakeup_id",
		"511_stage_generation_workspace_index":                  "511_wakeup_issue",
		"512_stage_completion_wake_workspace_index":             "512_wakeup_due",
		"513_governance_receipt":                                "513_wakeup_receipt_id",
		"514_governance_receipt_comment_idx":                    "514_wakeup_receipt_key",
		"515_provenance_export_log":                             "515_wakeup_receipt_pending",
		"516_provenance_export_log_workspace_idx":               "516_wakeup_pending_scope",
		"518_governance_case_persistence":                       "518_wakeup_event_capture",
		"519_governance_case_identity_uidx":                     "519_wakeup_event_issue",
		"520_collaboration_wakeup_events":                       "520_governance_case_material_fingerprint_uidx",
		"521_governance_case_workspace_created_idx":             "521_wakeup_workspace_summary",
		"522_governance_case_transition_identity_uidx":          "522_wakeup_run_lookup",
		"523_governance_case_transition_cause_uidx":             "523_wakeup_registration_source",
		"524_governance_case_transition_workspace_created_idx":  "524_wakeup_workspace_history",
		"525_governance_attempt_identity_uidx":                  "525_wakeup_active_runs",
		"526_governance_attempt_active_agent_uidx":              "526_wakeup_terminal_runs",
		"527_governance_attempt_workspace_deadline_idx":         "527_wakeup_receipt_expiry",
		"528_governance_evaluation_workspace_captured_idx":      "528_wakeup_receipt_coalescing",
		"529_governance_evaluation_source_identity_uidx":        "529_wakeup_pending_event",
		"530_governance_evaluation_source_workspace_object_idx": "530_wakeup_bounded_capture",
		"531_governance_evidence_redaction":                     "531_wakeup_actor_filter",
		"532_governance_attempt_workspace_case_created_idx":     "532_wakeup_actor_capture",
		"533_governance_attempt_workspace_created_idx":          "533_wakeup_close_in_app",
		"534_drop_comment_agent_delivery":                       "534_governance_evaluation_workspace_case_captured_idx",
		"535_github_pr_address_index":                           "535_governance_case_transition_workspace_case_created_idx",
		"536_governance_receipt_workspace_comment_created_idx":  "536_issue_duplicate_of",
		"548_governance_workspace_control":                      "548_task_supplement_comment_task_index",
		"549_governance_jev_credential":                         "549_task_supplement_comment_task_primary_key",
		"550_comment_suppressed_agents":                         "550_governance_proposal_task_tokens",
	}
	stemByNumber := make(map[int]string)
	for _, file := range files {
		stem, _, ok := splitMigrationFilename(filepath.Base(file))
		if !ok {
			continue
		}
		prefix, _, ok := strings.Cut(stem, "_")
		if !ok {
			continue
		}
		number, err := strconv.Atoi(prefix)
		if err != nil || number < firstUniqueMigrationNumber {
			continue
		}
		if previous, exists := stemByNumber[number]; exists {
			if allowedForkCollisions[previous] == stem {
				continue
			}
			t.Errorf("migrations %s and %s share numeric prefix %s", previous, stem, prefix)
			continue
		}
		stemByNumber[number] = stem
	}
}

func TestMigrationFilesHaveMatchingDirections(t *testing.T) {
	files := migrationFilesForLint(t, "*.sql")

	directionsByStem := make(map[string]map[string]bool)
	for _, file := range files {
		stem, direction, ok := splitMigrationFilename(filepath.Base(file))
		if !ok {
			continue
		}
		if directionsByStem[stem] == nil {
			directionsByStem[stem] = make(map[string]bool)
		}
		directionsByStem[stem][direction] = true
	}

	for stem, directions := range directionsByStem {
		if !directions["up"] || !directions["down"] {
			t.Errorf("migration %s must have both .up.sql and .down.sql files", stem)
		}
	}
}

func migrationFilesForLint(t *testing.T, pattern string) []string {
	t.Helper()

	dir := realMigrationsDir(t)
	files, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no migration files matched %s in %s", pattern, dir)
	}
	sort.Strings(files)
	return files
}

func realMigrationsDir(t *testing.T) string {
	t.Helper()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve migration lint test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(self), "..", "..", "migrations"))
}

func splitMigrationFilename(name string) (stem, direction string, ok bool) {
	for _, candidateDirection := range []string{"up", "down"} {
		suffix := fmt.Sprintf(".%s.sql", candidateDirection)
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix), candidateDirection, true
		}
	}
	return "", "", false
}
