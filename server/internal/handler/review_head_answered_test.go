package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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

// verdictFor is a reviewer's signed verdict comment for one candidate head.
func verdictFor(word, head string) string {
	return "**Verdict: " + word + "**\nReviewer: x\nCandidate: `" + head + "`\n\nFindings: none.\n"
}

// waitSkipped waits for the async skip record (the refusal rolls back the
// enqueue transaction, so the hook writes it on its own connection).
func waitSkipped(t *testing.T, issueID, head string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_skipped' AND details->>'reason'='review_head_answered' AND details->>'head_sha'=$2`, issueID, head) >= 1 {
			return
		}
	}
	t.Fatal("no run_skipped activity with reason review_head_answered")
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
	for name, verdict := range map[string]string{
		"pass":    verdictFor("PASS", headOld),
		"changes": verdictFor("CHANGES REQUESTED", headOld),
		"blocked": verdictFor("BLOCKED", headOld),
		"not-run": verdictFor("NOT-RUN", headOld),
		"bare":    "BLOCKED: no runner\nCandidate: `" + headOld + "`\n",
	} {
		t.Run(name, func(t *testing.T) {
			if testHandler == nil {
				t.Skip("database not available")
			}
			c, agentID := reviewCase(t, 4649001)
			linkPR(t, c.issueID, 4649001, headOld)
			agentSays(t, c, agentID, verdict)
			if err := enqueueReview(t, c); !errors.Is(err, service.ErrReviewHeadAnswered) {
				t.Fatalf("same-head review run = %v, want ErrReviewHeadAnswered", err)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, c.issueID); n != 0 {
				t.Fatalf("task rows = %d, want none: a skipped run must not count against the run budget", n)
			}
			waitSkipped(t, c.issueID, headOld)
		})
	}
}

func TestReviewRunStartsOnNewHead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, agentID := reviewCase(t, 4649002)
	linkPR(t, c.issueID, 4649002, headOld)
	agentSays(t, c, agentID, verdictFor("FAIL", headOld))
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
	agentSays(t, c, other, verdictFor("PASS", headOld))
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
		agentSays(t, c, agentID, verdictFor("PASS", headOld))
		if err := enqueueReview(t, c); err != nil {
			t.Fatalf("run with 0 linked PRs = %v, want it started", err)
		}
	})
	t.Run("two PRs", func(t *testing.T) {
		c, agentID := reviewCase(t, 4649005)
		linkPR(t, c.issueID, 4649005, headOld)
		linkPR(t, c.issueID, 4649006, headNew)
		agentSays(t, c, agentID, verdictFor("PASS", headOld))
		if err := enqueueReview(t, c); err != nil {
			t.Fatalf("run with 2 linked PRs = %v, want it started", err)
		}
	})
}

// A comment that merely contains PASS next to the head is not the reviewer's
// verdict: a CI status report, or a verdict for another head, starts the run.
func TestReviewRunStartsWhenCommentIsNotAVerdictForThisHead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for name, text := range map[string]string{
		"ci status report": "CI PASS on " + headOld[:7] + ", reviewing now.",
		"other head":       verdictFor("PASS", headNew),
		"head in prose":    "**Verdict: PASS**\nSuperseded " + headOld[:7] + " earlier.\n",
		"lower case":       "verdict: this may fail on " + headOld[:7],
	} {
		t.Run(name, func(t *testing.T) {
			c, agentID := reviewCase(t, 4649007)
			linkPR(t, c.issueID, 4649007, headOld)
			agentSays(t, c, agentID, text)
			if err := enqueueReview(t, c); err != nil {
				t.Fatalf("review run after %q = %v, want it started", name, err)
			}
		})
	}
}

