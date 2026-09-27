package caselifecycle

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestTransitionCompletionDedupesAfterServiceRestart(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseAgentAttempt)
	attempt := fixture.insertAgentAttempt(t)
	leaseToken := lifecycleUUID(t)
	service := fixture.service()
	if _, err := service.ClaimLease(context.Background(), LeaseCommand{
		WorkspaceID:      fixture.workspaceID,
		CaseID:           fixture.caseRow.ID,
		ExpectedState:    CaseAgentAttempt,
		ExpectedRevision: 0,
		Token:            leaseToken,
		Duration:         time.Minute,
	}); err != nil {
		t.Fatalf("claim lease: %v", err)
	}

	command := TransitionCommand{
		WorkspaceID:      fixture.workspaceID,
		CaseID:           fixture.caseRow.ID,
		ExpectedState:    CaseAgentAttempt,
		ExpectedRevision: 0,
		NextState:        CaseCorrectionPending,
		CauseEventKey:    "completion-1",
		Actor:            ActorSystem,
		Reason:           ReasonValidProposal,
		LeaseToken:       leaseToken,
		AttemptID:        attempt.ID,
		AttemptFence:     attempt.AttemptFence,
	}
	first, err := service.Transition(context.Background(), command)
	if err != nil {
		t.Fatalf("apply completion: %v", err)
	}
	if first.Case.State != string(CaseCorrectionPending) || first.Case.StateRevision != 1 {
		t.Fatalf("completion state = %s/%d, want %s/1", first.Case.State, first.Case.StateRevision, CaseCorrectionPending)
	}

	restartedService := fixture.service()
	second, err := restartedService.Transition(context.Background(), command)
	if err != nil {
		t.Fatalf("dedupe completion after restart: %v", err)
	}
	if !second.Duplicate || second.Case.StateRevision != 1 {
		t.Fatalf("duplicate result = duplicate:%t revision:%d", second.Duplicate, second.Case.StateRevision)
	}
	if second.Transition.ID != first.Transition.ID {
		t.Fatalf("duplicate transition id = %v, want original %v", second.Transition.ID, first.Transition.ID)
	}
	conflictingCommand := command
	conflictingCommand.Actor = ActorAgent
	conflictingCommand.ActorID = lifecycleUUID(t)
	if _, err := restartedService.Transition(context.Background(), conflictingCommand); !errors.Is(err, ErrInputConflict) {
		t.Fatalf("conflicting duplicate input error = %v, want ErrInputConflict", err)
	}
	if got := fixture.countTransitions(t, "completion-1"); got != 1 {
		t.Fatalf("completion transition count = %d, want 1", got)
	}
}

func TestLeaseTheftFencesStaleCompletion(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseAgentAttempt)
	attempt := fixture.insertAgentAttempt(t)
	firstToken := lifecycleUUID(t)
	secondToken := lifecycleUUID(t)
	service := fixture.service()
	claim := func(token pgtype.UUID) error {
		_, err := service.ClaimLease(context.Background(), LeaseCommand{
			WorkspaceID:      fixture.workspaceID,
			CaseID:           fixture.caseRow.ID,
			ExpectedState:    CaseAgentAttempt,
			ExpectedRevision: 0,
			Token:            token,
			Duration:         time.Minute,
		})
		return err
	}
	if err := claim(firstToken); err != nil {
		t.Fatalf("claim initial lease: %v", err)
	}
	fixture.clock.Advance(2 * time.Minute)
	if err := claim(secondToken); err != nil {
		t.Fatalf("steal expired lease: %v", err)
	}
	command := TransitionCommand{
		WorkspaceID:      fixture.workspaceID,
		CaseID:           fixture.caseRow.ID,
		ExpectedState:    CaseAgentAttempt,
		ExpectedRevision: 0,
		NextState:        CaseCorrectionPending,
		CauseEventKey:    "stale-completion",
		Actor:            ActorSystem,
		Reason:           ReasonValidProposal,
		LeaseToken:       firstToken,
		AttemptID:        attempt.ID,
		AttemptFence:     attempt.AttemptFence,
	}
	if _, err := service.Transition(context.Background(), command); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("stale lease completion error = %v, want ErrStaleFence", err)
	}
	current, err := fixture.currentCase(t)
	if err != nil {
		t.Fatalf("read case after stale completion: %v", err)
	}
	if current.State != string(CaseAgentAttempt) || current.StateRevision != 0 {
		t.Fatalf("stale completion changed case to %s/%d", current.State, current.StateRevision)
	}
	command.CauseEventKey = "current-completion"
	command.LeaseToken = secondToken
	if _, err := service.Transition(context.Background(), command); err != nil {
		t.Fatalf("current lease completion: %v", err)
	}
}

