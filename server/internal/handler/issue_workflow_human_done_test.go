package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9259: through the real handler, an agent reaches done from a started step
// only after a member's recorded done decision (human_done) or once every
// linked PR has merged (pr.all_merged).

var humanDoneSteps = []map[string]any{
	{"status_key": "backlog"},
	{"status_key": "todo"},
	{"status_key": "in_progress", "edges": []map[string]any{
		{"to": "done", "guard": "human_done"},
		{"to": "done", "guard": "pr.all_merged"},
	}},
	{"status_key": "blocked"},
	{"status_key": "done"},
}

type humanDoneCase struct {
	edgeCase
	agentID, taskID string
}

// newHumanDoneCase puts an issue on humanDoneSteps at in_progress, with a run
// of an agent on it, as an agent's own status write would arrive.
func newHumanDoneCase(t *testing.T, installationID int64) humanDoneCase {
	t.Helper()
	c := newEdgeCase(t, installationID, humanDoneSteps)
	c.enter(t, "in_progress")
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	taskID := dbfx.Task(t, agentID, testutil.Cols{"issue_id": c.issueID, "runtime_id": runtimeID, "status": "running", "started_at": time.Now()})
	return humanDoneCase{edgeCase: c, agentID: agentID, taskID: taskID}
}

func (c humanDoneCase) agentMovesToDone(t *testing.T) *testutil.Response {
	t.Helper()
	return agentUpdateIssue(t, c.issueID, c.agentID, c.taskID, map[string]any{"status": "done"})
}

// answered records a card on the issue with the options, answered by a member
// who chose optionIndex (nil: no chosen option, as a free-text record has).
func (c humanDoneCase) answered(t *testing.T, options []decisiontx.AskOption, optionIndex *int) (requestID, answerID string) {
	t.Helper()
	raw, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	return insertAnsweredCard(t, c.issueID, c.agentID, string(raw), optionIndex)
}

// insertAnsweredCard writes an answered human decision request on the issue
// and one delivered answer by the test member (an undelivered answer holds an
// agent's close for a separate reason).
func insertAnsweredCard(t *testing.T, issueID, agentID, options string, optionIndex *int) (requestID, answerID string) {
	t.Helper()
	sessionID := dbfx.Insert(t, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": testUserID, "title": "human done card"})
	requestID = dbfx.Insert(t, "decision_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": issueID, "asking_agent_id": agentID, "human_user_id": testUserID,
		"chat_session_id": sessionID, "ask": "Close it?", "escalation_class": "reversible", "origin": "asked",
		"status": "answered", "options": options,
	})
	answerID = uuid.NewString()
	dbfx.Exec(t, `INSERT INTO decision_answer (id, decision_request_id, content_snapshot, chat_message_ids, effective_at, woken_at, answered_by_user_id, chosen_option_index, answer_source)
		VALUES ($1, $2, 'answer', '{}', now() - interval '1 minute', now(), $3, $4, 'card')`, answerID, requestID, testUserID, optionIndex)
	return requestID, answerID
}

func (c humanDoneCase) wantRefused(t *testing.T) {
	t.Helper()
	body := c.agentMovesToDone(t).Want(http.StatusBadRequest).Map()
	if body["code"] != "status_move_not_allowed" || !strings.Contains(body["error"].(string), "human_done") {
		t.Fatalf("refusal = %v; want status_move_not_allowed naming human_done", body)
	}
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("status = %q after a refused move, want in_progress", got)
	}
}

var doneAndLeaveOpen = []decisiontx.AskOption{
	{Label: "Set done", OptionKey: decisiontx.OptionKeyDone},
	{Label: "Leave open"},
}

// The CHE-7689 replay: no decision, the agent is refused; once the member's
// answer chose done, the same move goes through.
func TestAgentClosesAfterAMembersDoneDecision(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newHumanDoneCase(t, 9259001)
	c.wantRefused(t)
	c.answered(t, doneAndLeaveOpen, intp(0))
	c.agentMovesToDone(t).Want(http.StatusOK)
	if got := c.status(t); got != "done" {
		t.Fatalf("status = %q after the done decision, want done", got)
	}
}

