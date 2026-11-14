//go:build issue_channels_red

package handler

// CHE-5906 (C1 spike, CHE-5673 plan v2): failing test for slice C5, "thread run cap".
//
// Contract under test (plan v2 section 3.4, C5). `thread_run_cap` is a workspace setting
// {"mode": "off"|"warn"|"block", "max": M}. It counts the runs whose trigger_comment_id lies in one
// thread (found with GetThreadRoot) inside a rolling 24 hours. Under "block" the comment is saved and
// the run is refused; the refusal shows as reason_code "thread_run_cap" in trigger_outcomes.
//
// The test is behind the `issue_channels_red` build tag so main stays green. C5 deletes the tag.
// Run:  go test -tags issue_channels_red -run TestThreadRunCapC5 ./internal/handler/

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestThreadRunCapC5(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "C5 cap agent", nil)
	runtimeID := handlerTestRuntimeID(t)

	// seedRuns adds n finished runs triggered by comments of the thread, created `age` ago.
	seedRuns := func(t *testing.T, issueID, commentID string, n int, age string) {
		t.Helper()
		for i := 0; i < n; i++ {
			dbfx.Task(t, agentID, testutil.Cols{
				"issue_id": issueID, "runtime_id": runtimeID, "status": "completed",
				"trigger_comment_id": commentID,
				"created_at":         testutil.Raw(fmt.Sprintf("now() - interval '%s'", age)),
				"completed_at":       testutil.Raw(fmt.Sprintf("now() - interval '%s'", age)),
			})
		}
	}
	setCap := func(t *testing.T, mode string, max int) {
		t.Helper()
		var original []byte
		dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&original)
		t.Cleanup(func() { dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, original, testWorkspaceID) })
		patch, _ := json.Marshal(map[string]any{"thread_run_cap": map[string]any{"mode": mode, "max": max}})
		dbfx.Exec(t, `UPDATE workspace SET settings = COALESCE(settings, '{}'::jsonb) || $1::jsonb WHERE id = $2`, patch, testWorkspaceID)
	}
	// mention posts a member reply that names the agent. It returns the HTTP status and trigger_outcomes.
	mention := func(t *testing.T, issueID, parentID string) (int, []CommentTriggerOutcome) {
		t.Helper()
		content := fmt.Sprintf("[@C5 cap agent](mention://agent/%s) please look", agentID)
		body := map[string]any{"content": content}
		if parentID != "" {
			body["parent_id"] = parentID
		}
		res := postCommentForGate(t, newRequest(http.MethodPost, "/api/issues/"+issueID+"/comments", body), issueID)
		var resp CommentResponse
		res.JSON(&resp)
		return res.Code, resp.TriggerOutcomes
	}
	newThread := func(t *testing.T, title string) (issueID, rootID string) {
		t.Helper()
		issueID = gateIssue(t, title)
		rootID = dbfx.Comment(t, issueID, "thread root", testutil.Cols{"author_type": "agent", "author_id": agentID})
		return issueID, rootID
	}
	wantOutcome := func(t *testing.T, outcomes []CommentTriggerOutcome, wantBlocked bool) {
		t.Helper()
		if len(outcomes) != 1 || outcomes[0].TargetID != agentID {
			t.Fatalf("trigger_outcomes = %+v, want exactly one outcome for agent %s", outcomes, agentID)
		}
		blocked := string(outcomes[0].ReasonCode) == "thread_run_cap"
		if blocked != wantBlocked {
			t.Errorf("outcome = %+v, blocked by thread_run_cap = %v, want %v", outcomes[0], blocked, wantBlocked)
		}
	}

	t.Run("block refuses the run over the cap and saves the comment", func(t *testing.T) {
		setCap(t, "block", 2)
		issueID, rootID := newThread(t, "C5 block")
		seedRuns(t, issueID, rootID, 2, "1 hour")
		status, outcomes := mention(t, issueID, rootID)
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201: the comment is saved even when the run is refused", status)
		}
		wantOutcome(t, outcomes, true)
		if n := countIssueTasks(t, issueID); n != 2 {
			t.Errorf("tasks = %d, want 2: the refused run must not be enqueued", n)
		}
	})

	t.Run("runs older than 24 hours do not count", func(t *testing.T) {
		setCap(t, "block", 2)
		issueID, rootID := newThread(t, "C5 window")
		seedRuns(t, issueID, rootID, 2, "25 hours") // outside the window
		seedRuns(t, issueID, rootID, 2, "1 hour")   // inside the window: at the cap
		status, outcomes := mention(t, issueID, rootID)
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want 201", status)
		}
		wantOutcome(t, outcomes, true) // 2 in the window, so the third is refused
	})

	t.Run("count is per thread, not per issue", func(t *testing.T) {
		setCap(t, "block", 2)
		issueID, rootA := newThread(t, "C5 threads")
		rootB := dbfx.Comment(t, issueID, "second thread root", testutil.Cols{"author_type": "agent", "author_id": agentID})
		seedRuns(t, issueID, rootA, 2, "1 hour")
		// A reply deep in thread A counts against A (GetThreadRoot), so it is refused ...
		replyA := dbfx.Comment(t, issueID, "reply in A", testutil.Cols{"author_type": "agent", "author_id": agentID, "parent_id": rootA})
		_, outcomesA := mention(t, issueID, replyA)
		wantOutcome(t, outcomesA, true)
		// ... while thread B has no runs and still starts one.
		_, outcomesB := mention(t, issueID, rootB)
		wantOutcome(t, outcomesB, false)
	})

	t.Run("member and agent triggered runs both count", func(t *testing.T) {
		setCap(t, "block", 2)
		issueID, rootID := newThread(t, "C5 origins")
		memberReply := dbfx.Comment(t, issueID, "member reply", testutil.Cols{"author_type": "member", "author_id": testUserID, "parent_id": rootID})
		seedRuns(t, issueID, rootID, 1, "1 hour")     // triggered by an agent comment
		seedRuns(t, issueID, memberReply, 1, "1 hour") // triggered by a member comment (OriginNamed skips the loop breaker)
		_, outcomes := mention(t, issueID, rootID)
		wantOutcome(t, outcomes, true)
	})

	t.Run("warn logs only and still starts the run", func(t *testing.T) {
		setCap(t, "warn", 2)
		issueID, rootID := newThread(t, "C5 warn")
		seedRuns(t, issueID, rootID, 3, "1 hour")
		_, outcomes := mention(t, issueID, rootID)
		wantOutcome(t, outcomes, false)
		if n := countIssueTasks(t, issueID); n != 4 {
			t.Errorf("tasks = %d, want 4: warn must not refuse", n)
		}
	})
}
