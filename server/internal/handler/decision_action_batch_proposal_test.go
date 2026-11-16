package handler

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
)

const batchProposalPath = proposalPath + "/batch"

type batchView struct {
	Batches []struct {
		BatchID    string         `json:"batch_id"`
		ActionType string         `json:"action_type"`
		Replayed   bool           `json:"replayed"`
		Requests   []proposalView `json:"requests"`
	} `json:"batches"`
}

// batchItem is one copy proposal on its own issue.
func (fixture dmProposalFixture) copyItem(test *testing.T, title string) map[string]any {
	test.Helper()
	item := fixture.proposal(test)
	delete(item, "for")
	item["issue_id"] = dbfx.Issue(test, title, testutil.Cols{"workspace_id": testWorkspaceID})
	dbfx.Cleanup(test, `DELETE FROM inbox_item WHERE issue_id = $1`, item["issue_id"])
	dbfx.Cleanup(test, `DELETE FROM decision_request WHERE issue_id = $1`, item["issue_id"])
	return item
}

func (fixture dmProposalFixture) pauseItem(test *testing.T) map[string]any {
	test.Helper()
	item := fixture.copyItem(test, "batch pause")
	item["action_type"] = "agent.pause"
	item["params"] = map[string]any{"agent_id": fixture.agentID}
	return item
}

func (fixture dmProposalFixture) batchBody(items ...map[string]any) map[string]any {
	return map[string]any{"goal": "Batch goal " + uuid.NewString()[:8], "for": fixture.humanID, "items": items}
}

func (fixture dmProposalFixture) batchPropose(test *testing.T, body map[string]any, status int) batchView {
	test.Helper()
	request := newRequest(http.MethodPost, batchProposalPath, body)
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.chatTaskID)
	var view batchView
	testutil.Call(test, testHandler.ProposeDecisionActionBatch, request).Want(status).JSON(&view)
	return view
}

func (fixture dmProposalFixture) cards(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'decision_request'`, fixture.sessionID)
}

func (fixture dmProposalFixture) batchRows(test *testing.T, batchID string) (requests, actions, fingerprints int) {
	test.Helper()
	requests = dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE batch_id = $1`, batchID)
	actions = dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE decision_request_id IN (SELECT id FROM decision_request WHERE batch_id = $1)`, batchID)
	fingerprints = dbfx.Count(test, `SELECT count(DISTINCT proposal_fingerprint) FROM decision_request WHERE batch_id = $1`, batchID)
	return requests, actions, fingerprints
}

func TestBatchProposalOfThreeIssuesOpensOneCard(test *testing.T) {
	fixture := newDMProposalFixture(test)
	body := fixture.batchBody(fixture.copyItem(test, "batch a"), fixture.copyItem(test, "batch b"), fixture.copyItem(test, "batch c"))
	view := fixture.batchPropose(test, body, http.StatusCreated)
	if len(view.Batches) != 1 || len(view.Batches[0].Requests) != 3 || view.Batches[0].Replayed {
		test.Fatalf("batches = %+v, want one new batch of 3", view.Batches)
	}
	if cards := fixture.cards(test); cards != 1 {
		test.Fatalf("cards = %d, want 1", cards)
	}
	if requests, actions, fingerprints := fixture.batchRows(test, view.Batches[0].BatchID); requests != 3 || actions != 3 || fingerprints != 3 {
		test.Fatalf("requests=%d actions=%d distinct fingerprints=%d, want 3/3/3", requests, actions, fingerprints)
	}
	if pings := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND issue_id IN (SELECT issue_id FROM decision_request WHERE batch_id = $2)`, fixture.humanID, view.Batches[0].BatchID); pings != 1 {
		test.Fatalf("pings = %d, want 1 for the whole batch", pings)
	}
}

func TestBatchProposalOfMixedTypesOpensSeparateCards(test *testing.T) {
	fixture := newDMProposalFixture(test)
	body := fixture.batchBody(fixture.copyItem(test, "mixed a"), fixture.pauseItem(test), fixture.copyItem(test, "mixed b"))
	view := fixture.batchPropose(test, body, http.StatusCreated)
	if len(view.Batches) != 2 {
		test.Fatalf("batches = %+v, want 2 (one per action type)", view.Batches)
	}
	sizes := map[string]int{}
	ids := map[string]bool{}
	for _, batch := range view.Batches {
		sizes[batch.ActionType] = len(batch.Requests)
		ids[batch.BatchID] = true
	}
	if sizes["agent.copy"] != 2 || sizes["agent.pause"] != 1 || len(ids) != 2 {
		test.Fatalf("sizes = %v, batch ids = %v, want copy=2 pause=1 and two ids", sizes, ids)
	}
	if cards := fixture.cards(test); cards != 2 {
		test.Fatalf("cards = %d, want 2", cards)
	}
}

func TestBatchProposalReplayReturnsTheOpenBatch(test *testing.T) {
	fixture := newDMProposalFixture(test)
	body := fixture.batchBody(fixture.copyItem(test, "replay a"), fixture.copyItem(test, "replay b"))
	first := fixture.batchPropose(test, body, http.StatusCreated)
	rowsBefore := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE batch_id = $1`, first.Batches[0].BatchID)
	second := fixture.batchPropose(test, body, http.StatusOK)
	if len(second.Batches) != 1 || second.Batches[0].BatchID != first.Batches[0].BatchID || !second.Batches[0].Replayed {
		test.Fatalf("replay = %+v, want the open batch %s flagged replayed", second.Batches, first.Batches[0].BatchID)
	}
	if cards := fixture.cards(test); cards != 1 {
		test.Fatalf("cards = %d after replay, want 1", cards)
	}
	if rows := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE batch_id = $1`, first.Batches[0].BatchID); rows != rowsBefore {
		test.Fatalf("rows %d -> %d, want no new rows", rowsBefore, rows)
	}
	if all := dbfx.Count(test, `SELECT count(*) FROM decision_action WHERE requested_by_agent_id = $1`, fixture.agentID); all != 2 {
		test.Fatalf("actions = %d, want 2", all)
	}
}
