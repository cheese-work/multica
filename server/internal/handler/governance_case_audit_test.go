package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/scheduler"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/featureflag"
)

type governanceCaseAuditFixture struct {
	workspaceID  string
	userID       string
	caseID       string
	evaluationID string
	secret       string
}

func newGovernanceCaseAuditFixture(t *testing.T, suffix, secret string) governanceCaseAuditFixture {
	t.Helper()
	workspaceID := dbfx.Workspace(t, "Governance Audit "+suffix, "governance-audit-"+suffix)
	userID := dbfx.User(t, "Governance Auditor "+suffix, "governance-auditor-"+suffix+"@example.test")
	dbfx.Member(t, workspaceID, userID, "admin")
	var caseID, subjectID, ruleID, budgetRootID, attemptID, evaluationID, obligationID string
	if err := testPool.QueryRow(context.Background(), `
	SELECT gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid()
	`).Scan(&caseID, &subjectID, &ruleID, &budgetRootID, &attemptID, &evaluationID, &obligationID); err != nil {
		t.Fatalf("generate governance audit fixture ids: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO governance_case (
    id, workspace_id, subject_type, subject_id, subject_revision, rule_id,
    generation, material_fingerprint, state, authority_lineage, trigger_aliases,
    evidence_digest, rule_revision, activation_revision, config_revision,
    budget_root_id, frozen_strategy, reason
) VALUES ($1, $2, 'issue', $3, 1, $4, 0, $5, 'captured', '[]'::jsonb,
          $6::jsonb, $5, 'rule-revision', 'activation-revision', 'config-revision', $7, '[]'::jsonb, $5)
`, caseID, workspaceID, subjectID, ruleID, secret, `["`+secret+`"]`, budgetRootID); err != nil {
		t.Fatalf("insert governance case fixture: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO governance_case_transition (
    workspace_id, case_id, resulting_state_revision, expected_state_revision,
    from_state, to_state, cause_event_key, actor_type, sanitized_reason
) VALUES ($1, $2, 1, 0, 'captured', 'resolved', $3, 'member', $3)
`, workspaceID, caseID, secret); err != nil {
		t.Fatalf("insert governance transition fixture: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO governance_attempt (
    id, workspace_id, case_id, ordinal, kind, obligation_id, input_digest, attempt_fence,
    confidence, result, usage
) VALUES ($1, $2, $3, 0, 'jev', $4, $5, gen_random_uuid(), $6::jsonb, $6::jsonb, $6::jsonb)
`, attemptID, workspaceID, caseID, obligationID, secret, `{"sentinel":"`+secret+`"}`); err != nil {
		t.Fatalf("insert governance attempt fixture: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO governance_evaluation (
    id, workspace_id, case_id, attempt_id, trigger_identity,
    subject_revision_vector, snapshot, snapshot_digest, snapshot_schema_version,
    required_complete, candidate_map, citation_map, estimated_tokens,
    applicable_rule_digests, question_criteria_hash, requested_model, returned_model, answers
) VALUES ($1, $2, $3, $4, 'test', '{}'::jsonb, $5::jsonb, $6, 1, true,
          $7::jsonb, $7::jsonb, 4, '[]'::jsonb, 'criteria', $6, $6, $7::jsonb)
`, evaluationID, workspaceID, caseID, attemptID, `{"sentinel":"`+secret+`"}`, secret, `["`+secret+`"]`); err != nil {
		t.Fatalf("insert governance evaluation fixture: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO governance_evaluation_source (
    workspace_id, evaluation_id, object_type, object_id, object_revision,
    object_digest, copied_context
) VALUES ($1, $2, 'comment', $3, '1', $4, $5::jsonb)
`, workspaceID, evaluationID, subjectID, secret, `{"sentinel":"`+secret+`"}`); err != nil {
		t.Fatalf("insert governance evaluation source fixture: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = testPool.Exec(ctx, `DELETE FROM governance_evaluation_source WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(ctx, `DELETE FROM governance_evaluation WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(ctx, `DELETE FROM governance_attempt WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(ctx, `DELETE FROM governance_case_transition WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(ctx, `DELETE FROM governance_case WHERE workspace_id = $1`, workspaceID)
		_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE id = $1`, workspaceID)
	})
	return governanceCaseAuditFixture{
		workspaceID: workspaceID, userID: userID, caseID: caseID,
		evaluationID: evaluationID, secret: secret,
	}
}

