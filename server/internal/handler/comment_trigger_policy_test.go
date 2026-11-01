package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func setAgentCommentWakePolicyForTest(t *testing.T, enabled bool) {
	t.Helper()

	var previous []byte
	if err := testPool.QueryRow(context.Background(), `SELECT settings FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&previous); err != nil {
		t.Fatalf("read workspace settings: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `UPDATE workspace SET settings=$1 WHERE id=$2`, previous, testWorkspaceID); err != nil {
			t.Errorf("restore workspace settings: %v", err)
		}
	})
	saveWorkspaceSettings(t, testWorkspaceID, map[string]any{agentCommentWakePolicySetting: enabled})
}

func postCommentForTriggerPolicyTest(t *testing.T, issueID, content string) []CommentTriggerOutcome {
	t.Helper()

	req := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content})
	req = withURLParam(req, "id", issueID)
	rec := httptest.NewRecorder()
	testHandler.CreateComment(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("CreateComment: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		TriggerOutcomes []CommentTriggerOutcome `json:"trigger_outcomes"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode comment response: %v", err)
	}
	return response.TriggerOutcomes
}

func postAgentCommentForTriggerPolicyTest(t *testing.T, issueID, authorAgentID, taskID, content string) []CommentTriggerOutcome {
	t.Helper()

	req := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content})
	req = withURLParam(req, "id", issueID)
	req.Header.Set("X-Agent-ID", authorAgentID)
	req.Header.Set("X-Task-ID", taskID)
	rec := httptest.NewRecorder()
	testHandler.CreateComment(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("CreateComment: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		TriggerOutcomes []CommentTriggerOutcome `json:"trigger_outcomes"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode comment response: %v", err)
	}
	return response.TriggerOutcomes
}

func createAgentCommentTaskForPolicyTest(t *testing.T, agentID, issueID string) string {
	t.Helper()

	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, issueID)
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET originator_user_id=$1, accountable_user_id=$1 WHERE id=$2`, testUserID, taskID); err != nil {
		t.Fatalf("set comment task originator: %v", err)
	}
	return taskID
}

func TestAgentCommentExplicitAgentMentionTriggersTarget(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, true)

	fx := newSquadCommentTriggerFixture(t)
	content := "[@Worker](mention://agent/" + fx.OtherID + ") please review"
	triggers, targets := testHandler.computeCommentAgentTriggers(context.Background(), fx.Issue, content, nil, "agent", fx.LeaderID, commentTriggerComputeOptions{})
	if len(triggers) != 1 || uuidToString(triggers[0].Agent.ID) != fx.OtherID {
		t.Fatalf("explicit agent mention triggers = %+v, want only %s", triggers, fx.OtherID)
	}
	if len(targets) != 1 || targets[0].TargetType != "agent" || targets[0].TargetID != fx.OtherID {
		t.Fatalf("explicit mention outcomes = %+v, want the mentioned agent", targets)
	}
}

func TestAgentCommentWakePolicyCanRestoreLegacyRouting(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, false)

	fx := newSquadCommentTriggerFixture(t)
	triggers, _ := testHandler.computeCommentAgentTriggers(context.Background(), fx.Issue, "worker finished", nil, "agent", fx.OtherID, commentTriggerComputeOptions{})
	if len(triggers) != 1 || uuidToString(triggers[0].Agent.ID) != fx.LeaderID {
		t.Fatalf("legacy implicit route = %+v, want assigned leader %s", triggers, fx.LeaderID)
	}
}

func TestAgentCommentWakePolicyNullOverrideKeepsStrictRouting(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	var previous []byte
	if err := testPool.QueryRow(context.Background(), `SELECT settings FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&previous); err != nil {
		t.Fatalf("read workspace settings: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `UPDATE workspace SET settings=$1 WHERE id=$2`, previous, testWorkspaceID); err != nil {
			t.Errorf("restore workspace settings: %v", err)
		}
	})
	if _, err := testPool.Exec(context.Background(), `UPDATE workspace SET settings=jsonb_set(settings, '{agent_comment_wake_policy_enabled}', 'null'::jsonb, true) WHERE id=$1`, testWorkspaceID); err != nil {
		t.Fatalf("set null comment wake policy: %v", err)
	}

	fx := newSquadCommentTriggerFixture(t)
	triggers, targets := testHandler.computeCommentAgentTriggers(context.Background(), fx.Issue, "worker finished", nil, "agent", fx.OtherID, commentTriggerComputeOptions{})
	if len(triggers) != 0 {
		t.Fatalf("null setting implicit routes = %+v, want none", triggers)
	}
	if len(targets) != 1 || targets[0].ReasonCode != ReasonAgentCommentImplicitRouteSuppressed {
		t.Fatalf("null setting outcomes = %+v, want strict-policy suppression", targets)
	}
}

