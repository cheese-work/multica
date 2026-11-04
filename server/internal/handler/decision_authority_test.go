package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Even a forged owner answer must never turn an agent instance into human authority.
func TestAgentAuthorityCannotBindHumanWrite(t *testing.T) {
	resetDecisionLease(t)
	fixture := newBoundWriteFixture(t, decisiontx.ApproveWriteLabel)
	dbfx.Exec(t, `UPDATE decision_request SET authority = 'agents' WHERE id = $1`, fixture.decision.ID)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM decision_bound_write WHERE decision_id = $1`, fixture.decision.ID) })
	err := fixture.verifyBinding(allowAnyAgent, boundAfter)
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || !strings.Contains(refusal.message, "human") {
		t.Fatalf("agent authority binding = %v, want human-authority refusal", err)
	}
	if n := boundWriteRows(t, fixture); n != 0 {
		t.Fatalf("forged authority left %d write receipts", n)
	}
}

func TestAgentAuthorityHiddenFromHumanProjections(t *testing.T) {
	resetDecisionLease(t)
	fixture := newCardFixture(t)
	ctx := context.Background()
	params := db.ListOpenDecisionRequestsForHumanParams{WorkspaceID: fixture.issue.WorkspaceID, HumanUserID: parseUUID(fixture.humanID)}
	before, err := testHandler.Queries.ListOpenDecisionRequestsForHuman(ctx, params)
	if err != nil || len(before) == 0 {
		t.Fatalf("human control: %v, %v", before, err)
	}
	dbfx.Exec(t, `UPDATE decision_request SET authority = 'agents' WHERE id = $1`, fixture.decision.ID)
	needs, err := testHandler.Queries.ListOpenDecisionRequestsForHuman(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range needs {
		if r.ID == fixture.decision.ID {
			t.Error("agent instance appeared in Needs you")
		}
	}
	cards, err := testHandler.Queries.ListDecisionRequestCards(ctx, db.ListDecisionRequestCardsParams{WorkspaceID: fixture.issue.WorkspaceID, MessageIds: []pgtype.UUID{fixture.decision.PingMessageID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 0 {
		t.Error("agent instance appeared as a human request card")
	}
}

func TestAgentAuthorityHiddenFromRecordCards(t *testing.T) {
	resetDecisionLease(t)
	fixture := newBoundWriteFixture(t, decisiontx.ApproveWriteLabel)
	var answerID pgtype.UUID
	dbfx.QueryRow(t, `SELECT id FROM decision_answer WHERE decision_request_id = $1`, fixture.decision.ID).Scan(&answerID)
	params := db.ListDecisionCardsParams{WorkspaceID: fixture.issue.WorkspaceID, AnswerIds: []pgtype.UUID{answerID}}
	before, err := testHandler.Queries.ListDecisionCards(context.Background(), params)
	if err != nil || len(before) != 1 {
		t.Fatalf("human record control: %v, %v", before, err)
	}
	dbfx.Exec(t, `UPDATE decision_request SET authority = 'agents' WHERE id = $1`, fixture.decision.ID)
	after, err := testHandler.Queries.ListDecisionCards(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatal("agent authority projected as a human answer")
	}
}
