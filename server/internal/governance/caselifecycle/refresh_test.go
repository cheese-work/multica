package caselifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const testFreshness = 60 * time.Second

func (fixture *lifecycleFixture) setMaxRefreshes(t *testing.T, limit *int64) {
	t.Helper()
	settings := `{"jev_governance_enabled":true,"rule_mode":"shadow"}`
	if limit != nil {
		settings = fmt.Sprintf(`{"jev_governance_enabled":true,"rule_mode":"shadow","limits":{"max_refreshes":%d}}`, *limit)
	}
	if _, err := fixture.pool.Exec(context.Background(),
		`UPDATE governance_workspace_config SET settings = $2::jsonb WHERE workspace_id = $1`, fixture.workspaceID, settings); err != nil {
		t.Fatalf("set max refreshes: %v", err)
	}
}

func (fixture *lifecycleFixture) insertEvaluation(t *testing.T, capturedAt time.Time, digest string, complete bool) pgtype.UUID {
	t.Helper()
	id := lifecycleUUID(t)
	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM governance_evaluation WHERE workspace_id = $1`, fixture.workspaceID)
	})
	if _, err := fixture.pool.Exec(context.Background(), `
		INSERT INTO governance_evaluation (id, workspace_id, case_id, trigger_identity, subject_revision_vector,
			snapshot, snapshot_digest, snapshot_schema_version, required_complete, question_criteria_hash,
			estimated_tokens, captured_at)
		VALUES ($1, $2, $3, 'trigger', '{}'::jsonb, '{}'::jsonb, $4, 1, $5, 'criteria', 0, $6)
	`, id, fixture.workspaceID, fixture.caseRow.ID, digest, complete, capturedAt); err != nil {
		t.Fatalf("insert evaluation: %v", err)
	}
	return id
}

func (fixture *lifecycleFixture) refreshCommand() RefreshCommand {
	return RefreshCommand{
		WorkspaceID:      fixture.workspaceID,
		CaseID:           fixture.caseRow.ID,
		ControlEpoch:     fixture.caseRow.ControlEpoch,
		ExpectedRevision: 0,
		MaxEvidenceAge:   testFreshness,
	}
}

func (fixture *lifecycleFixture) readCase(t *testing.T) db.GovernanceCase {
	t.Helper()
	row, err := fixture.queries.LockGovernanceCaseForUpdate(context.Background(), db.LockGovernanceCaseForUpdateParams{
		WorkspaceID: fixture.workspaceID, ID: fixture.caseRow.ID,
	})
	if err != nil {
		t.Fatalf("read case: %v", err)
	}
	return row
}

func ptr[T any](value T) *T { return &value }

// expiredFixture is a correction_pending case whose evidence is 61s old with
// two refreshes already consumed (fixture default) and a limit of five.
func expiredFixture(t *testing.T) (*lifecycleFixture, pgtype.UUID) {
	fixture := newLifecycleFixture(t, CaseCorrectionPending)
	fixture.setMaxRefreshes(t, ptr(int64(5)))
	evaluation := fixture.insertEvaluation(t, fixture.clock.Now().Add(-61*time.Second), "digest-old", true)
	return fixture, evaluation
}

func TestAgeOnlyExpiryRefreshesOnceAndPreservesLineage(t *testing.T) {
	fixture, _ := expiredFixture(t)
	service := fixture.service()
	command := fixture.refreshCommand()

	first, err := service.BeginRefresh(context.Background(), command)
	if err != nil {
		t.Fatalf("begin refresh: %v", err)
	}
	if first.Duplicate || first.Case.State != string(CaseRefreshing) || first.Case.RefreshCount != 3 {
		t.Fatalf("first refresh duplicate:%t state:%s refresh_count:%d", first.Duplicate, first.Case.State, first.Case.RefreshCount)
	}
	// Duplicate sweeps, even after restart (new service), consume nothing.
	for _, sweeper := range []*Service{service, fixture.service()} {
		again, err := sweeper.BeginRefresh(context.Background(), command)
		if err != nil || !again.Duplicate || again.Case.RefreshCount != 3 {
			t.Fatalf("duplicate sweep err:%v duplicate:%t refresh_count:%d", err, again.Duplicate, again.Case.RefreshCount)
		}
	}
	got := fixture.readCase(t)
	if got.BudgetRootID != fixture.caseRow.BudgetRootID || got.AbsoluteDeadline != fixture.caseRow.AbsoluteDeadline ||
		!bytesEqual(got.FrozenStrategy, fixture.caseRow.FrozenStrategy) || got.EvidenceEpoch != fixture.caseRow.EvidenceEpoch ||
		got.StateRevision != 1 || got.ID != fixture.caseRow.ID {
		t.Fatalf("refresh did not preserve root/deadline/strategy/epoch/revision: %+v", got)
	}
	if count := fixture.countTransitionsLike(t, "refresh-expiry:%"); count != 1 {
		t.Fatalf("expiry transitions = %d, want 1", count)
	}
}

func TestFreshEvidenceIsNotRefreshed(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseCorrectionPending)
	fixture.setMaxRefreshes(t, ptr(int64(5)))
	fixture.insertEvaluation(t, fixture.clock.Now().Add(-59*time.Second), "digest-recent", true)
	// An event arriving now must not make 59s-old evidence expire or fresh.
	_, err := fixture.service().BeginRefresh(context.Background(), fixture.refreshCommand())
	if !errors.Is(err, ErrEvidenceFresh) {
		t.Fatalf("fresh evidence refresh error = %v, want ErrEvidenceFresh", err)
	}
	if got := fixture.readCase(t); got.State != string(CaseCorrectionPending) || got.RefreshCount != 2 {
		t.Fatalf("fresh case mutated: %s/%d", got.State, got.RefreshCount)
	}
}

func TestRefreshBlockedByUnreconciledWork(t *testing.T) {
	cases := map[string]string{
		"active attempt": `INSERT INTO governance_attempt (workspace_id, case_id, ordinal, kind, input_digest, attempt_fence, confidence, result, usage)
			VALUES ($1, $2, 0, 'agent', 'in', gen_random_uuid(), '{}', '{}', '{}')`,
		"uncertain reservation": `INSERT INTO governance_budget_reservation (workspace_id, reservation_id, budget_root_id, case_id, attempt_id, obligation_id,
				resource, control_epoch, window_start, window_end, root_cap_micro_usd, window_cap_micro_usd, max_attempt_cost_micro_usd,
				retry_allowance, retry_policy_bounded, retry_allowance_remaining,
				total_cap_micro_usd, remaining_micro_usd, request_digest)
			SELECT $1, gen_random_uuid(), budget_root_id, id, gen_random_uuid(), gen_random_uuid(), 'r', 1, now(), now() + interval '1 hour',
				10, 10, 10, 0, true, 0, 10, 10, repeat('a', 64)
			FROM governance_case WHERE workspace_id = $1 AND id = $2`,
	}
	for name, statement := range cases {
		t.Run(name, func(t *testing.T) {
			fixture, _ := expiredFixture(t)
			if _, err := fixture.pool.Exec(context.Background(), statement, fixture.workspaceID, fixture.caseRow.ID); err != nil {
				t.Fatalf("seed unreconciled work: %v", err)
			}
			t.Cleanup(func() {
				_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM governance_budget_reservation WHERE workspace_id = $1`, fixture.workspaceID)
			})
			_, err := fixture.service().BeginRefresh(context.Background(), fixture.refreshCommand())
			if !errors.Is(err, ErrRefreshUnreconciled) {
				t.Fatalf("refresh error = %v, want ErrRefreshUnreconciled", err)
			}
			if got := fixture.readCase(t); got.State != string(CaseCorrectionPending) || got.RefreshCount != 2 {
				t.Fatalf("blocked refresh mutated case: %s/%d", got.State, got.RefreshCount)
			}
		})
	}
}