func governanceCaseAuditFlag(t *testing.T, enabled bool) {
	t.Helper()
	previous := testHandler.FeatureFlags
	provider := featureflag.NewStaticProvider()
	if enabled {
		provider.LoadRules(map[string]featureflag.Rule{featureflags.GovernanceCaseAudit: {Default: true}})
	}
	testHandler.FeatureFlags = featureflag.NewService(provider)
	t.Cleanup(func() { testHandler.FeatureFlags = previous })
}

func governanceCaseAuditRequest(method, path string, fixture governanceCaseAuditFixture) *http.Request {
	return testutil.WithHeaders(testutil.JSONRequest(method, path, nil),
		"X-User-ID", fixture.userID, "X-Workspace-ID", fixture.workspaceID)
}

func TestGovernanceCaseAuditIsDefaultOff(t *testing.T) {
	governanceCaseAuditFlag(t, false)
	req := testutil.WithHeaders(testutil.JSONRequest(http.MethodGet, "/api/governance/cases", nil),
		"X-User-ID", testUserID, "X-Workspace-ID", testWorkspaceID)
	testutil.Call(t, testHandler.ListGovernanceCaseAudit, req).Want(http.StatusServiceUnavailable)
}

func TestGovernanceCaseAuditRequiresWorkspaceAdmin(t *testing.T) {
	requireProvenanceDB(t)
	governanceCaseAuditFlag(t, true)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	fixture := newGovernanceCaseAuditFixture(t, suffix, "audit-auth-sentinel-"+suffix)
	memberID := dbfx.User(t, "Governance Member "+suffix, "governance-member-"+suffix+"@example.test")
	dbfx.Member(t, fixture.workspaceID, memberID, "member")
	req := testutil.WithHeaders(testutil.JSONRequest(http.MethodGet, "/api/governance/cases", nil),
		"X-User-ID", memberID, "X-Workspace-ID", fixture.workspaceID)
	testutil.Call(t, testHandler.ListGovernanceCaseAudit, req).Want(http.StatusForbidden)
}

func TestGovernanceCaseAuditScopesAndRedactsListDetailAndExport(t *testing.T) {
	requireProvenanceDB(t)
	governanceCaseAuditFlag(t, true)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	secret := "audit-secret-sentinel-" + suffix
	fixture := newGovernanceCaseAuditFixture(t, suffix, secret)
	other := newGovernanceCaseAuditFixture(t, suffix+"-foreign", "foreign-sentinel-"+suffix)
	basePath := "/api/governance/cases"

	list := testutil.Call(t, testHandler.ListGovernanceCaseAudit,
		governanceCaseAuditRequest(http.MethodGet, basePath, fixture)).Want(http.StatusOK)
	if strings.Contains(list.Body.String(), secret) || strings.Contains(list.Body.String(), other.caseID) {
		t.Fatalf("case list exposed evidence or a foreign-tenant case: %s", list.Body.String())
	}
	if !strings.Contains(list.Body.String(), fixture.caseID) {
		t.Fatalf("case list omitted workspace case %s: %s", fixture.caseID, list.Body.String())
	}

	detailPath := basePath + "/" + fixture.caseID
	detail := testutil.Call(t, testHandler.GetGovernanceCaseAudit,
		withURLParam(governanceCaseAuditRequest(http.MethodGet, detailPath, fixture), "caseId", fixture.caseID)).Want(http.StatusOK)
	if strings.Contains(detail.Body.String(), secret) || strings.Contains(detail.Body.String(), "sentinel") {
		t.Fatalf("case detail exposed private evidence: %s", detail.Body.String())
	}

	exportPath := detailPath + "/export"
	export := testutil.Call(t, testHandler.ExportGovernanceCaseAudit,
		withURLParam(governanceCaseAuditRequest(http.MethodPost, exportPath, fixture), "caseId", fixture.caseID)).Want(http.StatusOK)
	if strings.Contains(export.Body.String(), secret) || strings.Contains(export.Body.String(), "sentinel") {
		t.Fatalf("case export exposed private evidence: %s", export.Body.String())
	}

	foreignDetail := testutil.Call(t, testHandler.GetGovernanceCaseAudit,
		withURLParam(governanceCaseAuditRequest(http.MethodGet, basePath+"/"+other.caseID, fixture), "caseId", other.caseID)).Want(http.StatusNotFound)
	if strings.Contains(foreignDetail.Body.String(), other.caseID) {
		t.Fatalf("cross-workspace detail disclosed foreign case id: %s", foreignDetail.Body.String())
	}
}