// A typed issue.status.set to done counts as done too.
func TestAgentClosesAfterATypedStatusSetDoneAnswer(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newHumanDoneCase(t, 9259002)
	action := &decisiontx.OptionAction{Type: decisiontx.ActionStatusSet}
	action.Params.Status = "done"
	c.answered(t, []decisiontx.AskOption{{Label: "Close", AfterChoice: decisiontx.AfterServerAction, Action: action}}, intp(0))
	c.agentMovesToDone(t).Want(http.StatusOK)
}

// Answers that do not choose done leave the agent refused: another option, a
// label that only reads like done, a record with no chosen option, a typed
// action to another status, and a disclaimed answer.
func TestAgentIsRefusedWhenNoAnswerChoseDone(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	inReview := &decisiontx.OptionAction{Type: decisiontx.ActionStatusSet}
	inReview.Params.Status = "in_review"
	cases := map[string]func(c humanDoneCase){
		"the other option chosen": func(c humanDoneCase) { c.answered(t, doneAndLeaveOpen, intp(1)) },
		"label reads Set done but has no key": func(c humanDoneCase) {
			c.answered(t, []decisiontx.AskOption{{Label: "Set done"}, {Label: "Leave open"}}, intp(0))
		},
		"a record with no chosen option": func(c humanDoneCase) { c.answered(t, doneAndLeaveOpen, nil) },
		"a typed action to another status": func(c humanDoneCase) {
			c.answered(t, []decisiontx.AskOption{{Label: "Review", AfterChoice: decisiontx.AfterServerAction, Action: inReview}}, intp(0))
		},
		"a disclaimed answer": func(c humanDoneCase) {
			_, answerID := c.answered(t, doneAndLeaveOpen, intp(0))
			dbfx.Exec(t, `UPDATE decision_answer SET disclaimed_at = now() WHERE id = $1`, answerID)
		},
		"a request that is not answered": func(c humanDoneCase) {
			requestID, _ := c.answered(t, doneAndLeaveOpen, intp(0))
			dbfx.Exec(t, `UPDATE decision_request SET status = 'cancelled' WHERE id = $1`, requestID)
		},
	}
	installation := int64(9259010)
	for name, setup := range cases {
		installation++
		t.Run(name, func(t *testing.T) {
			c := newHumanDoneCase(t, installation)
			setup(c)
			c.wantRefused(t)
		})
	}
}

// An answer on another issue does not close this one.
func TestAnotherIssuesDoneDecisionDoesNotCountHere(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newHumanDoneCase(t, 9259003)
	other := dbfx.Issue(t, "other issue", testutil.Cols{"status": "in_progress"})
	raw, _ := json.Marshal(doneAndLeaveOpen)
	insertAnsweredCard(t, other, c.agentID, string(raw), intp(0))
	c.wantRefused(t)
}

// The engine applies a human done decision itself; no agent write is needed.
func TestEngineClosesTheIssueAfterAMembersDoneDecision(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newHumanDoneCase(t, 9259004)
	tickEdges(t)
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("status = %q with no decision, want in_progress", got)
	}
	c.answered(t, doneAndLeaveOpen, intp(0))
	// The scheduler pass runs on the watch's next due time; bring it forward.
	dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at = now() - interval '1 second' WHERE issue_id = $1 AND system_rule = 'workflow_edges'`, c.issueID)
	tickEdges(t)
	if got := c.status(t); got != "done" {
		t.Fatalf("status = %q after the done decision and a tick, want done", got)
	}
}

// pr.all_merged: no PR, an open PR and a closed-unmerged PR refuse; every PR
// merged opens the exit.
func TestAgentClosesWhenEveryLinkedPRMerged(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newHumanDoneCase(t, 9259005)
	body := c.agentMovesToDone(t).Want(http.StatusBadRequest).Map()
	if body["code"] != "status_move_not_allowed" || !strings.Contains(body["error"].(string), "pr.all_merged") {
		t.Fatalf("no-PR refusal = %v", body)
	}
	linkedPRForWakeup(t, c.fx, 9259005, 925901, "all-merged")
	setChildPR(t, c.fx.child.ID, "open")
	c.agentMovesToDone(t).Want(http.StatusBadRequest)
	setChildPR(t, c.fx.child.ID, "closed")
	c.agentMovesToDone(t).Want(http.StatusBadRequest)
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("status = %q after refused moves, want in_progress", got)
	}
	setChildPR(t, c.fx.child.ID, "merged")
	c.agentMovesToDone(t).Want(http.StatusOK)
	if got := c.status(t); got != "done" {
		t.Fatalf("status = %q with every PR merged, want done", got)
	}
}