func TestMaterialSuccessorInvalidatesAndPreservesSourceCoverage(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseAgentAttempt)
	attempt := fixture.insertAgentAttempt(t)
	leaseToken := lifecycleUUID(t)
	service := fixture.service()
	if _, err := service.ClaimLease(context.Background(), LeaseCommand{
		WorkspaceID:      fixture.workspaceID,
		CaseID:           fixture.caseRow.ID,
		ExpectedState:    CaseAgentAttempt,
		ExpectedRevision: 0,
		Token:            leaseToken,
		Duration:         time.Minute,
	}); err != nil {
		t.Fatalf("claim lease: %v", err)
	}

	successorCommand := SuccessorCommand{
		WorkspaceID:      fixture.workspaceID,
		PredecessorID:    fixture.caseRow.ID,
		ExpectedState:    CaseAgentAttempt,
		ExpectedRevision: 0,
		CauseEventKey:    "source-revision-2",
		Actor:            ActorSystem,
		Successor: db.InsertNextGovernanceCaseParams{
			WorkspaceID:         fixture.workspaceID,
			SubjectType:         fixture.caseRow.SubjectType,
			SubjectID:           fixture.caseRow.SubjectID,
			SubjectRevision:     2,
			RuleID:              fixture.caseRow.RuleID,
			MaterialFingerprint: "fingerprint-b",
			State:               string(CaseCaptured),
			AuthorityLineage:    []byte("[]"),
			TriggerAliases:      []byte("[]"),
			EvidenceDigest:      "evidence-b",
			RuleRevision:        "rule-2",
			ActivationRevision:  "activation-2",
			ConfigRevision:      "config-2",
			BudgetRootID:        lifecycleUUID(t),
			FrozenStrategy:      []byte("[]"),
		},
	}
	successor, err := service.CreateSuccessor(context.Background(), successorCommand)
	if err != nil {
		t.Fatalf("create material successor: %v", err)
	}
	if !successor.HasInvalidation || successor.Case.State != string(CaseCaptured) {
		t.Fatalf("successor result has invalidation:%t state:%s", successor.HasInvalidation, successor.Case.State)
	}
	retry, err := service.CreateSuccessor(context.Background(), successorCommand)
	if err != nil {
		t.Fatalf("dedupe material successor: %v", err)
	}
	if !retry.Duplicate || retry.Case.ID != successor.Case.ID {
		t.Fatalf("material successor retry duplicate:%t id:%v, want %v", retry.Duplicate, retry.Case.ID, successor.Case.ID)
	}
	if successor.Case.PredecessorCaseID != fixture.caseRow.ID || successor.Case.BudgetRootID != fixture.caseRow.BudgetRootID ||
		successor.Case.AbsoluteDeadline != fixture.caseRow.AbsoluteDeadline ||
		!bytesEqual(successor.Case.FrozenStrategy, fixture.caseRow.FrozenStrategy) ||
		successor.Case.EvidenceEpoch != fixture.caseRow.EvidenceEpoch || successor.Case.RefreshCount != fixture.caseRow.RefreshCount {
		t.Fatal("successor did not preserve predecessor, source-obligation budget, deadline, strategy, and counters")
	}
	predecessor, err := fixture.currentCase(t)
	if err != nil {
		t.Fatalf("read invalidated predecessor: %v", err)
	}
	if predecessor.State != string(CaseInvalidated) || predecessor.StateRevision != 1 || predecessor.LeaseToken.Valid {
		t.Fatalf("predecessor after source change = %s/%d lease:%t", predecessor.State, predecessor.StateRevision, predecessor.LeaseToken.Valid)
	}
	var preservedObligationID pgtype.UUID
	if err := fixture.pool.QueryRow(context.Background(), `SELECT obligation_id FROM governance_attempt WHERE workspace_id = $1 AND case_id = $2 AND id = $3`, fixture.workspaceID, fixture.caseRow.ID, attempt.ID).Scan(&preservedObligationID); err != nil {
		t.Fatalf("read source obligation after material change: %v", err)
	}
	if preservedObligationID != attempt.ObligationID {
		t.Fatalf("source obligation after material change = %v, want %v", preservedObligationID, attempt.ObligationID)
	}
	_, err = service.Transition(context.Background(), TransitionCommand{
		WorkspaceID:      fixture.workspaceID,
		CaseID:           fixture.caseRow.ID,
		ExpectedState:    CaseAgentAttempt,
		ExpectedRevision: 0,
		NextState:        CaseCorrectionPending,
		CauseEventKey:    "late-completion-after-source-change",
		Actor:            ActorSystem,
		Reason:           ReasonValidProposal,
		LeaseToken:       leaseToken,
		AttemptID:        attempt.ID,
		AttemptFence:     attempt.AttemptFence,
	})
	if !errors.Is(err, ErrStaleCase) {
		t.Fatalf("late result after source change error = %v, want ErrStaleCase", err)
	}
	if got := fixture.countTransitions(t, "source-revision-2"); got != 1 {
		t.Fatalf("source-change transition count = %d, want 1", got)
	}
}