func TestFiniteRefreshLimitExhaustsOnceToHumanReview(t *testing.T) {
	for _, limit := range []int64{0, 2} { // 0 = zero configured; 2 = already consumed (fixture refresh_count 2)
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			fixture, _ := expiredFixture(t)
			fixture.setMaxRefreshes(t, ptr(limit))
			service := fixture.service()
			first, err := service.BeginRefresh(context.Background(), fixture.refreshCommand())
			if err != nil || !first.Exhausted || first.Case.State != string(CaseHumanReview) || first.Case.RefreshCount != 2 {
				t.Fatalf("exhaust err:%v exhausted:%t state:%s refresh_count:%d", err, first.Exhausted, first.Case.State, first.Case.RefreshCount)
			}
			again, err := fixture.service().BeginRefresh(context.Background(), fixture.refreshCommand())
			if err != nil || !again.Duplicate || !again.Exhausted || again.Case.State != string(CaseHumanReview) {
				t.Fatalf("repeat exhaust err:%v duplicate:%t exhausted:%t state:%s", err, again.Duplicate, again.Exhausted, again.Case.State)
			}
			if count := fixture.countTransitionsLike(t, "refresh-exhausted:%"); count != 1 {
				t.Fatalf("human item transitions = %d, want 1", count)
			}
		})
	}
}