func TestGovernanceCaseAuditDetailKeysetPagination(t *testing.T) {
	requireProvenanceDB(t)
	governanceCaseAuditFlag(t, true)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	fixture := newGovernanceCaseAuditFixture(t, suffix, "audit-page-sentinel-"+suffix)
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO governance_case_transition (
    workspace_id, case_id, resulting_state_revision, expected_state_revision,
    from_state, to_state, cause_event_key, actor_type, sanitized_reason, created_at
)
SELECT $1, $2, revision, revision - 1, 'captured', 'resolved',
       'audit-page-transition-' || revision, 'member', '',
       now() + revision * interval '1 second'
FROM generate_series(2, 3) AS series(revision)
`, fixture.workspaceID, fixture.caseID); err != nil {
		t.Fatalf("insert governance transition pages: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO governance_attempt (
    workspace_id, case_id, ordinal, kind, input_digest, attempt_fence, created_at
)
SELECT $1, $2, ordinal, 'jev', 'audit-page-attempt-' || ordinal,
       gen_random_uuid(), now() + ordinal * interval '1 second'
FROM generate_series(1, 3) AS series(ordinal)
`, fixture.workspaceID, fixture.caseID); err != nil {
		t.Fatalf("insert governance attempt pages: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO governance_evaluation (
    id, workspace_id, case_id, attempt_id, trigger_identity,
    subject_revision_vector, snapshot, snapshot_digest, snapshot_schema_version,
    required_complete, candidate_map, citation_map, estimated_tokens,
    applicable_rule_digests, question_criteria_hash, requested_model,
    returned_model, answers, captured_at
)
SELECT gen_random_uuid(), evaluation.workspace_id, evaluation.case_id,
       evaluation.attempt_id, evaluation.trigger_identity,
       evaluation.subject_revision_vector, evaluation.snapshot,
       evaluation.snapshot_digest, evaluation.snapshot_schema_version,
       evaluation.required_complete, evaluation.candidate_map, evaluation.citation_map,
       evaluation.estimated_tokens, evaluation.applicable_rule_digests,
       evaluation.question_criteria_hash, evaluation.requested_model,
       evaluation.returned_model, evaluation.answers,
       now() + ordinal * interval '1 second'
FROM governance_evaluation AS evaluation
CROSS JOIN generate_series(1, 3) AS series(ordinal)
WHERE evaluation.id = $1
`, fixture.evaluationID); err != nil {
		t.Fatalf("insert governance evaluation pages: %v", err)
	}

	basePath := "/api/governance/cases/" + fixture.caseID
	firstQuery := url.Values{"limit": []string{"1"}}
	firstResult := testutil.Call(t, testHandler.GetGovernanceCaseAudit,
		withURLParam(governanceCaseAuditRequest(http.MethodGet, basePath+"?"+firstQuery.Encode(), fixture), "caseId", fixture.caseID)).Want(http.StatusOK)
	var first governanceCaseAuditResponse
	if err := json.Unmarshal(firstResult.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first audit page: %v", err)
	}
	if len(first.Transitions) != 1 || len(first.Attempts) != 1 || len(first.Evaluations) != 1 ||
		first.TransitionsNextCursor == nil || first.AttemptsNextCursor == nil || first.EvaluationsNextCursor == nil {
		t.Fatalf("first page missing bounded items or continuation cursors: %+v", first)
	}

	secondQuery := url.Values{"limit": []string{"1"}}
	secondQuery.Set("transitions_before_created_at", first.TransitionsNextCursor.CreatedAt)
	secondQuery.Set("transitions_before_id", first.TransitionsNextCursor.ID)
	secondQuery.Set("attempts_before_created_at", first.AttemptsNextCursor.CreatedAt)
	secondQuery.Set("attempts_before_id", first.AttemptsNextCursor.ID)
	secondQuery.Set("evaluations_before_captured_at", first.EvaluationsNextCursor.CapturedAt)
	secondQuery.Set("evaluations_before_id", first.EvaluationsNextCursor.ID)
	secondResult := testutil.Call(t, testHandler.GetGovernanceCaseAudit,
		withURLParam(governanceCaseAuditRequest(http.MethodGet, basePath+"?"+secondQuery.Encode(), fixture), "caseId", fixture.caseID)).Want(http.StatusOK)
	var second governanceCaseAuditResponse
	if err := json.Unmarshal(secondResult.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second audit page: %v", err)
	}
	if len(second.Transitions) != 1 || len(second.Attempts) != 1 || len(second.Evaluations) != 1 {
		t.Fatalf("second page has unexpected item counts: transitions=%d attempts=%d evaluations=%d",
			len(second.Transitions), len(second.Attempts), len(second.Evaluations))
	}
	if first.Transitions[0].ID == second.Transitions[0].ID || first.Attempts[0].ID == second.Attempts[0].ID ||
		first.Evaluations[0].ID == second.Evaluations[0].ID {
		t.Fatal("keyset pagination repeated an item")
	}
}

func TestGovernanceEvidenceIsRedactedInCommentDeleteTransaction(t *testing.T) {
	requireProvenanceDB(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	fixture := newGovernanceCaseAuditFixture(t, suffix, "delete-sentinel-"+suffix)
	issueID := dbfx.Issue(t, "Governance redaction source", testutil.Cols{"workspace_id": fixture.workspaceID})
	commentID := dbfx.Comment(t, issueID, "source secret", testutil.Cols{
		"workspace_id": fixture.workspaceID,
		"author_id":    fixture.userID,
	})
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO governance_evaluation_source (
    workspace_id, evaluation_id, object_type, object_id, object_revision,
    object_digest, copied_context
) VALUES ($1, $2, 'comment', $3, '1', $4, $5::jsonb)
`, fixture.workspaceID, fixture.evaluationID, commentID, fixture.secret, `{"copied":"`+fixture.secret+`"}`); err != nil {
		t.Fatalf("insert comment source index: %v", err)
	}
	workspaceUUID, err := util.ParseUUID(fixture.workspaceID)
	if err != nil {
		t.Fatalf("parse workspace id: %v", err)
	}
	commentUUID, err := util.ParseUUID(commentID)
	if err != nil {
		t.Fatalf("parse comment id: %v", err)
	}
	if _, err := testHandler.deleteComment(context.Background(), commentUUID, workspaceUUID); err != nil {
		t.Fatalf("delete source comment: %v", err)
	}
	var snapshot, candidates, citations, answers, copiedContext, result, confidence string
	var requestedModel, returnedModel, inputDigest string
	var caseReason, evidenceDigest, triggerAliases string
	var redactedAt time.Time
	var obligationID string
	if err := testPool.QueryRow(context.Background(), `
SELECT evaluation.snapshot::text, evaluation.candidate_map::text, evaluation.citation_map::text,
       evaluation.answers::text, evaluation.redacted_at,
       evaluation.requested_model, evaluation.returned_model, attempt.input_digest,
       source.copied_context::text, attempt.result::text, attempt.confidence::text,
       attempt.obligation_id::text
FROM governance_evaluation evaluation
JOIN governance_evaluation_source source ON source.evaluation_id = evaluation.id
JOIN governance_attempt attempt ON attempt.id = evaluation.attempt_id
WHERE evaluation.id = $1 AND source.object_id = $2
`, fixture.evaluationID, commentID).Scan(&snapshot, &candidates, &citations, &answers, &redactedAt,
		&requestedModel, &returnedModel, &inputDigest, &copiedContext, &result, &confidence, &obligationID); err != nil {
		t.Fatalf("read redacted governance evidence: %v", err)
	}
	for _, value := range []string{snapshot, candidates, citations, answers, copiedContext, result, confidence} {
		if strings.Contains(value, fixture.secret) {
			t.Fatalf("redacted evidence retained source content: %s", value)
		}
	}
	if snapshot != "{}" || candidates != "[]" || citations != "[]" || answers != "[]" || copiedContext != "{}" ||
		result != "{}" || confidence != "{}" || requestedModel != "" || returnedModel != "" || inputDigest != "" {
		t.Fatalf("unexpected redaction payloads: snapshot=%s candidates=%s citations=%s answers=%s context=%s result=%s confidence=%s requested_model=%q returned_model=%q input_digest=%q",
			snapshot, candidates, citations, answers, copiedContext, result, confidence, requestedModel, returnedModel, inputDigest)
	}
	if redactedAt.IsZero() {
		t.Fatal("evaluation redaction timestamp was not set")
	}
	if obligationID == "" {
		t.Fatal("non-content obligation key was removed by source redaction")
	}
	if err := testPool.QueryRow(context.Background(), `
SELECT reason, evidence_digest, trigger_aliases::text
FROM governance_case WHERE id = $1
`, fixture.caseID).Scan(&caseReason, &evidenceDigest, &triggerAliases); err != nil {
		t.Fatalf("read redacted case metadata: %v", err)
	}
	if caseReason != "" || evidenceDigest != "" || triggerAliases != "[]" {
		t.Fatalf("source-linked case content was not redacted: reason=%q digest=%q aliases=%s", caseReason, evidenceDigest, triggerAliases)
	}
}