func TestTerminalCaseDeduplicatesSuccessorWithoutReopening(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseResolved)
	service := fixture.service()
	command := SuccessorCommand{
		WorkspaceID:      fixture.workspaceID,
		PredecessorID:    fixture.caseRow.ID,
		ExpectedState:    CaseResolved,
		ExpectedRevision: 0,
		CauseEventKey:    "resolved-source-refresh",
		Actor:            ActorSystem,
		Successor: db.InsertNextGovernanceCaseParams{
			WorkspaceID:         fixture.workspaceID,
			SubjectType:         fixture.caseRow.SubjectType,
			SubjectID:           fixture.caseRow.SubjectID,
			SubjectRevision:     2,
			RuleID:              fixture.caseRow.RuleID,
			MaterialFingerprint: "resolved-successor",
			State:               string(CaseCaptured),
			AuthorityLineage:    []byte("[]"),
			TriggerAliases:      []byte("[]"),
			EvidenceDigest:      "fresh-evidence",
			RuleRevision:        "rule-2",
			ActivationRevision:  "activation-2",
			ConfigRevision:      "config-2",
		},
	}
	first, err := service.CreateSuccessor(context.Background(), command)
	if err != nil {
		t.Fatalf("create successor from terminal case: %v", err)
	}
	if first.HasInvalidation || first.Case.State != string(CaseCaptured) {
		t.Fatalf("terminal successor result invalidated:%t state:%s", first.HasInvalidation, first.Case.State)
	}
	second, err := service.CreateSuccessor(context.Background(), command)
	if err != nil {
		t.Fatalf("dedupe terminal successor: %v", err)
	}
	if !second.Duplicate || second.Case.ID != first.Case.ID {
		t.Fatalf("duplicate successor result duplicate:%t id:%v, want %v", second.Duplicate, second.Case.ID, first.Case.ID)
	}
	predecessor, err := fixture.currentCase(t)
	if err != nil {
		t.Fatalf("read terminal predecessor: %v", err)
	}
	if predecessor.State != string(CaseResolved) || predecessor.StateRevision != 0 {
		t.Fatalf("terminal predecessor reopened as %s/%d", predecessor.State, predecessor.StateRevision)
	}
	if got := fixture.countTransitions(t, command.CauseEventKey); got != 0 {
		t.Fatalf("terminal successor recorded %d predecessor transitions, want none", got)
	}
}

func TestParkedCaseRequiresFreshSuccessorWhenReenabled(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseParked)
	service := fixture.service()
	if _, err := service.Transition(context.Background(), TransitionCommand{
		WorkspaceID:      fixture.workspaceID,
		CaseID:           fixture.caseRow.ID,
		ExpectedState:    CaseParked,
		ExpectedRevision: 0,
		NextState:        CaseEvidenceReady,
		CauseEventKey:    "resume-parked-case",
		Actor:            ActorSystem,
		Reason:           ReasonFreshEvidence,
	}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resume parked case error = %v, want ErrInvalidTransition", err)
	}

	successor, err := service.CreateSuccessor(context.Background(), SuccessorCommand{
		WorkspaceID:      fixture.workspaceID,
		PredecessorID:    fixture.caseRow.ID,
		ExpectedState:    CaseParked,
		ExpectedRevision: 0,
		CauseEventKey:    "reenabled-with-fresh-evidence",
		Actor:            ActorSystem,
		Successor: db.InsertNextGovernanceCaseParams{
			WorkspaceID:         fixture.workspaceID,
			SubjectType:         fixture.caseRow.SubjectType,
			SubjectID:           fixture.caseRow.SubjectID,
			SubjectRevision:     2,
			RuleID:              fixture.caseRow.RuleID,
			MaterialFingerprint: "reenabled-fingerprint",
			State:               string(CaseCaptured),
			AuthorityLineage:    []byte("[]"),
			TriggerAliases:      []byte("[]"),
			EvidenceDigest:      "reenabled-fresh-evidence",
			RuleRevision:        "rule-2",
			ActivationRevision:  "activation-2",
			ConfigRevision:      "config-2",
		},
	})
	if err != nil {
		t.Fatalf("create fresh successor after reenable: %v", err)
	}
	if !successor.HasInvalidation || successor.Case.State != string(CaseCaptured) || successor.Case.EvidenceDigest != "reenabled-fresh-evidence" {
		t.Fatalf("reenabled successor invalidated:%t state:%s evidence:%s", successor.HasInvalidation, successor.Case.State, successor.Case.EvidenceDigest)
	}
	predecessor, err := fixture.currentCase(t)
	if err != nil {
		t.Fatalf("read parked predecessor after reenable: %v", err)
	}
	if predecessor.State != string(CaseInvalidated) || predecessor.StateRevision != 1 {
		t.Fatalf("parked predecessor after reenable = %s/%d", predecessor.State, predecessor.StateRevision)
	}
}