func TestMissingRefreshLimitParksCase(t *testing.T) {
	fixture, _ := expiredFixture(t)
	fixture.setMaxRefreshes(t, nil)
	result, err := fixture.service().BeginRefresh(context.Background(), fixture.refreshCommand())
	if err != nil || result.Case.State != string(CaseParked) || result.Case.Reason != string(ReasonConfigMissing) {
		t.Fatalf("missing limit err:%v state:%s reason:%s", err, result.Case.State, result.Case.Reason)
	}
}

func TestDisabledOrStaleControlRejectsRefresh(t *testing.T) {
	fixture, _ := expiredFixture(t)
	command := fixture.refreshCommand()
	command.ControlEpoch = 2
	if _, err := fixture.service().BeginRefresh(context.Background(), command); !errors.Is(err, ErrStaleControlEpoch) {
		t.Fatalf("stale epoch error = %v, want ErrStaleControlEpoch", err)
	}
	if _, err := fixture.pool.Exec(context.Background(),
		`UPDATE governance_workspace_config SET settings = settings || '{"jev_governance_enabled":false}'::jsonb WHERE workspace_id = $1`, fixture.workspaceID); err != nil {
		t.Fatalf("disable governance: %v", err)
	}
	if _, err := fixture.service().BeginRefresh(context.Background(), fixture.refreshCommand()); !errors.Is(err, ErrStaleControlEpoch) {
		t.Fatalf("disabled gate error = %v, want ErrStaleControlEpoch", err)
	}
	if got := fixture.readCase(t); got.State != string(CaseCorrectionPending) || got.RefreshCount != 2 {
		t.Fatalf("rejected refresh mutated case: %s/%d", got.State, got.RefreshCount)
	}
}