// A wakeup reaches the reviewer through issue_wakeup dispatch, not the comment
// path: it must skip an answered head (and still start on a new one).
func TestReviewWakeupSkippedWhenAgentAlreadyAnsweredHead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	wakeups := &service.IssueWakeupService{Tasks: testHandler.TaskService}
	fire := func(t *testing.T, c edgeCase, agentID string) {
		t.Helper()
		w, err := wakeups.Create(ctx, parseUUID(c.issueID), parseUUID(testUserID), pgtype.UUID{}, service.WakeupInput{
			AgentID: agentID, Kind: "event", Mode: "once", EventTypes: []string{"comment.created"}, Instruction: "Review the PR head.",
		})
		if err != nil {
			t.Fatal(err)
		}
		dbfx.Cleanup(t, `DELETE FROM issue_wakeup WHERE id = $1`, w.ID)
		dbfx.Cleanup(t, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id = $1`, w.ID)
		dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, c.issueID)
		dbfx.Exec(t, `INSERT INTO issue_wakeup_receipt (id, wakeup_id, revision, event_key, event_type, payload) VALUES (gen_random_uuid(), $1, $2, 'review-head-test', 'comment.created', '{}'::jsonb)`, w.ID, w.Revision)
		if err := wakeups.TickWorkspaces(ctx, parseUUID(testWorkspaceID)); err != nil {
			t.Fatal(err)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NULL`, w.ID); n != 0 {
			t.Fatalf("%d receipts left unprocessed: the skip must consume them, not retry", n)
		}
	}
	t.Run("answered head", func(t *testing.T) {
		c, agentID := reviewCase(t, 4649008)
		linkPR(t, c.issueID, 4649008, headOld)
		agentSays(t, c, agentID, verdictFor("PASS", headOld))
		fire(t, c, agentID)
		if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, c.issueID); n != 0 {
			t.Fatalf("wakeup started %d runs on an answered head, want none", n)
		}
		waitSkipped(t, c.issueID, headOld)
	})
	t.Run("new head", func(t *testing.T) {
		c, agentID := reviewCase(t, 4649009)
		linkPR(t, c.issueID, 4649009, headNew)
		agentSays(t, c, agentID, verdictFor("PASS", headOld))
		fire(t, c, agentID)
		if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, c.issueID); n != 1 {
			t.Fatalf("wakeup on a new head started %d runs, want 1", n)
		}
	})
}

// Scheduled check-ins (at/every/cron) are not review runs: CHE-4615 excludes
// them, so a verdict on the head must not consume their schedule.
func TestScheduledCheckinRunsAfterVerdictOnUnchangedHead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	wakeups := &service.IssueWakeupService{Tasks: testHandler.TaskService}
	inputs := map[string]service.WakeupInput{
		"every": {Kind: "every", Mode: "continuous", IntervalSeconds: 3600, Instruction: "Check in."},
		"at":    {Kind: "at", Mode: "once", AfterSeconds: 3600, Instruction: "Check in."},
		"cron":  {Kind: "cron", Mode: "continuous", CronExpression: "0 * * * *", Timezone: "UTC", Instruction: "Check in."},
	}
	installation := int64(4649020)
	for name, in := range inputs {
		installation++
		t.Run(name, func(t *testing.T) {
			c, agentID := reviewCase(t, installation)
			linkPR(t, c.issueID, int32(installation), headOld)
			agentSays(t, c, agentID, verdictFor("PASS", headOld))
			in.AgentID = agentID
			w, err := wakeups.Create(ctx, parseUUID(c.issueID), parseUUID(testUserID), pgtype.UUID{}, in)
			if err != nil {
				t.Fatal(err)
			}
			dbfx.Cleanup(t, `DELETE FROM issue_wakeup WHERE id = $1`, w.ID)
			dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, c.issueID)
			dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at = now() - interval '1 minute' WHERE id = $1`, w.ID)
			if err := wakeups.TickWorkspaces(ctx, parseUUID(testWorkspaceID)); err != nil {
				t.Fatal(err)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, c.issueID); n != 1 {
				t.Fatalf("%s check-in after a verdict on the same head started %d runs, want 1", name, n)
			}
		})
	}
}

// The skip shows in the run list the UI and `multica issue runs` read.
func TestReviewRunSkipIsInRunList(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, agentID := reviewCase(t, 4649010)
	linkPR(t, c.issueID, 4649010, headOld)
	agentSays(t, c, agentID, verdictFor("PASS", headOld))
	if err := enqueueReview(t, c); !errors.Is(err, service.ErrReviewHeadAnswered) {
		t.Fatalf("same-head review run = %v, want ErrReviewHeadAnswered", err)
	}
	waitSkipped(t, c.issueID, headOld)
	req := withURLParam(newRequest("GET", "/api/issues/"+c.issueID+"/task-runs", nil), "id", c.issueID)
	w := httptest.NewRecorder()
	testHandler.ListTasksByIssue(w, req)
	if w.Code != 200 {
		t.Fatalf("run list = %d: %s", w.Code, w.Body.String())
	}
	var runs []AgentTaskResponse
	if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.FailureReason == string(ReasonReviewHeadAnswered) {
			if run.AgentID != agentID || run.Status != "cancelled" || run.Error == nil || !strings.Contains(*run.Error, headOld[:7]) {
				t.Fatalf("skipped run = %+v, want agent %s, status cancelled, error naming the head", run, agentID)
			}
			return
		}
	}
	t.Fatalf("run list has no skipped run with reason review_head_answered: %s", w.Body.String())
}