func TestGovernanceEvidenceIsRedactedInIssueDeleteTransaction(t *testing.T) {
	requireProvenanceDB(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	fixture := newGovernanceCaseAuditFixture(t, suffix, "issue-delete-sentinel-"+suffix)
	issueID := dbfx.Issue(t, "Governance redaction issue source", testutil.Cols{"workspace_id": fixture.workspaceID})
	commentID := dbfx.Comment(t, issueID, "source comment", testutil.Cols{
		"workspace_id": fixture.workspaceID,
		"author_id":    fixture.userID,
	})
	if _, err := testPool.Exec(context.Background(), `
INSERT INTO governance_evaluation_source (
    workspace_id, evaluation_id, object_type, object_id, object_revision,
    object_digest, copied_context
) VALUES
    ($1, $2, 'issue', $3, '1', $4, $5::jsonb),
    ($1, $2, 'comment', $6, '1', $4, $5::jsonb)
`, fixture.workspaceID, fixture.evaluationID, issueID, fixture.secret,
		`{"copied":"`+fixture.secret+`"}`, commentID); err != nil {
		t.Fatalf("insert issue and comment source indexes: %v", err)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue for delete: %v", err)
	}
	if _, err := testHandler.deleteIssueAndCollectAttachmentURLs(context.Background(), issue, nil); err != nil {
		t.Fatalf("delete governance source issue: %v", err)
	}
	var snapshot, copiedContext, result string
	var redactedSources int
	var redactedAt time.Time
	if err := testPool.QueryRow(context.Background(), `
SELECT evaluation.snapshot::text, attempt.result::text, evaluation.redacted_at,
       count(*) FILTER (WHERE source.redacted_at IS NOT NULL)::integer,
       string_agg(source.copied_context::text, '')
FROM governance_evaluation evaluation
JOIN governance_attempt attempt ON attempt.id = evaluation.attempt_id
JOIN governance_evaluation_source source ON source.evaluation_id = evaluation.id
WHERE evaluation.id = $1
GROUP BY evaluation.snapshot, attempt.result, evaluation.redacted_at
`, fixture.evaluationID).Scan(&snapshot, &result, &redactedAt, &redactedSources, &copiedContext); err != nil {
		t.Fatalf("read issue-delete redaction: %v", err)
	}
	if snapshot != "{}" || result != "{}" || redactedAt.IsZero() || redactedSources != 3 || strings.Contains(copiedContext, fixture.secret) {
		t.Fatalf("issue deletion failed to redact indexed evidence: snapshot=%s result=%s redacted_at=%v sources=%d contexts=%s",
			snapshot, result, redactedAt, redactedSources, copiedContext)
	}
}

func TestGovernanceEvidenceRetentionPreservesQualificationAndObligationKeys(t *testing.T) {
	requireProvenanceDB(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	fixture := newGovernanceCaseAuditFixture(t, suffix, "retention-sentinel-"+suffix)
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	if _, err := testPool.Exec(context.Background(), `
	UPDATE governance_evaluation SET captured_at = $2, created_at = $2 WHERE id = $1
	`, fixture.evaluationID, old); err != nil {
		t.Fatalf("age governance evidence for 30-day retention: %v", err)
	}
	job := scheduler.GovernanceCaseEvidenceRetentionJob(testHandler.Queries, testPool)
	if _, err := job.Handler(context.Background(), scheduler.HandlerInput{}); err != nil {
		t.Fatalf("run 30-day governance retention: %v", err)
	}
	var snapshot, copiedContext, result, caseReason, evidenceDigest, requestedModel, returnedModel, inputDigest string
	var redacted bool
	if err := testPool.QueryRow(context.Background(), `
SELECT evaluation.snapshot::text, source.copied_context::text, attempt.result::text,
       evaluation.requested_model, evaluation.returned_model, attempt.input_digest,
       governance_case.reason, governance_case.evidence_digest,
       evaluation.redacted_at IS NOT NULL
FROM governance_evaluation evaluation
JOIN governance_evaluation_source source ON source.evaluation_id = evaluation.id
JOIN governance_attempt attempt ON attempt.id = evaluation.attempt_id
JOIN governance_case ON governance_case.id = evaluation.case_id
WHERE evaluation.id = $1
	`, fixture.evaluationID).Scan(&snapshot, &copiedContext, &result, &requestedModel, &returnedModel, &inputDigest,
		&caseReason, &evidenceDigest, &redacted); err != nil {
		t.Fatalf("read retained governance evidence: %v", err)
	}
	if snapshot != "{}" || copiedContext != "{}" || result != "{}" || requestedModel != "" || returnedModel != "" ||
		inputDigest != "" || caseReason != "" || evidenceDigest != "" || !redacted {
		t.Fatalf("30-day content retention failed: snapshot=%s source=%s result=%s requested_model=%q returned_model=%q input_digest=%q reason=%q digest=%q redacted=%t",
			snapshot, copiedContext, result, requestedModel, returnedModel, inputDigest, caseReason, evidenceDigest, redacted)
	}
	var ruleRevision, activationRevision, configRevision, obligationID string
	if err := testPool.QueryRow(context.Background(), `
SELECT governance_case.rule_revision, governance_case.activation_revision,
       governance_case.config_revision, attempt.obligation_id::text
FROM governance_case
JOIN governance_attempt attempt ON attempt.case_id = governance_case.id
WHERE governance_case.id = $1
`, fixture.caseID).Scan(&ruleRevision, &activationRevision, &configRevision, &obligationID); err != nil {
		t.Fatalf("read retained governance references: %v", err)
	}
	if ruleRevision != "rule-revision" || activationRevision != "activation-revision" ||
		configRevision != "config-revision" || obligationID == "" {
		t.Fatalf("retention removed qualification or obligation references: rule=%q activation=%q config=%q obligation=%q",
			ruleRevision, activationRevision, configRevision, obligationID)
	}

	older := time.Now().UTC().Add(-91 * 24 * time.Hour)
	if _, err := testPool.Exec(context.Background(), `
	UPDATE governance_evaluation SET captured_at = $2 WHERE id = $1
	`, fixture.evaluationID, older); err != nil {
		t.Fatalf("age terminal evaluation metadata: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
	UPDATE governance_case_transition SET created_at = $2 WHERE case_id = $1
	`, fixture.caseID, older); err != nil {
		t.Fatalf("age terminal transition metadata: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `
	UPDATE governance_attempt
	SET created_at = $2, terminal_at = $2, terminal_reason = 'expired'
	WHERE case_id = $1
	`, fixture.caseID, older); err != nil {
		t.Fatalf("age terminal attempt metadata: %v", err)
	}
	if _, err := job.Handler(context.Background(), scheduler.HandlerInput{}); err != nil {
		t.Fatalf("run 90-day retention for active governance case: %v", err)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM governance_evaluation WHERE id = $1`, fixture.evaluationID); got != 1 {
		t.Fatalf("90-day retention removed active evaluation = %d, want 1", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM governance_evaluation_source WHERE evaluation_id = $1`, fixture.evaluationID); got != 1 {
		t.Fatalf("90-day retention removed active source index rows = %d, want 1", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM governance_case_transition WHERE case_id = $1`, fixture.caseID); got != 1 {
		t.Fatalf("90-day retention removed active case transitions = %d, want 1", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM governance_attempt WHERE case_id = $1`, fixture.caseID); got != 1 {
		t.Fatalf("90-day retention removed active attempts = %d, want 1", got)
	}
	if _, err := testPool.Exec(context.Background(), `
	UPDATE governance_case SET state = 'resolved' WHERE id = $1
	`, fixture.caseID); err != nil {
		t.Fatalf("mark governance case terminal: %v", err)
	}
	if _, err := job.Handler(context.Background(), scheduler.HandlerInput{}); err != nil {
		t.Fatalf("run 90-day retention for terminal governance case: %v", err)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM governance_evaluation WHERE id = $1`, fixture.evaluationID); got != 0 {
		t.Fatalf("expired terminal evaluations = %d, want 0", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM governance_evaluation_source WHERE evaluation_id = $1`, fixture.evaluationID); got != 0 {
		t.Fatalf("expired source index rows = %d, want 0", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM governance_case_transition WHERE case_id = $1`, fixture.caseID); got != 0 {
		t.Fatalf("expired case transitions = %d, want 0", got)
	}
	var candidateIsNull, taskIsNull bool
	if err := testPool.QueryRow(context.Background(), `
SELECT obligation_id::text, candidate_id IS NULL, task_id IS NULL,
       input_digest, result::text
FROM governance_attempt WHERE case_id = $1
	`, fixture.caseID).Scan(&obligationID, &candidateIsNull, &taskIsNull, &snapshot, &result); err != nil {
		t.Fatalf("read retained obligation record: %v", err)
	}
	if obligationID == "" || !candidateIsNull || !taskIsNull || snapshot != "" || result != "{}" {
		t.Fatalf("90-day pruning did not retain only the obligation key: obligation=%q candidate_null=%t task_null=%t digest=%q result=%s",
			obligationID, candidateIsNull, taskIsNull, snapshot, result)
	}
}