func TestCompleteRefreshRequiresRebuiltEvidence(t *testing.T) {
	fixture, expired := expiredFixture(t)
	service := fixture.service()
	if _, err := service.BeginRefresh(context.Background(), fixture.refreshCommand()); err != nil {
		t.Fatalf("begin refresh: %v", err)
	}
	complete := CompleteRefreshCommand{
		WorkspaceID:           fixture.workspaceID,
		CaseID:                fixture.caseRow.ID,
		ControlEpoch:          fixture.caseRow.ControlEpoch,
		ExpectedRevision:      1,
		ExpectedEvidenceEpoch: fixture.caseRow.EvidenceEpoch,
		MaxEvidenceAge:        testFreshness,
	}
	rejected := map[string]func(*CompleteRefreshCommand){
		"old evaluation retimestamped as current": func(c *CompleteRefreshCommand) { c.FreshEvaluationID = expired },
		"stale evidence epoch": func(c *CompleteRefreshCommand) {
			c.FreshEvaluationID = fixture.insertEvaluation(t, fixture.clock.Now(), "digest-new", true)
			c.ExpectedEvidenceEpoch--
		},
		"incomplete required observation": func(c *CompleteRefreshCommand) {
			c.FreshEvaluationID = fixture.insertEvaluation(t, fixture.clock.Now(), "digest-partial", false)
		},
		"capture older than the expired evidence": func(c *CompleteRefreshCommand) {
			c.FreshEvaluationID = fixture.insertEvaluation(t, fixture.clock.Now().Add(-120*time.Second), "digest-older", true)
		},
		"already expired capture": func(c *CompleteRefreshCommand) {
			c.FreshEvaluationID = fixture.insertEvaluation(t, fixture.clock.Now().Add(-60*time.Second-time.Millisecond), "digest-late", true)
		},
		"unknown evaluation": func(c *CompleteRefreshCommand) { c.FreshEvaluationID = lifecycleUUID(t) },
	}
	for name, mutate := range rejected {
		attempt := complete
		mutate(&attempt)
		if _, err := service.CompleteRefresh(context.Background(), attempt); !errors.Is(err, ErrRefreshEvidenceRejected) && !errors.Is(err, ErrStaleEvidenceEpoch) {
			t.Fatalf("%s: error = %v, want rejection", name, err)
		}
		if got := fixture.readCase(t); got.State != string(CaseRefreshing) || got.EvidenceEpoch != fixture.caseRow.EvidenceEpoch {
			t.Fatalf("%s: mutated case %s/%d", name, got.State, got.EvidenceEpoch)
		}
	}

	fresh := fixture.insertEvaluation(t, fixture.clock.Now(), "digest-new", true)
	complete.FreshEvaluationID = fresh
	done, err := service.CompleteRefresh(context.Background(), complete)
	if err != nil {
		t.Fatalf("complete refresh: %v", err)
	}
	if done.Case.State != string(CaseEvidenceReady) || done.Case.EvidenceEpoch != fixture.caseRow.EvidenceEpoch+1 ||
		done.Case.EvidenceDigest != "digest-new" || done.Case.EvidenceID != fresh || done.Case.RefreshCount != 3 ||
		done.Case.BudgetRootID != fixture.caseRow.BudgetRootID || done.Case.AbsoluteDeadline != fixture.caseRow.AbsoluteDeadline {
		t.Fatalf("completed refresh = %+v", done.Case)
	}
	retry, err := fixture.service().CompleteRefresh(context.Background(), complete)
	if err != nil || !retry.Duplicate || retry.Case.EvidenceEpoch != done.Case.EvidenceEpoch {
		t.Fatalf("duplicate completion err:%v duplicate:%t epoch:%d", err, retry.Duplicate, retry.Case.EvidenceEpoch)
	}
}

func TestHumanApprovalRequiresCurrentEvidenceEpoch(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseHumanReview)
	fixture.insertEvaluation(t, fixture.clock.Now(), "digest-current", true)
	approve := TransitionCommand{
		WorkspaceID:      fixture.workspaceID,
		CaseID:           fixture.caseRow.ID,
		ControlEpoch:     fixture.caseRow.ControlEpoch,
		ExpectedState:    CaseHumanReview,
		ExpectedRevision: 0,
		NextState:        CaseCorrectionPending,
		CauseEventKey:    "approval-1",
		Actor:            ActorMember,
		ActorID:          lifecycleUUID(t),
		Reason:           ReasonHumanApproval,
	}
	service := fixture.service()
	if _, err := service.Transition(context.Background(), approve); !errors.Is(err, ErrStaleEvidenceEpoch) {
		t.Fatalf("approval without epoch error = %v, want ErrStaleEvidenceEpoch", err)
	}
	approve.EvidenceEpoch = ptr(fixture.caseRow.EvidenceEpoch - 1)
	if _, err := service.Transition(context.Background(), approve); !errors.Is(err, ErrStaleEvidenceEpoch) {
		t.Fatalf("stale approval error = %v, want ErrStaleEvidenceEpoch", err)
	}
	approve.EvidenceEpoch = ptr(fixture.caseRow.EvidenceEpoch)
	if result, err := service.Transition(context.Background(), approve); err != nil || result.Case.State != string(CaseCorrectionPending) {
		t.Fatalf("current approval err:%v state:%s", err, result.Case.State)
	}
}

