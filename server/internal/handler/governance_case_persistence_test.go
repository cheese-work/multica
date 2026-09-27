package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestGovernanceCasePersistence_EnforcesIdentityAndEvidenceBounds(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const slug = "handler-tests-governance-case-bounds"
	_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, slug)
	workspaceID := dbfx.Insert(t, "workspace", testutil.Cols{
		"name": "Governance case persistence bounds",
		"slug": slug,
	})
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM governance_evaluation_source WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM governance_evaluation WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM governance_attempt WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM governance_case_transition WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM governance_case WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, workspaceID)
	})

	var subjectID, ruleID, budgetRootID, caseID string
	if err := testPool.QueryRow(ctx, `SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid()`).Scan(&subjectID, &ruleID, &budgetRootID, &caseID); err != nil {
		t.Fatalf("generate governance case IDs: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO governance_case (
    id, workspace_id, subject_type, subject_id, subject_revision, rule_id,
    generation, material_fingerprint, state, budget_root_id
) VALUES ($1, $2, 'issue', $3, 1, $4, 0, 'fingerprint-a', 'captured', $5)
`, caseID, workspaceID, subjectID, ruleID, budgetRootID); err != nil {
		t.Fatalf("insert governance case: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO governance_case (
    workspace_id, subject_type, subject_id, subject_revision, rule_id,
    generation, material_fingerprint, state, budget_root_id
) VALUES ($1, 'issue', $2, 1, $3, 1, 'fingerprint-a', 'captured', $4)
`, workspaceID, subjectID, ruleID, budgetRootID); err == nil {
		t.Fatal("duplicate material fingerprint inserted another case generation")
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO governance_case (
    workspace_id, subject_type, subject_id, subject_revision, rule_id,
    generation, material_fingerprint, state, budget_root_id
) VALUES ($1, 'issue', $2, 1, $3, 1, 'fingerprint-b', 'captured', $4)
`, workspaceID, subjectID, ruleID, budgetRootID); err != nil {
		t.Fatalf("insert material-fingerprint successor: %v", err)
	}

	tooManyCandidates, err := json.Marshal(make([]map[string]string, 17))
	if err != nil {
		t.Fatalf("marshal candidate map: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO governance_evaluation (
    workspace_id, case_id, trigger_identity, subject_revision_vector, snapshot,
    snapshot_digest, snapshot_schema_version, required_complete, candidate_map,
    estimated_tokens, question_criteria_hash
) VALUES ($1, $2, 'test', '{}'::jsonb, '{}'::jsonb, 'snapshot-a', 1, true, $3, 0, 'criteria-a')
`, workspaceID, caseID, tooManyCandidates); err == nil {
		t.Fatal("17 candidates bypassed the C01 hard limit")
	}
	if _, err := testPool.Exec(ctx, `
INSERT INTO governance_evaluation (
    workspace_id, case_id, trigger_identity, subject_revision_vector, snapshot,
    snapshot_digest, snapshot_schema_version, required_complete,
    estimated_tokens, question_criteria_hash
) VALUES ($1, $2, 'test', '{}'::jsonb, '{}'::jsonb, 'snapshot-b', 1, true, 8001, 'criteria-b')
	`, workspaceID, caseID); err == nil {
		t.Fatal("8,001 estimated tokens bypassed the C01 hard limit")
	}

	// The production list query is keyset-paged by this same order. Disable a
	// sequential scan locally because this intentionally tiny fixture cannot
	// otherwise make PostgreSQL prefer the representative index plan.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("prefer index for explain: %v", err)
	}
	rows, err := tx.Query(ctx, `
EXPLAIN (ANALYZE, BUFFERS)
SELECT id
FROM governance_case
WHERE workspace_id = $1
ORDER BY created_at DESC, id DESC
LIMIT 10
`, workspaceID)
	if err != nil {
		t.Fatalf("explain governance case listing: %v", err)
	}
	defer rows.Close()
	var planLines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan governance case explain: %v", err)
		}
		planLines = append(planLines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate governance case explain: %v", err)
	}
	if plan := strings.Join(planLines, "\n"); !strings.Contains(plan, "idx_governance_case_workspace_created") {
		t.Fatalf("case listing plan did not use workspace-leading index:\n%s", plan)
	}
}

