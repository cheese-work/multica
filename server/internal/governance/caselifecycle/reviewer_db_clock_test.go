package caselifecycle

import (
	"context"
	"testing"
	"time"
)

func TestReviewerDBClockRejectsStaleApprovalDespiteSlowServer(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseHumanReview)
	fixture.setMaxRefreshes(t, ptr(int64(5)))
	var databaseNow time.Time
	if err := fixture.pool.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		t.Fatal(err)
	}
	fixture.clock.current = databaseNow.Add(-30 * time.Second)
	evaluationID := fixture.insertEvaluation(t, databaseNow, "digest-db-expired", true)
	if _, err := fixture.pool.Exec(context.Background(), `UPDATE governance_evaluation SET captured_at = clock_timestamp() - interval '61 seconds' WHERE workspace_id = $1 AND id = $2`, fixture.workspaceID, evaluationID); err != nil {
		t.Fatal(err)
	}
	var databaseAgeSeconds float64
	if err := fixture.pool.QueryRow(context.Background(), `SELECT extract(epoch FROM clock_timestamp() - captured_at)::double precision FROM governance_evaluation WHERE workspace_id = $1 AND id = $2`, fixture.workspaceID, evaluationID).Scan(&databaseAgeSeconds); err != nil {
		t.Fatal(err)
	}
	if databaseAgeSeconds <= MaxEvidenceFreshness.Seconds() {
		t.Fatalf("invalid control: database age %.3fs is not expired", databaseAgeSeconds)
	}
	result, err := fixture.service().Transition(context.Background(), TransitionCommand{
		WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID, ControlEpoch: fixture.caseRow.ControlEpoch,
		ExpectedState: CaseHumanReview, ExpectedRevision: 0, NextState: CaseCorrectionPending,
		CauseEventKey: "reviewer-db-aged-approval", Actor: ActorMember, ActorID: lifecycleUUID(t),
		Reason: ReasonHumanApproval, EvidenceEpoch: ptr(fixture.caseRow.EvidenceEpoch),
	})
	if err == nil {
		t.Fatalf("DB-expired approval accepted with service clock 30s behind: database_age=%.3fs state=%s epoch=%d", databaseAgeSeconds, result.Case.State, result.Case.EvidenceEpoch)
	}
}

func TestReviewerHumanDispositionPersistsAfterApprovalAndNextRefresh(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseHumanReview)
	fixture.setMaxRefreshes(t, ptr(int64(5)))
	fixture.insertEvaluationAge(t, 0, "digest-approved", true)
	service := fixture.service()
	approved, err := service.Transition(context.Background(), TransitionCommand{
		WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID, ControlEpoch: fixture.caseRow.ControlEpoch,
		ExpectedState: CaseHumanReview, ExpectedRevision: 0, NextState: CaseCorrectionPending,
		CauseEventKey: "reviewer-first-approval", Actor: ActorMember, ActorID: lifecycleUUID(t),
		Reason: ReasonHumanApproval, EvidenceEpoch: ptr(fixture.caseRow.EvidenceEpoch),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.ageEvidence(t, 61*time.Second)
	refreshCommand := fixture.refreshCommand()
	refreshCommand.ExpectedRevision = approved.Case.StateRevision
	refresh, err := service.BeginRefresh(context.Background(), refreshCommand)
	if err != nil {
		t.Fatal(err)
	}
	fresh := fixture.insertEvaluationAge(t, 0, "digest-after-approval", true)
	result, err := service.CompleteRefresh(context.Background(), CompleteRefreshCommand{
		WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID, ControlEpoch: fixture.caseRow.ControlEpoch,
		ExpectedRevision: refresh.Case.StateRevision, ExpectedEvidenceEpoch: fixture.caseRow.EvidenceEpoch,
		FreshEvaluationID: fresh, MaxEvidenceAge: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Case.State != string(CaseHumanReview) {
		t.Fatalf("prior human approval lost disposition requirement at replacement epoch: state=%s epoch=%d previous_epoch=%d", result.Case.State, result.Case.EvidenceEpoch, fixture.caseRow.EvidenceEpoch)
	}
}