func TestDuplicateMaterialEventDoesNotRefreshIdentityOrAllowances(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseCorrectionPending)
	service := fixture.service()
	command := SuccessorCommand{
		WorkspaceID:      fixture.workspaceID,
		PredecessorID:    fixture.caseRow.ID,
		ExpectedState:    CaseCorrectionPending,
		ExpectedRevision: 0,
		CauseEventKey:    "duplicate-pr-status-delivery",
		Actor:            ActorSystem,
		Successor: db.InsertNextGovernanceCaseParams{
			WorkspaceID:         fixture.workspaceID,
			ControlEpoch:        1,
			SubjectType:         fixture.caseRow.SubjectType,
			SubjectID:           fixture.caseRow.SubjectID,
			SubjectRevision:     fixture.caseRow.SubjectRevision,
			RuleID:              fixture.caseRow.RuleID,
			MaterialFingerprint: fixture.caseRow.MaterialFingerprint, // same facts, new delivery/arrival time
			State:               string(CaseCaptured),
			AuthorityLineage:    []byte("[]"),
			TriggerAliases:      []byte("[]"),
			BudgetRootID:        lifecycleUUID(t),
			FrozenStrategy:      []byte("[]"),
		},
	}
	result, err := service.CreateSuccessor(context.Background(), command)
	if err != nil || !result.Duplicate || result.HasInvalidation || result.Case.ID != fixture.caseRow.ID {
		t.Fatalf("duplicate event err:%v duplicate:%t invalidated:%t id match:%t", err, result.Duplicate, result.HasInvalidation, result.Case.ID == fixture.caseRow.ID)
	}
	if got := fixture.readCase(t); got.State != string(CaseCorrectionPending) || got.StateRevision != 0 ||
		got.RefreshCount != 2 || got.EvidenceEpoch != 3 {
		t.Fatalf("duplicate event mutated case: %s rev:%d refresh:%d epoch:%d", got.State, got.StateRevision, got.RefreshCount, got.EvidenceEpoch)
	}
}

func TestMaterialSuccessorAfterRefreshKeepsConsumedCounters(t *testing.T) {
	fixture, _ := expiredFixture(t)
	service := fixture.service()
	if _, err := service.BeginRefresh(context.Background(), fixture.refreshCommand()); err != nil {
		t.Fatalf("begin refresh: %v", err)
	}
	result, err := service.CreateSuccessor(context.Background(), SuccessorCommand{
		WorkspaceID:      fixture.workspaceID,
		PredecessorID:    fixture.caseRow.ID,
		ExpectedState:    CaseRefreshing,
		ExpectedRevision: 1,
		CauseEventKey:    "pr-head-changed",
		Actor:            ActorSystem,
		Successor: db.InsertNextGovernanceCaseParams{
			WorkspaceID:         fixture.workspaceID,
			ControlEpoch:        1,
			SubjectType:         fixture.caseRow.SubjectType,
			SubjectID:           fixture.caseRow.SubjectID,
			SubjectRevision:     2,
			RuleID:              fixture.caseRow.RuleID,
			MaterialFingerprint: "fingerprint-new-head",
			State:               string(CaseCaptured),
			AuthorityLineage:    []byte("[]"),
			TriggerAliases:      []byte("[]"),
			BudgetRootID:        lifecycleUUID(t),
			FrozenStrategy:      []byte("[]"),
		},
	})
	if err != nil {
		t.Fatalf("material successor: %v", err)
	}
	if result.Case.RefreshCount != 3 || result.Case.EvidenceEpoch != fixture.caseRow.EvidenceEpoch ||
		result.Case.BudgetRootID != fixture.caseRow.BudgetRootID || result.Case.PredecessorCaseID != fixture.caseRow.ID {
		t.Fatalf("successor lost lineage: refresh:%d epoch:%d", result.Case.RefreshCount, result.Case.EvidenceEpoch)
	}
}

func (fixture *lifecycleFixture) countTransitionsLike(t *testing.T, pattern string) int {
	t.Helper()
	var count int
	if err := fixture.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM governance_case_transition WHERE workspace_id = $1 AND cause_event_key LIKE $2`, fixture.workspaceID, pattern).Scan(&count); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	return count
}

func TestConcurrentExpirySweepsConsumeOneRefresh(t *testing.T) {
	fixture, _ := expiredFixture(t)
	const sweepers = 6
	results := make(chan RefreshResult, sweepers)
	errs := make(chan error, sweepers)
	var wg sync.WaitGroup
	for range sweepers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := fixture.service().BeginRefresh(context.Background(), fixture.refreshCommand())
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent sweep: %v", err)
		}
	}
	firsts := 0
	for result := range results {
		if !result.Duplicate {
			firsts++
		}
	}
	if got := fixture.readCase(t); firsts != 1 || got.RefreshCount != 3 || got.StateRevision != 1 {
		t.Fatalf("firsts:%d refresh_count:%d revision:%d, want 1/3/1", firsts, got.RefreshCount, got.StateRevision)
	}
}