func TestEditedAgentCommentKeepsAgentRoutingPolicy(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, true)

	fx := newSquadCommentTriggerFixture(t)
	commentID := dbfx.Comment(t, uuidToString(fx.Issue.ID), "worker finished", testutil.Cols{
		"author_type": "agent",
		"author_id":   fx.OtherID,
	})
	comment, err := testHandler.Queries.GetComment(context.Background(), parseUUID(commentID))
	if err != nil {
		t.Fatalf("load agent comment: %v", err)
	}
	outcomes := testHandler.triggerTasksForComment(context.Background(), fx.Issue, comment, nil, "member", testUserID, testUserID, nil, commentSteerRequest{})
	if len(outcomes) != 1 || outcomes[0].Status != DispatchBlocked || outcomes[0].ReasonCode != ReasonAgentCommentImplicitRouteSuppressed {
		t.Fatalf("edited agent comment outcomes = %+v, want an implicit-route suppression", outcomes)
	}
	var leaderRuns int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, fx.Issue.ID, fx.LeaderID).Scan(&leaderRuns); err != nil {
		t.Fatalf("count assigned leader runs: %v", err)
	}
	if leaderRuns != 0 {
		t.Fatalf("edited agent comment queued %d assigned-leader runs, want none", leaderRuns)
	}
}

func TestEditedAgentCommentPreviewKeepsAgentRoutingPolicy(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, true)

	fx := newSquadCommentTriggerFixture(t)
	commentID := dbfx.Comment(t, uuidToString(fx.Issue.ID), "worker finished", testutil.Cols{
		"author_type": "agent",
		"author_id":   fx.OtherID,
	})
	preview := previewCommentTriggersForTest(t, uuidToString(fx.Issue.ID), map[string]any{
		"content":            "worker finished with an edit",
		"editing_comment_id": commentID,
	})
	if len(preview.Agents) != 0 {
		t.Fatalf("edited agent comment preview agents = %+v, want no implicit trigger", preview.Agents)
	}
	if len(preview.Blocked) != 1 || preview.Blocked[0].ReasonCode != ReasonAgentCommentImplicitRouteSuppressed {
		t.Fatalf("edited agent comment preview blocked outcomes = %+v, want implicit-route suppression", preview.Blocked)
	}
}

func TestMemberCommentKeepsImplicitSquadRouting(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, true)

	fx := newSquadCommentTriggerFixture(t)
	triggers, _ := testHandler.computeCommentAgentTriggers(context.Background(), fx.Issue, "please take a look", nil, "member", testUserID, commentTriggerComputeOptions{})
	if len(triggers) != 1 || uuidToString(triggers[0].Agent.ID) != fx.LeaderID {
		t.Fatalf("member implicit route = %+v, want assigned leader %s", triggers, fx.LeaderID)
	}
}

func TestCommentTriggerRateLimitCoalescesBlocksAndExpires(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, true)

	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)
	authorTaskID := createAgentCommentTaskForPolicyTest(t, fx.LeaderID, issueID)
	content := "[@Worker](mention://agent/" + fx.OtherID + ") please review"

	first := postAgentCommentForTriggerPolicyTest(t, issueID, fx.LeaderID, authorTaskID, content)
	if len(first) != 1 || first[0].Status != DispatchQueued {
		t.Fatalf("first comment outcomes = %+v, want queued", first)
	}
	second := postAgentCommentForTriggerPolicyTest(t, issueID, fx.LeaderID, authorTaskID, content)
	if len(second) != 1 || second[0].Status != DispatchCoalesced {
		t.Fatalf("second comment outcomes = %+v, want coalesced into the pending run", second)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE issue_id=$1 AND agent_id=$2`, issueID, fx.OtherID); err != nil {
		t.Fatalf("complete first run: %v", err)
	}
	third := postAgentCommentForTriggerPolicyTest(t, issueID, fx.LeaderID, authorTaskID, content)
	if len(third) != 1 || third[0].Status != DispatchBlocked || string(third[0].ReasonCode) != "comment_trigger_rate_limited" {
		t.Fatalf("third comment outcomes = %+v, want blocked with comment_trigger_rate_limited", third)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE comment_trigger_rate_limit SET triggered_at=now()-interval '11 minutes' WHERE issue_id=$1 AND agent_id=$2`, issueID, fx.OtherID); err != nil {
		t.Fatalf("expire comment trigger window: %v", err)
	}
	fourth := postAgentCommentForTriggerPolicyTest(t, issueID, fx.LeaderID, authorTaskID, content)
	if len(fourth) != 1 || fourth[0].Status != DispatchQueued {
		t.Fatalf("fourth comment outcomes = %+v, want queued after expiry", fourth)
	}
	var taskCount int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, fx.OtherID).Scan(&taskCount); err != nil {
		t.Fatalf("count target runs: %v", err)
	}
	if taskCount != 2 {
		t.Fatalf("target run count = %d, want 2 (initial + post-window)", taskCount)
	}
}

