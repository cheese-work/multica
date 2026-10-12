package caselifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestReviewerRefreshHonorsAbsoluteDeadline(t *testing.T) {
	fixture, _ := expiredFixture(t)
	fixture.expireDeadline(t)
	result, err := fixture.service().BeginRefresh(context.Background(), fixture.refreshCommand())
	if err == nil && !result.Exhausted {
		t.Fatalf("refresh admitted after absolute deadline: state=%s refresh_count=%d deadline=%s now=%s", result.Case.State, result.Case.RefreshCount, result.Case.AbsoluteDeadline.Time, fixture.clock.Now())
	}
}

func TestReviewerHumanApprovalRejectsAgeExpiredSameEpoch(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseHumanReview)
	fixture.setMaxRefreshes(t, ptr(int64(5)))
	fixture.insertEvaluationAge(t, 61*time.Second, "digest-old", true)
	result, err := fixture.service().Transition(context.Background(), TransitionCommand{
		WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID, ControlEpoch: fixture.caseRow.ControlEpoch,
		ExpectedState: CaseHumanReview, ExpectedRevision: 0, NextState: CaseCorrectionPending,
		CauseEventKey: "reviewer-aged-approval", Actor: ActorMember, ActorID: lifecycleUUID(t),
		Reason: ReasonHumanApproval, EvidenceEpoch: ptr(fixture.caseRow.EvidenceEpoch),
	})
	if err == nil {
		t.Fatalf("age-expired approval accepted at unchanged evidence epoch: state=%s epoch=%d", result.Case.State, result.Case.EvidenceEpoch)
	}
}

func TestReviewerFreshnessCeilingCannotBeWidened(t *testing.T) {
	fixture, _ := expiredFixture(t)
	service := fixture.service()
	if _, err := service.BeginRefresh(context.Background(), fixture.refreshCommand()); err != nil {
		t.Fatal(err)
	}
	fresh := fixture.insertEvaluationAge(t, 0, "digest-new", true)
	fixture.ageEvidence(t, 61*time.Second) // both captures age; the replacement is now 61s old
	result, err := service.CompleteRefresh(context.Background(), CompleteRefreshCommand{
		WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID, ControlEpoch: fixture.caseRow.ControlEpoch,
		ExpectedRevision: 1, ExpectedEvidenceEpoch: fixture.caseRow.EvidenceEpoch,
		FreshEvaluationID: fresh, MaxEvidenceAge: time.Hour,
	})
	if err == nil {
		t.Fatalf("61-second-old evidence accepted under caller-supplied one-hour age: state=%s epoch=%d", result.Case.State, result.Case.EvidenceEpoch)
	}
}

func TestReviewerHumanReviewCanRequestBoundedRefresh(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseHumanReview)
	fixture.setMaxRefreshes(t, ptr(int64(5)))
	fixture.insertEvaluationAge(t, 61*time.Second, "digest-old", true)
	result, err := fixture.service().BeginRefresh(context.Background(), fixture.refreshCommand())
	if err != nil {
		t.Fatalf("age-expired human review cannot request same-case refresh: %v", err)
	}
	if result.Case.State != string(CaseRefreshing) {
		t.Fatalf("human-review refresh state=%s", result.Case.State)
	}
}