func TestDeleteWorkspace_CleansGovernanceCaseEvidence(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const targetSlug = "handler-tests-delete-governance-case"
	const neighbourSlug = "handler-tests-delete-governance-case-neighbour"
	for _, slug := range []string{targetSlug, neighbourSlug} {
		_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = $1`, slug)
	}

	newWorkspace := func(name, slug string) string {
		return dbfx.Insert(t, "workspace", testutil.Cols{"name": name, "slug": slug})
	}
	targetWorkspaceID := newWorkspace("Governance case teardown target", targetSlug)
	neighbourWorkspaceID := newWorkspace("Governance case teardown neighbour", neighbourSlug)
	t.Cleanup(func() {
		for _, workspaceID := range []string{targetWorkspaceID, neighbourWorkspaceID} {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM governance_evaluation_source WHERE workspace_id = $1`, workspaceID)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM governance_evaluation WHERE workspace_id = $1`, workspaceID)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM governance_attempt WHERE workspace_id = $1`, workspaceID)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM governance_case_transition WHERE workspace_id = $1`, workspaceID)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM governance_case WHERE workspace_id = $1`, workspaceID)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, workspaceID)
		}
	})
	dbfx.Exec(t, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`, targetWorkspaceID, testUserID)

	insertEvidence := func(workspaceID string) {
		var subjectID, ruleID, budgetRootID, caseID, evaluationID string
		if err := testPool.QueryRow(ctx, `SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid()`).Scan(&subjectID, &ruleID, &budgetRootID, &caseID, &evaluationID); err != nil {
			t.Fatalf("generate governance evidence IDs: %v", err)
		}
		dbfx.Exec(t, `
INSERT INTO governance_case (
    id, workspace_id, subject_type, subject_id, subject_revision, rule_id,
    generation, material_fingerprint, state, budget_root_id
) VALUES ($1, $2, 'issue', $3, 1, $4, 0, 'teardown-fingerprint', 'captured', $5)
`, caseID, workspaceID, subjectID, ruleID, budgetRootID)
		dbfx.Exec(t, `
INSERT INTO governance_case_transition (
    workspace_id, case_id, resulting_state_revision, expected_state_revision,
    from_state, to_state, cause_event_key, actor_type
) VALUES ($1, $2, 1, 0, 'captured', 'evidence_ready', 'teardown-event', 'system')
`, workspaceID, caseID)
		dbfx.Exec(t, `
INSERT INTO governance_attempt (
    workspace_id, case_id, ordinal, kind, input_digest, attempt_fence
) VALUES ($1, $2, 0, 'jev', 'input', gen_random_uuid())
`, workspaceID, caseID)
		dbfx.Exec(t, `
INSERT INTO governance_evaluation (
    id, workspace_id, case_id, trigger_identity, subject_revision_vector, snapshot,
    snapshot_digest, snapshot_schema_version, required_complete,
    estimated_tokens, question_criteria_hash
) VALUES ($1, $2, $3, 'teardown', '{}'::jsonb, '{}'::jsonb, 'snapshot', 1, true, 0, 'criteria')
`, evaluationID, workspaceID, caseID)
		dbfx.Exec(t, `
INSERT INTO governance_evaluation_source (
    workspace_id, evaluation_id, object_type, object_id, object_revision, object_digest
) VALUES ($1, $2, 'comment', 'source-id', '1', 'source-digest')
`, workspaceID, evaluationID)
	}
	insertEvidence(targetWorkspaceID)
	insertEvidence(neighbourWorkspaceID)

	req := newRequest(http.MethodDelete, "/api/workspaces/"+targetWorkspaceID, nil)
	req = withURLParam(req, "id", targetWorkspaceID)
	testutil.Call(t, testHandler.DeleteWorkspace, req).Want(http.StatusNoContent)

	for _, table := range []string{
		"governance_case",
		"governance_case_transition",
		"governance_attempt",
		"governance_evaluation",
		"governance_evaluation_source",
	} {
		var targetCount, neighbourCount int
		dbfx.QueryRow(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, targetWorkspaceID).Scan(&targetCount)
		dbfx.QueryRow(t, `SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, neighbourWorkspaceID).Scan(&neighbourCount)
		if targetCount != 0 {
			t.Fatalf("%s rows survived target workspace teardown: %d", table, targetCount)
		}
		if neighbourCount != 1 {
			t.Fatalf("%s neighbour rows = %d, want 1", table, neighbourCount)
		}
	}
}