func TestMemberCommentTriggersAreNotRateLimited(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, true)

	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)
	if _, err := testPool.Exec(context.Background(), `INSERT INTO comment_trigger_rate_limit (issue_id, agent_id, triggered_at) VALUES ($1, $2, now())`, fx.Issue.ID, fx.LeaderID); err != nil {
		t.Fatalf("seed agent-comment rate limit: %v", err)
	}
	for index := range 2 {
		outcomes := postCommentForTriggerPolicyTest(t, issueID, fmt.Sprintf("member follow-up %d", index))
		for _, outcome := range outcomes {
			if outcome.Status == DispatchBlocked && outcome.ReasonCode == ReasonCommentTriggerRateLimited {
				t.Fatalf("member comment %d outcomes = %+v, want normal member routing despite agent rate-limit state", index+1, outcomes)
			}
		}
	}
	var commentCount, taskCount int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM comment WHERE issue_id=$1 AND author_type='member'`, fx.Issue.ID).Scan(&commentCount); err != nil {
		t.Fatalf("count member comments: %v", err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, fx.Issue.ID, fx.LeaderID).Scan(&taskCount); err != nil {
		t.Fatalf("count member-triggered leader runs: %v", err)
	}
	if commentCount != 2 || taskCount != 1 {
		t.Fatalf("member comments=%d leader runs=%d, want two routed comments covered by one run", commentCount, taskCount)
	}
}

func TestCommentTriggerRateLimitDoesNotApplyToOtherEnqueueSources(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, true)

	fx := newSquadCommentTriggerFixture(t)
	issueID := createCommentTriggerPreviewIssue(t, "workflow enqueue rate limit bypass", "agent", fx.OtherID)
	content := "[@Worker](mention://agent/" + fx.OtherID + ") please review"
	outcomes := postCommentForTriggerPolicyTest(t, issueID, content)
	if len(outcomes) != 1 || outcomes[0].Status != DispatchQueued {
		t.Fatalf("comment outcomes = %+v, want queued", outcomes)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE issue_id=$1 AND agent_id=$2`, issueID, fx.OtherID); err != nil {
		t.Fatalf("complete comment-triggered run: %v", err)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue for non-comment enqueue: %v", err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); err != nil {
		t.Fatalf("non-comment issue enqueue was rate-limited: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE issue_id=$1 AND agent_id=$2`, issueID, fx.OtherID); err != nil {
		t.Fatalf("complete non-comment issue enqueue: %v", err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForMention(context.Background(), issue, parseUUID(fx.OtherID), pgtype.UUID{}, service.OriginNamed); err != nil {
		t.Fatalf("non-comment named enqueue was rate-limited: %v", err)
	}
}

func TestConcurrentCommentTriggersStartAtMostOneRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	setAgentCommentWakePolicyForTest(t, true)

	fx := newSquadCommentTriggerFixture(t)
	issueID := uuidToString(fx.Issue.ID)
	authorTaskID := createAgentCommentTaskForPolicyTest(t, fx.LeaderID, issueID)
	content := "[@Worker](mention://agent/" + fx.OtherID + ") please review"
	type result struct {
		outcomes []CommentTriggerOutcome
		err      error
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			req := newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", map[string]any{"content": content})
			req = withURLParam(req, "id", issueID)
			req.Header.Set("X-Agent-ID", fx.LeaderID)
			req.Header.Set("X-Task-ID", authorTaskID)
			rec := httptest.NewRecorder()
			testHandler.CreateComment(rec, req)
			if rec.Code != http.StatusCreated {
				results <- result{err: fmt.Errorf("CreateComment: expected 201, got %d: %s", rec.Code, rec.Body.String())}
				return
			}
			var response struct {
				TriggerOutcomes []CommentTriggerOutcome `json:"trigger_outcomes"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
				results <- result{err: err}
				return
			}
			results <- result{outcomes: response.TriggerOutcomes}
		}()
	}
	wait.Wait()
	close(results)
	queued, accounted := 0, 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.outcomes) != 1 {
			t.Fatalf("concurrent trigger outcomes = %+v, want one target outcome", result.outcomes)
		}
		accounted++
		switch result.outcomes[0].Status {
		case DispatchQueued:
			queued++
		case DispatchCoalesced:
		case DispatchBlocked:
			if string(result.outcomes[0].ReasonCode) != "comment_trigger_rate_limited" {
				t.Fatalf("blocked concurrent outcome = %+v, want rate-limited reason", result.outcomes[0])
			}
		default:
			t.Fatalf("concurrent trigger outcome = %+v, want queued/coalesced/limited", result.outcomes[0])
		}
	}
	if queued != 1 || accounted != 2 {
		t.Fatalf("concurrent outcomes = queued %d, accounted %d, want one queued and both accounted", queued, accounted)
	}
	var taskCount int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, fx.OtherID).Scan(&taskCount); err != nil {
		t.Fatalf("count target runs: %v", err)
	}
	if taskCount != 1 {
		t.Fatalf("target run count = %d, want exactly one", taskCount)
	}
}