func TestReviewerRefreshReconcilesAncestorUnknownLiability(t *testing.T) {
	fixture, _ := expiredFixture(t)
	predecessor := fixture.caseRow
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO governance_budget_reservation (workspace_id, reservation_id, budget_root_id, case_id, attempt_id, obligation_id,
			resource, control_epoch, window_start, window_end, root_cap_micro_usd, window_cap_micro_usd, max_attempt_cost_micro_usd,
			retry_allowance, retry_policy_bounded, retry_allowance_remaining, total_cap_micro_usd, remaining_micro_usd, request_digest)
		SELECT workspace_id, gen_random_uuid(), budget_root_id, id, gen_random_uuid(), gen_random_uuid(), 'r', 1, now(), now() + interval '1 hour',
			10, 10, 10, 0, true, 0, 10, 10, repeat('a', 64)
		FROM governance_case WHERE workspace_id = $1 AND id = $2`, fixture.workspaceID, predecessor.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM governance_budget_reservation WHERE workspace_id = $1`, fixture.workspaceID)
	})
	successor, err := fixture.service().CreateSuccessor(context.Background(), SuccessorCommand{
		WorkspaceID: fixture.workspaceID, PredecessorID: predecessor.ID,
		ExpectedState: CaseCorrectionPending, ExpectedRevision: 0,
		CauseEventKey: "reviewer-new-pr-head", Actor: ActorSystem,
		Successor: db.InsertNextGovernanceCaseParams{
			WorkspaceID: fixture.workspaceID, ControlEpoch: 1, SubjectType: predecessor.SubjectType,
			SubjectID: predecessor.SubjectID, SubjectRevision: 2, RuleID: predecessor.RuleID,
			MaterialFingerprint: "reviewer-new-head", State: string(CaseCaptured),
			AuthorityLineage: []byte("[]"), TriggerAliases: []byte("[]"),
			BudgetRootID: lifecycleUUID(t), FrozenStrategy: []byte("[]"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.caseRow = successor.Case
	if _, err := fixture.pool.Exec(context.Background(), `UPDATE governance_case SET state = 'correction_pending' WHERE workspace_id = $1 AND id = $2`, fixture.workspaceID, fixture.caseRow.ID); err != nil {
		t.Fatal(err)
	}
	fixture.insertEvaluationAge(t, 61*time.Second, "digest-successor", true)
	result, err := fixture.service().BeginRefresh(context.Background(), fixture.refreshCommand())
	if !errors.Is(err, ErrRefreshUnreconciled) {
		t.Fatalf("refresh bypassed ancestor's reserved unknown liability under same root: error=%v state=%s root_preserved=%t", err, result.Case.State, successor.Case.BudgetRootID == predecessor.BudgetRootID)
	}
}

func TestCompleteRefreshPastAbsoluteDeadlineExhaustsToHumanOnce(t *testing.T) {
	fixture, _ := expiredFixture(t)
	service := fixture.service()
	if _, err := service.BeginRefresh(context.Background(), fixture.refreshCommand()); err != nil {
		t.Fatal(err)
	}
	fresh := fixture.insertEvaluationAge(t, 0, "digest-new", true)
	fixture.expireDeadline(t)
	complete := CompleteRefreshCommand{
		WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID, ControlEpoch: fixture.caseRow.ControlEpoch,
		ExpectedRevision: 1, ExpectedEvidenceEpoch: fixture.caseRow.EvidenceEpoch,
		FreshEvaluationID: fresh, MaxEvidenceAge: time.Minute,
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := service.CompleteRefresh(context.Background(), complete)
		if err != nil || !result.Exhausted || result.Case.State != string(CaseHumanReview) || result.Case.RefreshCount != 3 || result.Case.EvidenceEpoch != fixture.caseRow.EvidenceEpoch {
			t.Fatalf("attempt %d err:%v exhausted:%t state:%s refresh:%d epoch:%d", attempt, err, result.Exhausted, result.Case.State, result.Case.RefreshCount, result.Case.EvidenceEpoch)
		}
	}
	if count := fixture.countTransitionsLike(t, "refresh-exhausted:%"); count != 1 {
		t.Fatalf("human item transitions = %d, want 1", count)
	}
}

func TestHumanReviewRefreshReturnsToHumanReviewWithFreshEvidence(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseHumanReview)
	fixture.setMaxRefreshes(t, ptr(int64(5)))
	fixture.insertEvaluationAge(t, 61*time.Second, "digest-old", true)
	service := fixture.service()
	if _, err := service.BeginRefresh(context.Background(), fixture.refreshCommand()); err != nil {
		t.Fatal(err)
	}
	fresh := fixture.insertEvaluationAge(t, 0, "digest-new", true)
	done, err := service.CompleteRefresh(context.Background(), CompleteRefreshCommand{
		WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID, ControlEpoch: fixture.caseRow.ControlEpoch,
		ExpectedRevision: 1, ExpectedEvidenceEpoch: fixture.caseRow.EvidenceEpoch,
		FreshEvaluationID: fresh, MaxEvidenceAge: time.Minute,
	})
	if err != nil || done.Case.State != string(CaseHumanReview) || done.Case.EvidenceEpoch != fixture.caseRow.EvidenceEpoch+1 {
		t.Fatalf("human refresh err:%v state:%s epoch:%d, want human_review/epoch+1", err, done.Case.State, done.Case.EvidenceEpoch)
	}
	// The human still has to approve, at the refreshed epoch, against fresh evidence.
	approve := TransitionCommand{
		WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID, ControlEpoch: fixture.caseRow.ControlEpoch,
		ExpectedState: CaseHumanReview, ExpectedRevision: 2, NextState: CaseCorrectionPending,
		CauseEventKey: "approval-after-refresh", Actor: ActorMember, ActorID: lifecycleUUID(t),
		Reason: ReasonHumanApproval, EvidenceEpoch: ptr(done.Case.EvidenceEpoch),
	}
	if result, err := service.Transition(context.Background(), approve); err != nil || result.Case.State != string(CaseCorrectionPending) {
		t.Fatalf("approval at refreshed epoch err:%v", err)
	}
}

func TestAncestorUnfinishedAttemptBlocksRefreshAcrossLineage(t *testing.T) {
	fixture, _ := expiredFixture(t)
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO governance_case (workspace_id, subject_type, subject_id, subject_revision, rule_id, generation, material_fingerprint,
			state, budget_root_id)
		SELECT workspace_id, subject_type, subject_id, 0, rule_id, 99, 'ancestor', 'invalidated', budget_root_id
		FROM governance_case WHERE workspace_id = $1 AND id = $2`, fixture.workspaceID, fixture.caseRow.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO governance_attempt (workspace_id, case_id, ordinal, kind, input_digest, attempt_fence)
		SELECT workspace_id, id, 0, 'agent', 'in', gen_random_uuid() FROM governance_case
		WHERE workspace_id = $1 AND material_fingerprint = 'ancestor'`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service().BeginRefresh(context.Background(), fixture.refreshCommand()); !errors.Is(err, ErrRefreshUnreconciled) {
		t.Fatalf("refresh error = %v, want ErrRefreshUnreconciled", err)
	}
}
