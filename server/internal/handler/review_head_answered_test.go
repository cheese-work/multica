package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
)

// CHE-4649: a reviewer run for a PR head the agent already answered is not
// started, and the skip is recorded where the run list shows it.

const (
	headOld = "aaaaaaa1111111111111111111111111111111aa"
	headNew = "bbbbbbb2222222222222222222222222222222bb"
)

// reviewCase is an in_review issue assigned to a reviewer in the guard's set.
func reviewCase(t *testing.T, installation int64) (edgeCase, string) {
	t.Helper()
	c := newEdgeCase(t, installation, mergeEdgeSteps)
	agentID := seededReadyAgentID(t)
	dbfx.Exec(t, `UPDATE issue SET status='in_review', assignee_type='agent', assignee_id=$2 WHERE id=$1`, c.issueID, agentID)
	service.SetReviewGuardAgents(agentID)
	t.Cleanup(func() { service.SetReviewGuardAgents() })
	return c, agentID
}

func agentSays(t *testing.T, c edgeCase, agentID, text string) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content) VALUES ($1, $2, 'agent', $3, $4)`,
		c.issueID, testWorkspaceID, agentID, text)
}

func enqueueReview(t *testing.T, c edgeCase) error {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	_, err = testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue)
	return err
}

func TestReviewRunSkippedWhenAgentAlreadyGaveVerdictForHead(t *testing.T) {
	for name, verdict := range map[string]string{"verdict": "Verdict: PASS", "blocked": "BLOCKED: no runner", "not-run": "NOT-RUN: device offline"} {
		t.Run(name, func(t *testing.T) {
			if testHandler == nil {
				t.Skip("database not available")
			}
			c, agentID := reviewCase(t, 4649001)
			linkPR(t, c.issueID, 4649001, headOld)
			agentSays(t, c, agentID, "Reviewed head "+headOld[:7]+". "+verdict)
			if err := enqueueReview(t, c); !errors.Is(err, service.ErrReviewHeadAnswered) {
				t.Fatalf("same-head review run = %v, want ErrReviewHeadAnswered", err)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, c.issueID); n != 0 {
				t.Fatalf("task rows = %d, want none: a skipped run must not count against the run budget", n)
			}
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_skipped' AND details->>'reason'='review_head_answered' AND details->>'head_sha'=$2`, c.issueID, headOld) == 1 {
					return
				}
			}
			t.Fatal("no run_skipped activity with reason review_head_answered")
		})
	}
}

func TestReviewRunStartsOnNewHead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, agentID := reviewCase(t, 4649002)
	linkPR(t, c.issueID, 4649002, headOld)
	agentSays(t, c, agentID, "Reviewed head "+headOld[:7]+". Verdict: FAIL")
	dbfx.Exec(t, `UPDATE github_pull_request SET head_sha=$2 WHERE id IN (SELECT pull_request_id FROM issue_pull_request WHERE issue_id=$1)`, c.issueID, headNew)
	if err := enqueueReview(t, c); err != nil {
		t.Fatalf("new-head review run = %v, want it started", err)
	}
}

func TestReviewRunIgnoresAnotherAgentsVerdict(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, _ := reviewCase(t, 4649003)
	linkPR(t, c.issueID, 4649003, headOld)
	other := "0f0f0f0f-0000-4000-8000-00000000c4a9"
	agentSays(t, c, other, "Reviewed head "+headOld[:7]+". Verdict: PASS")
	if err := enqueueReview(t, c); err != nil {
		t.Fatalf("review run after another agent's verdict = %v, want it started", err)
	}
}

func TestReviewRunStartsWithoutExactlyOneLinkedPR(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Run("no PR", func(t *testing.T) {
		c, agentID := reviewCase(t, 4649004)
		agentSays(t, c, agentID, "Reviewed head "+headOld[:7]+". Verdict: PASS")
		if err := enqueueReview(t, c); err != nil {
			t.Fatalf("run with 0 linked PRs = %v, want it started", err)
		}
	})
	t.Run("two PRs", func(t *testing.T) {
		c, agentID := reviewCase(t, 4649005)
		linkPR(t, c.issueID, 4649005, headOld)
		linkPR(t, c.issueID, 4649006, headNew)
		agentSays(t, c, agentID, "Reviewed head "+headOld[:7]+". Verdict: PASS")
		if err := enqueueReview(t, c); err != nil {
			t.Fatalf("run with 2 linked PRs = %v, want it started", err)
		}
	})
}
