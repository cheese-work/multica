package handler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	boundField  = "workflow.steps"
	boundBefore = "1111111111111111111111111111111111111111111111111111111111111111"
	boundAfter  = "2222222222222222222222222222222222222222222222222222222222222222"
)

func allowAnyAgent(context.Context, *db.Queries, pgtype.UUID, pgtype.UUID, string) (bool, bool) {
	return true, true
}

func denyEveryAgent(context.Context, *db.Queries, pgtype.UUID, pgtype.UUID, string) (bool, bool) {
	return true, false
}

// newBoundWriteFixture builds a card whose ask names boundAfter, answers it
// with option as the workspace owner, and returns the answered request.
func newBoundWriteFixture(test *testing.T, option string) askedRecordFixture {
	test.Helper()
	fixture := newCardFixture(test)
	options := `[{"label":"` + decisiontx.ApproveWriteLabel + `","what_happens":"apply"},{"label":"Reject","what_happens":"drop"}]`
	dbfx.Exec(test, `UPDATE decision_request SET options = $2::jsonb, ask = $3 WHERE id = $1`,
		fixture.decision.ID, options, decisiontx.BoundWriteDecision(boundField, boundBefore, boundAfter))
	dbfx.Exec(test, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, fixture.issue.WorkspaceID, parseUUID(fixture.humanID))
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: option}); err != nil {
		test.Fatal(err)
	}
	return fixture
}

func (fixture askedRecordFixture) verifyBinding(gate delegatedWriteGate, afterSHA string) error {
	return verifyDecisionBinding(context.Background(), testHandler.Queries, gate, fixture.issue.WorkspaceID, fixture.decision.ID, parseUUID(fixture.agentID), boundField, boundBefore, afterSHA)
}

func boundWriteRows(test *testing.T, fixture askedRecordFixture) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM decision_bound_write WHERE decision_id = $1`, fixture.decision.ID)
}

func TestDecisionBindingRefusals(test *testing.T) {
	resetDecisionLease(test)
	for _, scenario := range []struct {
		name    string
		option  string
		mutate  func(*testing.T, askedRecordFixture)
		gate    delegatedWriteGate
		after   string
		refusal string
	}{
		{name: "human is not an owner", option: decisiontx.ApproveWriteLabel, gate: allowAnyAgent, after: boundAfter, refusal: "not a workspace owner",
			mutate: func(test *testing.T, fixture askedRecordFixture) {
				dbfx.Exec(test, `UPDATE member SET role = 'admin' WHERE workspace_id = $1 AND user_id = $2`, fixture.issue.WorkspaceID, parseUUID(fixture.humanID))
			}},
		{name: "hash is not in the ask", option: decisiontx.ApproveWriteLabel, gate: allowAnyAgent, refusal: "ask does not name",
			after: "3333333333333333333333333333333333333333333333333333333333333333"},
		{name: "answer chose another option", option: "Reject", gate: allowAnyAgent, after: boundAfter, refusal: "not the approve option"},
		{name: "decision expired", option: decisiontx.ApproveWriteLabel, gate: allowAnyAgent, after: boundAfter, refusal: "decision expired",
			mutate: func(test *testing.T, fixture askedRecordFixture) {
				dbfx.Exec(test, `UPDATE decision_answer SET created_at = clock_timestamp() - interval '8 days' WHERE decision_request_id = $1`, fixture.decision.ID)
			}},
		{name: "answer inside the veto window", option: decisiontx.ApproveWriteLabel, gate: allowAnyAgent, after: boundAfter, refusal: "not yet effective",
			mutate: func(test *testing.T, fixture askedRecordFixture) {
				dbfx.Exec(test, `UPDATE decision_answer SET effective_at = clock_timestamp() + interval '10 minutes' WHERE decision_request_id = $1`, fixture.decision.ID)
			}},
		{name: "answer vetoed", option: decisiontx.ApproveWriteLabel, gate: allowAnyAgent, after: boundAfter, refusal: "no live answer",
			mutate: func(test *testing.T, fixture askedRecordFixture) {
				dbfx.Exec(test, `UPDATE decision_answer SET disclaimed_at = clock_timestamp(), disclaim_reason = 'agent_veto' WHERE decision_request_id = $1`, fixture.decision.ID)
			}},
		{name: "agent not allowed", option: decisiontx.ApproveWriteLabel, gate: denyEveryAgent, after: boundAfter, refusal: "agent is not allowed"},
		{name: "hash is not a sha256", option: decisiontx.ApproveWriteLabel, gate: allowAnyAgent, after: "xyz", refusal: "invalid sha256"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newBoundWriteFixture(test, scenario.option)
			if scenario.mutate != nil {
				scenario.mutate(test, fixture)
			}
			err := fixture.verifyBinding(scenario.gate, scenario.after)
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || !strings.Contains(refusal.message, scenario.refusal) {
				test.Fatalf("error = %v, want refusal containing %q", err, scenario.refusal)
			}
			if rows := boundWriteRows(test, fixture); rows != 0 {
				test.Fatalf("refused write left %d audit rows", rows)
			}
		})
	}
}

func TestDecisionBindingRefusesAnotherWorkspace(test *testing.T) {
	resetDecisionLease(test)
	fixture := newBoundWriteFixture(test, decisiontx.ApproveWriteLabel)
	other := parseUUID(uuid.NewString())
	err := verifyDecisionBinding(context.Background(), testHandler.Queries, allowAnyAgent, other, fixture.decision.ID, parseUUID(fixture.agentID), boundField, boundBefore, boundAfter)
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.message, "decision request not found") {
		test.Fatalf("error = %v, want not found", err)
	}
}

func TestDecisionBindingAcceptsOnceAndRecordsAudit(test *testing.T) {
	resetDecisionLease(test)
	fixture := newBoundWriteFixture(test, decisiontx.ApproveWriteLabel)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	test.Cleanup(func() { slog.SetDefault(previous) })
	test.Cleanup(func() {
		dbfx.Exec(test, `DELETE FROM decision_bound_write WHERE decision_id = $1`, fixture.decision.ID)
	})

	if err := fixture.verifyBinding(allowAnyAgent, boundAfter); err != nil {
		test.Fatal(err)
	}
	var agentID, field, before, after string
	dbfx.QueryRow(test, `SELECT agent_id::text, field, before_sha256, after_sha256 FROM decision_bound_write WHERE decision_id = $1`, fixture.decision.ID).Scan(&agentID, &field, &before, &after)
	if agentID != fixture.agentID || field != boundField || before != boundBefore || after != boundAfter {
		test.Fatalf("audit row = %s %s %s %s", agentID, field, before, after)
	}
	for _, want := range []string{"decision-bound write accepted", "agent_id=" + fixture.agentID, "field=" + boundField, "before_sha256=" + boundBefore, "after_sha256=" + boundAfter, "decision_id=" + uuidToString(fixture.decision.ID)} {
		if !strings.Contains(logs.String(), want) {
			test.Fatalf("log lacks %q: %s", want, logs.String())
		}
	}

	err := fixture.verifyBinding(allowAnyAgent, boundAfter)
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.message, "already used") {
		test.Fatalf("reuse error = %v, want already used", err)
	}
	if rows := boundWriteRows(test, fixture); rows != 1 {
		test.Fatalf("audit rows = %d, want 1", rows)
	}
}
