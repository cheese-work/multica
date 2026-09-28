package caselifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestStolenJevLeaseCannotComplete(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseJevEvaluating)
	attempt := insertJevAttempt(t, fixture)
	service := fixture.service()
	firstToken := lifecycleUUID(t)
	claim := LeaseCommand{WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
		ControlEpoch:  fixture.caseRow.ControlEpoch,
		ExpectedState: CaseJevEvaluating, ExpectedRevision: 0, Token: firstToken, Duration: time.Minute}
	if _, err := service.ClaimLease(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	fixture.clock.Advance(2 * time.Minute)
	secondToken := lifecycleUUID(t)
	claim.Token = secondToken
	if _, err := service.ClaimLease(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	_, err := service.Transition(context.Background(), TransitionCommand{
		WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
		ControlEpoch:  fixture.caseRow.ControlEpoch,
		ExpectedState: CaseJevEvaluating, ExpectedRevision: 0, NextState: CaseCorrectionPending,
		CauseEventKey: "stolen-jev-completion", Actor: ActorSystem, Reason: ReasonQualifiedProposal,
		LeaseToken: firstToken, AttemptID: attempt.ID, AttemptFence: attempt.AttemptFence,
	})
	if !errors.Is(err, ErrStaleFence) {
		t.Fatalf("stolen Jev lease completion error = %v, want ErrStaleFence", err)
	}
	current, err := fixture.currentCase(t)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != string(CaseJevEvaluating) || current.StateRevision != 0 || current.LeaseToken != secondToken {
		t.Fatalf("stale Jev completion changed case: state=%s revision=%d lease=%v", current.State, current.StateRevision, current.LeaseToken)
	}
}

func TestJevCompletionAcceptsCurrentLease(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseJevEvaluating)
	attempt := insertJevAttempt(t, fixture)
	leaseToken := lifecycleUUID(t)
	if _, err := fixture.service().ClaimLease(context.Background(), LeaseCommand{
		WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
		ControlEpoch:  fixture.caseRow.ControlEpoch,
		ExpectedState: CaseJevEvaluating, ExpectedRevision: 0, Token: leaseToken, Duration: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service().Transition(context.Background(), TransitionCommand{
		WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
		ControlEpoch:  fixture.caseRow.ControlEpoch,
		ExpectedState: CaseJevEvaluating, ExpectedRevision: 0, NextState: CaseCorrectionPending,
		CauseEventKey: "current-jev-completion", Actor: ActorSystem, Reason: ReasonQualifiedProposal,
		LeaseToken: leaseToken, AttemptID: attempt.ID, AttemptFence: attempt.AttemptFence,
	})
	if err != nil {
		t.Fatalf("current Jev lease completion: %v", err)
	}
	if result.Case.State != string(CaseCorrectionPending) || result.Case.StateRevision != 1 {
		t.Fatalf("current Jev completion = %s/%d, want correction_pending/1", result.Case.State, result.Case.StateRevision)
	}
}

func TestJevOutcomesRequireCurrentLease(t *testing.T) {
	outcomes := []struct {
		state  CaseState
		reason CaseReason
	}{
		{state: CaseCorrectionPending, reason: ReasonQualifiedProposal},
		{state: CaseAgentEscalation, reason: ReasonUncertainClassification},
		{state: CaseAbstained, reason: ReasonNoViolation},
	}
	for _, outcome := range outcomes {
		for _, ownership := range []string{"current", "stolen"} {
			t.Run(string(outcome.state)+"_"+ownership, func(t *testing.T) {
				fixture := newLifecycleFixture(t, CaseJevEvaluating)
				attempt := insertJevAttempt(t, fixture)
				service := fixture.service()
				firstToken := lifecycleUUID(t)
				currentToken := firstToken
				claim := LeaseCommand{WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
					ControlEpoch:  fixture.caseRow.ControlEpoch,
					ExpectedState: CaseJevEvaluating, ExpectedRevision: 0, Token: firstToken, Duration: time.Minute}
				if _, err := service.ClaimLease(context.Background(), claim); err != nil {
					t.Fatal(err)
				}
				if ownership == "stolen" {
					fixture.clock.Advance(2 * time.Minute)
					currentToken = lifecycleUUID(t)
					claim.Token = currentToken
					if _, err := service.ClaimLease(context.Background(), claim); err != nil {
						t.Fatal(err)
					}
				}
				causeEventKey := "jev-" + string(outcome.state) + "-" + ownership
				result, err := service.Transition(context.Background(), TransitionCommand{
					WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
					ControlEpoch:  fixture.caseRow.ControlEpoch,
					ExpectedState: CaseJevEvaluating, ExpectedRevision: 0, NextState: outcome.state,
					CauseEventKey: causeEventKey, Actor: ActorSystem, Reason: outcome.reason,
					LeaseToken: firstToken, AttemptID: attempt.ID, AttemptFence: attempt.AttemptFence,
				})
				if ownership == "current" {
					if err != nil || result.Case.State != string(outcome.state) || result.Case.StateRevision != 1 {
						t.Fatalf("current owner: error=%v state=%s revision=%d; want %s/1", err, result.Case.State, result.Case.StateRevision, outcome.state)
					}
					return
				}
				if !errors.Is(err, ErrStaleFence) {
					t.Fatalf("stolen owner: error=%v state=%s revision=%d; want ErrStaleFence", err, result.Case.State, result.Case.StateRevision)
				}
				current, readErr := fixture.currentCase(t)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if current.State != string(CaseJevEvaluating) || current.StateRevision != 0 || current.LeaseToken != currentToken {
					t.Errorf("stale result mutated case: state=%s revision=%d lease_preserved=%t; want jev_evaluating/0 and current lease intact", current.State, current.StateRevision, current.LeaseToken == currentToken)
				}
				if transitions := fixture.countTransitions(t, causeEventKey); transitions != 0 {
					t.Errorf("stale result wrote %d transitions; want zero", transitions)
				}
			})
		}
	}
}

func TestAttemptFenceRechecksExpiryAfterAttemptLock(t *testing.T) {
	tests := []struct {
		name          string
		leaseDuration time.Duration
		attemptExpiry time.Duration
		advance       time.Duration
	}{
		{name: "attempt deadline", leaseDuration: 10 * time.Minute, attemptExpiry: 5 * time.Minute, advance: 6 * time.Minute},
		{name: "lease expiry", leaseDuration: 5 * time.Minute, attemptExpiry: time.Hour, advance: 6 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLifecycleFixture(t, CaseAgentAttempt)
			attempt := fixture.insertAgentAttempt(t)
			if test.attemptExpiry != 5*time.Minute {
				if _, err := fixture.pool.Exec(context.Background(), `UPDATE governance_attempt SET deadline_at = $1 WHERE workspace_id = $2 AND case_id = $3 AND id = $4`,
					pgtype.Timestamptz{Time: fixture.clock.Now().Add(test.attemptExpiry), Valid: true}, fixture.workspaceID, fixture.caseRow.ID, attempt.ID); err != nil {
					t.Fatal(err)
				}
			}
			leaseToken := lifecycleUUID(t)
			if _, err := fixture.service().ClaimLease(context.Background(), LeaseCommand{
				WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
				ControlEpoch:  fixture.caseRow.ControlEpoch,
				ExpectedState: CaseAgentAttempt, ExpectedRevision: 0, Token: leaseToken, Duration: test.leaseDuration,
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			blocker, err := fixture.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			if _, err := blocker.Exec(ctx, `SELECT id FROM governance_attempt WHERE id = $1 FOR UPDATE`, attempt.ID); err != nil {
				t.Fatal(err)
			}
			clockRead := make(chan struct{})
			var signal sync.Once
			service, err := NewService(fixture.pool, ClockFunc(func() time.Time {
				captured := fixture.clock.Now()
				signal.Do(func() { close(clockRead) })
				return captured
			}))
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				err error
			}
			finished := make(chan outcome, 1)
			go func() {
				_, err := service.Transition(ctx, TransitionCommand{
					WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
					ControlEpoch:  fixture.caseRow.ControlEpoch,
					ExpectedState: CaseAgentAttempt, ExpectedRevision: 0, NextState: CaseCorrectionPending,
					CauseEventKey: "expired-after-attempt-lock-wait", Actor: ActorSystem, Reason: ReasonValidProposal,
					LeaseToken: leaseToken, AttemptID: attempt.ID, AttemptFence: attempt.AttemptFence,
				})
				finished <- outcome{err: err}
			}()
			select {
			case <-clockRead:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			fixture.clock.Advance(test.advance)
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			observed := <-finished
			if !errors.Is(observed.err, ErrStaleFence) {
				t.Fatalf("completion after expiry error = %v, want ErrStaleFence", observed.err)
			}
		})
	}
}

func TestDisabledControlEpochFencesLifecycleWrites(t *testing.T) {
	t.Run("new case admission", func(t *testing.T) {
		fixture := newLifecycleFixture(t, CaseCaptured)
		disableLifecycleControl(t, fixture)
		_, err := fixture.queries.InsertNextGovernanceCase(context.Background(), db.InsertNextGovernanceCaseParams{
			WorkspaceID: fixture.workspaceID, SubjectType: "issue", SubjectID: lifecycleUUID(t), SubjectRevision: 1,
			RuleID: lifecycleUUID(t), MaterialFingerprint: "post-disable", State: string(CaseCaptured),
			AuthorityLineage: []byte("[]"), TriggerAliases: []byte("[]"), EvidenceDigest: "evidence-post-disable",
			RuleRevision: "rule-1", ActivationRevision: "activation-1", ConfigRevision: "config-1",
			BudgetRootID: lifecycleUUID(t), FrozenStrategy: []byte("[]"), ControlEpoch: 1,
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("post-disable case admission error = %v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("action lease claim", func(t *testing.T) {
		fixture := newLifecycleFixture(t, CaseCorrectionPending)
		disableLifecycleControl(t, fixture)
		_, err := fixture.service().ClaimLease(context.Background(), LeaseCommand{
			WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
			ControlEpoch:  fixture.caseRow.ControlEpoch,
			ExpectedState: CaseCorrectionPending, ExpectedRevision: 0,
			Token: lifecycleUUID(t), Duration: time.Minute,
		})
		if !errors.Is(err, ErrStaleControlEpoch) {
			t.Fatalf("post-disable action claim error = %v, want ErrStaleControlEpoch", err)
		}
		current, err := fixture.currentCase(t)
		if err != nil {
			t.Fatal(err)
		}
		if current.LeaseToken.Valid || current.StateRevision != 0 {
			t.Fatalf("stale action claim mutated lease/revision: valid=%v revision=%d", current.LeaseToken.Valid, current.StateRevision)
		}
	})

	t.Run("queue transition", func(t *testing.T) {
		fixture := newLifecycleFixture(t, CaseCaptured)
		disableLifecycleControl(t, fixture)
		_, err := fixture.service().Transition(context.Background(), TransitionCommand{
			WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
			ControlEpoch:  fixture.caseRow.ControlEpoch,
			ExpectedState: CaseCaptured, ExpectedRevision: 0, NextState: CaseEvidenceReady,
			CauseEventKey: "post-disable-queue-transition", Actor: ActorSystem, Reason: ReasonEvidenceCaptured,
		})
		if !errors.Is(err, ErrStaleControlEpoch) {
			t.Fatalf("post-disable queue transition error = %v, want ErrStaleControlEpoch", err)
		}
		if transitions := fixture.countTransitions(t, "post-disable-queue-transition"); transitions != 0 {
			t.Fatalf("stale queue transition wrote %d rows, want zero", transitions)
		}
	})

	t.Run("correction transition", func(t *testing.T) {
		fixture := newLifecycleFixture(t, CaseJevEvaluating)
		disableLifecycleControl(t, fixture)
		_, err := fixture.service().Transition(context.Background(), TransitionCommand{
			WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
			ControlEpoch:  fixture.caseRow.ControlEpoch,
			ExpectedState: CaseJevEvaluating, ExpectedRevision: 0, NextState: CaseCorrectionPending,
			CauseEventKey: "post-disable-correction-transition", Actor: ActorSystem, Reason: ReasonQualifiedProposal,
			LeaseToken: lifecycleUUID(t), AttemptID: lifecycleUUID(t), AttemptFence: lifecycleUUID(t),
		})
		if !errors.Is(err, ErrStaleControlEpoch) {
			t.Fatalf("post-disable correction transition error = %v, want ErrStaleControlEpoch", err)
		}
		if transitions := fixture.countTransitions(t, "post-disable-correction-transition"); transitions != 0 {
			t.Fatalf("stale correction transition wrote %d rows, want zero", transitions)
		}
	})

	t.Run("successor mutation", func(t *testing.T) {
		fixture := newLifecycleFixture(t, CaseCaptured)
		disableLifecycleControl(t, fixture)
		_, err := fixture.service().CreateSuccessor(context.Background(), SuccessorCommand{
			WorkspaceID: fixture.workspaceID, PredecessorID: fixture.caseRow.ID,
			ExpectedState: CaseCaptured, ExpectedRevision: 0,
			CauseEventKey: "post-disable-successor", Actor: ActorSystem,
			Successor: db.InsertNextGovernanceCaseParams{
				WorkspaceID: fixture.workspaceID, SubjectType: fixture.caseRow.SubjectType,
				SubjectID: fixture.caseRow.SubjectID, SubjectRevision: 2, RuleID: fixture.caseRow.RuleID,
				MaterialFingerprint: "post-disable-successor", State: string(CaseCaptured),
				AuthorityLineage: []byte("[]"), TriggerAliases: []byte("[]"), EvidenceDigest: "fresh-evidence",
				RuleRevision: "rule-2", ActivationRevision: "activation-2", ConfigRevision: "config-2",
				ControlEpoch: 1,
			},
		})
		if !errors.Is(err, ErrStaleControlEpoch) {
			t.Fatalf("post-disable successor error = %v, want ErrStaleControlEpoch", err)
		}
	})
}

func TestLifecycleMutationsRejectCapturedEpochAfterControlChanges(t *testing.T) {
	tests := []struct {
		name    string
		state   CaseState
		prepare func(*testing.T, *lifecycleFixture) func() error
	}{
		{
			name:  "action claim",
			state: CaseCorrectionPending,
			prepare: func(t *testing.T, fixture *lifecycleFixture) func() error {
				return func() error {
					_, err := fixture.service().ClaimLease(context.Background(), LeaseCommand{
						WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
						ControlEpoch: fixture.caseRow.ControlEpoch, ExpectedState: CaseCorrectionPending,
						ExpectedRevision: 0, Token: lifecycleUUID(t), Duration: time.Minute,
					})
					return err
				}
			},
		},
		{
			name:  "queue mutation",
			state: CaseCaptured,
			prepare: func(_ *testing.T, fixture *lifecycleFixture) func() error {
				return func() error {
					_, err := fixture.service().Transition(context.Background(), TransitionCommand{
						WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
						ControlEpoch: fixture.caseRow.ControlEpoch, ExpectedState: CaseCaptured,
						ExpectedRevision: 0, NextState: CaseEvidenceReady,
						CauseEventKey: "stale-queue-after-control-change", Actor: ActorSystem, Reason: ReasonEvidenceCaptured,
					})
					return err
				}
			},
		},
		{
			name:  "correction mutation",
			state: CaseJevEvaluating,
			prepare: func(t *testing.T, fixture *lifecycleFixture) func() error {
				attempt := insertJevAttempt(t, fixture)
				token := lifecycleUUID(t)
				if _, err := fixture.service().ClaimLease(context.Background(), LeaseCommand{
					WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
					ControlEpoch: fixture.caseRow.ControlEpoch, ExpectedState: CaseJevEvaluating,
					ExpectedRevision: 0, Token: token, Duration: time.Minute,
				}); err != nil {
					t.Fatal(err)
				}
				return func() error {
					_, err := fixture.service().Transition(context.Background(), TransitionCommand{
						WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
						ControlEpoch: fixture.caseRow.ControlEpoch, ExpectedState: CaseJevEvaluating,
						ExpectedRevision: 0, NextState: CaseCorrectionPending,
						CauseEventKey: "stale-correction-after-control-change", Actor: ActorSystem,
						Reason: ReasonQualifiedProposal, LeaseToken: token,
						AttemptID: attempt.ID, AttemptFence: attempt.AttemptFence,
					})
					return err
				}
			},
		},
		{
			name:  "lease release",
			state: CaseCorrectionPending,
			prepare: func(t *testing.T, fixture *lifecycleFixture) func() error {
				token := lifecycleUUID(t)
				if _, err := fixture.service().ClaimLease(context.Background(), LeaseCommand{
					WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID,
					ControlEpoch: fixture.caseRow.ControlEpoch, ExpectedState: CaseCorrectionPending,
					ExpectedRevision: 0, Token: token, Duration: time.Minute,
				}); err != nil {
					t.Fatal(err)
				}
				return func() error {
					_, err := fixture.service().ReleaseLease(context.Background(), fixture.workspaceID,
						fixture.caseRow.ID, token, fixture.caseRow.ControlEpoch)
					return err
				}
			},
		},
		{
			name:  "successor insertion",
			state: CaseCaptured,
			prepare: func(_ *testing.T, fixture *lifecycleFixture) func() error {
				return func() error {
					next := newLifecycleCaseParams(fixture, "stale-successor-after-control-change", fixture.caseRow.ControlEpoch)
					_, err := fixture.service().CreateSuccessor(context.Background(), SuccessorCommand{
						WorkspaceID: fixture.workspaceID, PredecessorID: fixture.caseRow.ID,
						ExpectedState: CaseCaptured, ExpectedRevision: 0,
						CauseEventKey: "stale-successor-invalidation", Actor: ActorSystem, Successor: next,
					})
					return err
				}
			},
		},
	}

	for _, test := range tests {
		for _, controlChange := range []string{"effective config", "disable and re-enable"} {
			t.Run(test.name+" / "+controlChange, func(t *testing.T) {
				fixture := newLifecycleFixture(t, test.state)
				write := test.prepare(t, fixture)
				if controlChange == "effective config" {
					setLifecycleControl(t, fixture, true, "correction")
				} else {
					setLifecycleControl(t, fixture, false, "shadow")
					setLifecycleControl(t, fixture, true, "shadow")
				}
				before := snapshotLifecycleWrite(t, fixture)
				if err := write(); !errors.Is(err, ErrStaleControlEpoch) {
					t.Fatalf("mutation with captured epoch %d after %s = %v, want ErrStaleControlEpoch",
						fixture.caseRow.ControlEpoch, controlChange, err)
				}
				assertLifecycleWriteSnapshot(t, fixture, before)
			})
		}
	}
}

func disableLifecycleControl(t *testing.T, fixture *lifecycleFixture) {
	t.Helper()
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE governance_workspace_config
		SET config_version = config_version + 1,
		    control_epoch = control_epoch + 1,
		    settings = jsonb_set(settings, '{jev_governance_enabled}', 'false'::jsonb, true)
		WHERE workspace_id = $1
	`, fixture.workspaceID); err != nil {
		t.Fatal(err)
	}
}

func insertJevAttempt(t *testing.T, fixture *lifecycleFixture) db.GovernanceAttempt {
	t.Helper()
	attempt, err := fixture.queries.InsertGovernanceAttempt(context.Background(), db.InsertGovernanceAttemptParams{
		WorkspaceID: fixture.workspaceID, CaseID: fixture.caseRow.ID, Kind: "jev",
		ObligationID: lifecycleUUID(t), InputDigest: "input-jev", AttemptFence: lifecycleUUID(t),
		DeadlineAt: pgtype.Timestamptz{Time: fixture.clock.Now().Add(5 * time.Minute), Valid: true},
		Confidence: []byte("{}"), Result: []byte("{}"), Usage: []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `UPDATE governance_case SET current_attempt_id = $1 WHERE id = $2`, attempt.ID, fixture.caseRow.ID); err != nil {
		t.Fatal(err)
	}
	return attempt
}
