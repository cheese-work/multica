package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// recordWithMention posts the CHE-2459 shape: an authenticated decision record
// whose agent-written content names the next owner.
func (fixture askedRecordFixture) recordWithMention(test *testing.T, class, content string) (CommentResponse, int) {
	test.Helper()
	body := map[string]any{"decision_record": true, "message_ids": []string{fixture.messageID}, "parent_id": fixture.parentID, "content": content}
	if class != "" {
		body["escalation_class"] = class
	}
	encoded, _ := json.Marshal(body)
	request := fixture.recordRequest()
	request.Body = ioBody(string(encoded))
	response := httptest.NewRecorder()
	testHandler.CreateComment(response, withIssueParam(request, fixture.issueID))
	var comment CommentResponse
	if response.Code == http.StatusCreated {
		if err := json.Unmarshal(response.Body.Bytes(), &comment); err != nil {
			test.Fatal(err)
		}
	}
	return comment, response.Code
}

func mentionOf(agentID string) string {
	return fmt.Sprintf("[@Next](mention://agent/%s) please take the next step.", agentID)
}

func TestDecisionRecordDispatchesAgentMention(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	nextOwner := createHandlerTestAgent(test, "Record Next Owner", nil)
	comment, status := fixture.recordWithMention(test, "", mentionOf(nextOwner))
	if status != http.StatusCreated {
		test.Fatalf("record status=%d", status)
	}
	if len(comment.TriggerOutcomes) != 1 {
		test.Fatalf("trigger_outcomes = %+v, want one entry", comment.TriggerOutcomes)
	}
	outcome := findCommentOutcome(test, comment.TriggerOutcomes, nextOwner)
	if outcome.Status != DispatchQueued || outcome.ReasonCode != ReasonQueued {
		test.Fatalf("outcome = %+v, want queued", outcome)
	}
	if got := countQueuedCommentTriggerTasks(test, fixture.issueID, nextOwner); got != 1 {
		test.Fatalf("next-owner queued runs = %d, want 1", got)
	}
}

func TestDecisionRecordMentionControls(test *testing.T) {
	test.Run("no mention queues nothing", func(test *testing.T) {
		fixture := newAskedRecordFixture(test)
		before := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, fixture.issueID)
		comment, status := fixture.recordWithMention(test, "", "Plain explanation.")
		if status != http.StatusCreated || len(comment.TriggerOutcomes) != 0 {
			test.Fatalf("status=%d outcomes=%+v", status, comment.TriggerOutcomes)
		}
		if after := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, fixture.issueID); after != before {
			test.Fatalf("a record without a mention changed issue runs: %d -> %d", before, after)
		}
	})
	test.Run("mention inside the human quote is not routed", func(test *testing.T) {
		fixture := newAskedRecordFixture(test)
		nextOwner := createHandlerTestAgent(test, "Quoted Owner", nil)
		dbfx.Exec(test, `UPDATE chat_message SET content = $2 WHERE id = $1`, fixture.messageID, mentionOf(nextOwner))
		comment, status := fixture.recordWithMention(test, "", "Plain explanation.")
		if status != http.StatusCreated || len(comment.TriggerOutcomes) != 0 || countQueuedCommentTriggerTasks(test, fixture.issueID, nextOwner) != 0 {
			test.Fatalf("quoted mention was routed: status=%d outcomes=%+v", status, comment.TriggerOutcomes)
		}
	})
	test.Run("unauthorized target is blocked", func(test *testing.T) {
		fixture := newAskedRecordFixture(test)
		privateAgent, _, _ := privateAgentTestFixture(test)
		comment, status := fixture.recordWithMention(test, "", mentionOf(privateAgent))
		if status != http.StatusCreated {
			test.Fatalf("record status=%d", status)
		}
		outcome := findCommentOutcome(test, comment.TriggerOutcomes, privateAgent)
		if outcome.Status != DispatchBlocked || outcome.ReasonCode != ReasonInvocationNotAllowed || countQueuedCommentTriggerTasks(test, fixture.issueID, privateAgent) != 0 {
			test.Fatalf("outcome = %+v, want blocked/invocation_not_allowed", outcome)
		}
	})
	test.Run("stopped project is blocked", func(test *testing.T) {
		fixture := newAskedRecordFixture(test)
		nextOwner := createHandlerTestAgent(test, "Stopped Project Owner", nil)
		dbfx.Exec(test, `UPDATE issue SET project_id = $2 WHERE id = $1`, fixture.issueID, dbfx.Project(test, "Stopped", testutil.Cols{"status": "paused"}))
		comment, status := fixture.recordWithMention(test, "", mentionOf(nextOwner))
		if status != http.StatusCreated {
			test.Fatalf("record status=%d", status)
		}
		outcome := findCommentOutcome(test, comment.TriggerOutcomes, nextOwner)
		if outcome.Status != DispatchBlocked || outcome.ReasonCode != ReasonProjectInactive || countQueuedCommentTriggerTasks(test, fixture.issueID, nextOwner) != 0 {
			test.Fatalf("outcome = %+v, want blocked/project_inactive", outcome)
		}
	})
	test.Run("held class is deferred until release", func(test *testing.T) {
		fixture := newAskedRecordFixture(test)
		nextOwner := createHandlerTestAgent(test, "Held Owner", nil)
		dbfx.Exec(test, `UPDATE decision_request SET escalation_class = 'deploy' WHERE id = $1`, fixture.decision.ID)
		comment, status := fixture.recordWithMention(test, "", mentionOf(nextOwner))
		if status != http.StatusCreated {
			test.Fatalf("record status=%d", status)
		}
		outcome := findCommentOutcome(test, comment.TriggerOutcomes, nextOwner)
		if outcome.Status != DispatchDeferred || outcome.ReasonCode != ReasonDeferred || countQueuedCommentTriggerTasks(test, fixture.issueID, nextOwner) != 0 {
			test.Fatalf("outcome = %+v, want deferred with no run", outcome)
		}
	})
	test.Run("duplicate pending run is reused", func(test *testing.T) {
		fixture := newAskedRecordFixture(test)
		nextOwner := createHandlerTestAgent(test, "Duplicate Owner", nil)
		dbfx.Task(test, nextOwner, testutil.Cols{"issue_id": fixture.issueID, "status": "queued", "runtime_id": handlerTestRuntimeID(test)})
		comment, status := fixture.recordWithMention(test, "", mentionOf(nextOwner))
		if status != http.StatusCreated {
			test.Fatalf("record status=%d", status)
		}
		outcome := findCommentOutcome(test, comment.TriggerOutcomes, nextOwner)
		if outcome.Status == DispatchQueued || countQueuedCommentTriggerTasks(test, fixture.issueID, nextOwner) != 1 {
			test.Fatalf("duplicate created a run: outcome=%+v", outcome)
		}
	})
}
