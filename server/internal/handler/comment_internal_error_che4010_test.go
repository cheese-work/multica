package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// postMentionComment posts an agent mention through the HTTP handler and
// returns the decoded response, so trigger_outcomes is read where a client reads it.
func postMentionComment(t *testing.T, issueID, agentID, authorAgentID string) CommentResponse {
	t.Helper()
	content := fmt.Sprintf("[@Agent](mention://agent/%s) please take a look", agentID)
	w := httptest.NewRecorder()
	r := withURLParam(newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content}), "id", issueID)
	if authorAgentID != "" {
		r.Header.Set("X-Agent-ID", authorAgentID)
		r.Header.Set("X-Task-ID", createAgentCommentTaskForPolicyTest(t, authorAgentID, issueID))
	}
	testHandler.CreateComment(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateComment: want 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp CommentResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	return resp
}

// TestCreateComment_StageQueueFullMentionReportsTypedReason (CHE-4010): the
// incident path over HTTP. A mention whose enqueue is refused by a full review
// stage is already queued server-side (CHE-7011), so it answers deferred +
// stage_queue_full, never blocked or internal_error.
func TestCreateComment_StageQueueFullMentionReportsTypedReason(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	reviewerFull(t, agentID, 2)
	issueID := dbfx.Issue(t, "stage full mention", testutil.Cols{
		"status": "agent_review", "assignee_type": "agent", "assignee_id": agentID,
	})
	linkOpenPR(t, issueID, "now()")
	logs := captureWarnings(t)

	// An agent's mention is derived, not named, so the stage queue judges it.
	resp := postMentionComment(t, issueID, agentID, createHandlerTestAgent(t, "Stage Full Author", nil))

	got := findCommentOutcome(t, resp.TriggerOutcomes, agentID)
	if got.Status != DispatchDeferred || got.ReasonCode != ReasonStageQueueFull {
		t.Errorf("outcome = %+v, want deferred/stage_queue_full", got)
	}
	if strings.Contains(logs.String(), "internal_error") {
		t.Errorf("a typed refusal must not log internal_error; logs:\n%s", logs.String())
	}
}

// TestCreateComment_EnqueueFailureLogsIds (CHE-4010): an unclassified enqueue
// error answers internal_error over HTTP, and the warning names the issue,
// comment and target so the cause is readable without the reason code.
func TestCreateComment_EnqueueFailureLogsIds(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Enqueue Failure Agent", nil)
	issueID := createCommentTriggerPreviewIssue(t, "enqueue failure mention", "", "")
	logs := captureWarnings(t)
	prev := testHandler.TaskService.Queries
	testHandler.TaskService.Queries = db.New(failQueryDBTX{DBTX: testPool, failOn: "-- name: GetAgent :one", err: errors.New("forced db failure")})
	t.Cleanup(func() { testHandler.TaskService.Queries = prev })

	resp := postMentionComment(t, issueID, agentID, "")

	got := findCommentOutcome(t, resp.TriggerOutcomes, agentID)
	if got.Status != DispatchBlocked || got.ReasonCode != ReasonInternalError {
		t.Fatalf("outcome = %+v, want blocked/internal_error", got)
	}
	out := logs.String()
	for _, want := range []string{
		"level=WARN",
		"comment admission reported internal_error",
		"issue_id=" + issueID,
		"comment_id=" + resp.ID,
		"target_type=agent",
		"target_id=" + agentID,
		"forced db failure",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q; logs:\n%s", want, out)
		}
	}
}

// TestCreateComment_TriageIssueMentionReportsTypedReason (CHE-4012): an agent
// mention on an issue in Triage answers blocked + issue_in_triage over HTTP.
func TestCreateComment_TriageIssueMentionReportsTypedReason(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "Triage Mention Agent", nil)
	issueID := createCommentTriggerPreviewIssue(t, "triage mention", "", "")
	dbfx.Exec(t, `UPDATE issue SET triage_state = 'pending' WHERE id = $1`, issueID)

	resp := postMentionComment(t, issueID, agentID, createHandlerTestAgent(t, "Triage Author", nil))

	got := findCommentOutcome(t, resp.TriggerOutcomes, agentID)
	if got.Status != DispatchBlocked || got.ReasonCode != ReasonIssueInTriage {
		t.Errorf("outcome = %+v, want blocked/issue_in_triage", got)
	}
}