func TestConcurrentInvalidationAndLeaseClaimRespectLockOrder(t *testing.T) {
	fixture := newLifecycleFixture(t, CaseEvidenceReady)
	service := fixture.service()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	errorsFound := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, err := service.ClaimLease(ctx, LeaseCommand{
			WorkspaceID:      fixture.workspaceID,
			CaseID:           fixture.caseRow.ID,
			ExpectedState:    CaseEvidenceReady,
			ExpectedRevision: 0,
			Token:            lifecycleUUID(t),
			Duration:         time.Minute,
		})
		if err != nil && !errors.Is(err, ErrStaleCase) {
			errorsFound <- err
		}
	}()
	go func() {
		defer wait.Done()
		<-start
		_, err := service.CreateSuccessor(ctx, SuccessorCommand{
			WorkspaceID:      fixture.workspaceID,
			PredecessorID:    fixture.caseRow.ID,
			ExpectedState:    CaseEvidenceReady,
			ExpectedRevision: 0,
			CauseEventKey:    "concurrent-invalidation",
			Actor:            ActorSystem,
			Successor: db.InsertNextGovernanceCaseParams{
				WorkspaceID:         fixture.workspaceID,
				SubjectType:         fixture.caseRow.SubjectType,
				SubjectID:           fixture.caseRow.SubjectID,
				SubjectRevision:     2,
				RuleID:              fixture.caseRow.RuleID,
				MaterialFingerprint: "fingerprint-race",
				State:               string(CaseCaptured),
				AuthorityLineage:    []byte("[]"),
				TriggerAliases:      []byte("[]"),
				EvidenceDigest:      "fresh-evidence",
				RuleRevision:        "rule-2",
				ActivationRevision:  "activation-2",
				ConfigRevision:      "config-2",
			},
		})
		errorsFound <- err
	}()
	close(start)
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("concurrent invalidation/claim: %v", err)
		}
	}
	predecessor, err := fixture.currentCase(t)
	if err != nil {
		t.Fatalf("read predecessor after concurrency test: %v", err)
	}
	if predecessor.State != string(CaseInvalidated) || predecessor.StateRevision != 1 || predecessor.LeaseToken.Valid {
		t.Fatalf("final predecessor = %s/%d lease:%t", predecessor.State, predecessor.StateRevision, predecessor.LeaseToken.Valid)
	}
}

func TestLifecycleImplementationHasNoNetworkOrProviderImport(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.IsDir() || strings.HasSuffix(file.Name(), "_test.go") || !strings.HasSuffix(file.Name(), ".go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s imports: %v", file.Name(), err)
		}
		for _, imported := range parsed.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("parse import path in %s: %v", file.Name(), err)
			}
			if path == "net" || strings.HasPrefix(path, "net/") ||
				strings.Contains(path, "/pkg/jev") || strings.Contains(path, "/pkg/agent") {
				t.Errorf("%s imports outbound/provider dependency %q", file.Name(), path)
			}
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Now" && selector.Sel.Name != "Since" && selector.Sel.Name != "Until" {
				return true
			}
			receiver, ok := selector.X.(*ast.Ident)
			if ok && receiver.Name == "time" {
				t.Errorf("%s uses wall-clock time.%s instead of the injected clock", file.Name(), selector.Sel.Name)
			}
			return true
		})
	}
}
