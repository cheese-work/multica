package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestCommentDecisionHint(test *testing.T) {
	for _, scenario := range []string{"agent_member", "decision_intent", "agent_mention", "human_author", "no_mention", "repeated_member", "multiple_members", "invalid_member"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newIntentFixture(test)
			content := fixture.mention()
			intent, agent, wantHint := false, true, true
			targets := []string{fixture.humanID}
			switch scenario {
			case "decision_intent":
				intent, wantHint = true, false
			case "agent_mention":
				content, wantHint = "[@Agent](mention://agent/11111111-1111-4111-8111-111111111111)", false
			case "human_author":
				agent, wantHint = false, false
			case "no_mention":
				content, wantHint = "FYI: work is underway", false
			case "repeated_member":
				content += " " + fixture.mention()
			case "multiple_members":
				content += fmt.Sprintf(" [@Owner](mention://member/%s)", testUserID)
				targets = append(targets, testUserID)
			case "invalid_member":
				content, wantHint = "[@Human](mention://member/abc)", false
			}
			body := map[string]any{"content": content, "decision_intent": intent}
			request := withURLParam(newRequest(http.MethodPost, "/api/issues/"+fixture.issueID+"/comments", body), "id", fixture.issueID)
			if agent {
				request.Header.Set("X-Actor-Source", "task_token")
				request.Header.Set("X-Agent-ID", fixture.agentID)
				request.Header.Set("X-Task-ID", fixture.taskID)
			}
			var response struct {
				CommentResponse
				Hints []string `json:"hints"`
			}
			testutil.Call(test, testHandler.CreateComment, request).Want(http.StatusCreated).JSON(&response)
			if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE id = $1 AND issue_id = $2 AND content = $3`, response.ID, fixture.issueID, content); count != 1 {
				test.Fatalf("comment was not persisted: count=%d", count)
			}
			if !wantHint {
				if len(response.Hints) != 0 {
					test.Fatalf("unexpected hints: %v", response.Hints)
				}
				return
			}
			if len(response.Hints) != 1 {
				test.Fatalf("persisted comment needs one advisory hint, got %v", response.Hints)
			}
			for _, target := range targets {
				command := "multica decision request --issue " + fixture.issueID + " --for " + target
				if strings.Count(response.Hints[0], command) != 1 {
					test.Errorf("hint needs one command %q, got %q", command, response.Hints[0])
				}
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE comment_id = $1`, response.ID); count != 0 {
				test.Fatalf("advice created a decision window: count=%d", count)
			}
		})
	}
}

func TestCommentDecisionHintSingleCall(test *testing.T) {
	fixture := newSingleCallFixture(test)
	var comment CommentResponse
	fixture.post(test, "task_token", fixture.taskID, fixture.mention(), map[string]any{"decision_intent": false}).Want(http.StatusCreated).JSON(&comment)
	if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE id = $1 AND issue_id = $2`, comment.ID, fixture.issueID); count != 1 {
		test.Fatalf("comment was not persisted: count=%d", count)
	}
	wantHint := fmt.Sprintf("If this needs a human answer, prefer a decision request. Use: multica decision request --issue %s --for %s --class CLASS --ask-file ASK.json. A plain mention is for FYI.", fixture.issueID, fixture.humanID)
	if len(comment.Hints) != 1 || comment.Hints[0] != wantHint {
		test.Fatalf("hint must recommend the single-call path without an intent-window prerequisite: got %q, want %q", comment.Hints, wantHint)
	}
	fixture.details.SourceCommentID = comment.ID
	var decision map[string]any
	testutil.Call(test, testHandler.RequestDecision, fixture.call(fixture.body())).Want(http.StatusCreated).JSON(&decision)
	if decision["human_user_id"] != fixture.humanID || decision["status"] != "open" {
		test.Fatalf("hint's single-call request was not created for the mentioned human: %+v", decision)
	}
}
